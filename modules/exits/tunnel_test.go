package exits

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"aunefyren/solstein/logger"
	"aunefyren/solstein/outbound"

	"github.com/sirupsen/logrus"
	"golang.org/x/net/dns/dnsmessage"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

var (
	serverTunnelAddress = netip.MustParseAddr("10.99.0.1")
	clientTunnelAddress = netip.MustParseAddr("10.99.0.2")
	// fallbackResolverAddress is a DNS server over TCP inside the test peer,
	// standing in for a public resolver reached through the tunnel.
	fallbackResolverAddress = netip.MustParseAddr("10.99.0.53")
)

func quietLogs(t *testing.T) {
	t.Helper()
	original := logger.Log
	t.Cleanup(func() { logger.Log = original })
	logger.Log = logrus.New()
	logger.Log.SetOutput(io.Discard)
}

func generateKey(t *testing.T) Key {
	t.Helper()
	var key Key
	if _, err := rand.Read(key.bytes[:]); err != nil {
		t.Fatal(err)
	}
	// Curve25519 clamping, as wg genkey does.
	key.bytes[0] &= 248
	key.bytes[31] = key.bytes[31]&127 | 64
	return key
}

func freeUDPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.LocalAddr().(*net.UDPAddr).Port
}

// wireGuardServer is a WireGuard peer inside the test process, with a web
// server and a DNS server that exist only inside its tunnel.
type wireGuardServer struct {
	endpoint  string
	publicKey PublicKey
}

func startWireGuardServer(t *testing.T, clientPublic PublicKey) wireGuardServer {
	t.Helper()
	return startWireGuardServerWith(t, clientPublic, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fmt.Fprintf(writer, "hello from inside the tunnel, %s", strings.Split(request.RemoteAddr, ":")[0])
	}))
}

// startWireGuardServerWith is startWireGuardServer with its own web handler.
// dnsDrop makes the next test WireGuard server's DNS ignore that many
// queries first. Tests that set it reset it.
var dnsDrop int

func startWireGuardServerWith(t *testing.T, clientPublic PublicKey, handler http.Handler) wireGuardServer {
	t.Helper()
	key := generateKey(t)
	tunDevice, tunNet, err := netstack.CreateNetTUN([]netip.Addr{serverTunnelAddress, fallbackResolverAddress}, nil, defaultMTU)
	if err != nil {
		t.Fatal(err)
	}
	wgDevice := device.NewDevice(tunDevice, conn.NewDefaultBind(), device.NewLogger(device.LogLevelSilent, ""))
	port := freeUDPPort(t)
	config := fmt.Sprintf("private_key=%s\nlisten_port=%d\npublic_key=%s\nallowed_ip=%s/32\n", key.hex(), port, clientPublic.hex(), clientTunnelAddress)
	if err := wgDevice.IpcSet(config); err != nil {
		t.Fatal(err)
	}
	if err := wgDevice.Up(); err != nil {
		t.Fatal(err)
	}

	listener, err := tunNet.ListenTCP(&net.TCPAddr{IP: serverTunnelAddress.AsSlice(), Port: 80})
	if err != nil {
		t.Fatal(err)
	}
	go http.Serve(listener, handler)

	dnsConn, err := tunNet.ListenUDP(&net.UDPAddr{IP: serverTunnelAddress.AsSlice(), Port: 53})
	if err != nil {
		t.Fatal(err)
	}
	go serveDNSDropping(dnsConn, map[string]netip.Addr{"example.test.": serverTunnelAddress}, dnsDrop)

	fallbackListener, err := tunNet.ListenTCP(&net.TCPAddr{IP: fallbackResolverAddress.AsSlice(), Port: 53})
	if err != nil {
		t.Fatal(err)
	}
	go serveDNSOverTCP(fallbackListener, map[string]netip.Addr{"example.test.": serverTunnelAddress, "fallback.test.": serverTunnelAddress})

	t.Cleanup(func() {
		listener.Close()
		dnsConn.Close()
		fallbackListener.Close()
		wgDevice.Close()
	})
	return wireGuardServer{endpoint: fmt.Sprintf("127.0.0.1:%d", port), publicKey: key.Public()}
}

// serveDNS answers A queries for the given names and NXDOMAIN otherwise.
func serveDNS(packetConn net.PacketConn, records map[string]netip.Addr) {
	serveDNSDropping(packetConn, records, 0)
}

// serveDNSDropping is serveDNS that ignores the first drop queries, as a
// VPN's DNS sometimes does right after a tunnel comes up.
func serveDNSDropping(packetConn net.PacketConn, records map[string]netip.Addr, drop int) {
	buffer := make([]byte, 1500)
	for {
		size, from, err := packetConn.ReadFrom(buffer)
		if err != nil {
			return
		}
		if drop > 0 {
			drop--
			continue
		}
		if response, ok := dnsAnswer(buffer[:size], records); ok {
			packetConn.WriteTo(response, from)
		}
	}
}

// serveDNSOverTCP answers as serveDNS does, over TCP: each message is
// preceded by its length in two bytes.
func serveDNSOverTCP(listener net.Listener, records map[string]netip.Addr) {
	for {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer connection.Close()
			for {
				var length [2]byte
				if _, err := io.ReadFull(connection, length[:]); err != nil {
					return
				}
				query := make([]byte, int(length[0])<<8|int(length[1]))
				if _, err := io.ReadFull(connection, query); err != nil {
					return
				}
				response, ok := dnsAnswer(query, records)
				if !ok {
					return
				}
				connection.Write(append([]byte{byte(len(response) >> 8), byte(len(response))}, response...))
			}
		}()
	}
}

// dnsAnswer answers a query for an A record of the given names, with
// NXDOMAIN for other names and an empty answer for other types.
func dnsAnswer(query []byte, records map[string]netip.Addr) ([]byte, bool) {
	var parser dnsmessage.Parser
	header, err := parser.Start(query)
	if err != nil {
		return nil, false
	}
	question, err := parser.Question()
	if err != nil {
		return nil, false
	}
	response := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: header.ID, Response: true, Authoritative: true, RCode: dnsmessage.RCodeSuccess},
		Questions: []dnsmessage.Question{question},
	}
	address, known := records[strings.ToLower(question.Name.String())]
	switch {
	case !known:
		response.Header.RCode = dnsmessage.RCodeNameError
	case question.Type == dnsmessage.TypeA:
		response.Answers = []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{Name: question.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60},
			Body:   &dnsmessage.AResource{A: address.As4()},
		}}
	}
	packed, err := response.Pack()
	return packed, err == nil
}

// clientServer is the Server a .conf file would describe for the test peer.
func clientServer(t *testing.T, clientKey Key, peer wireGuardServer, dns bool) Server {
	t.Helper()
	server := Server{
		Name:          "test-server",
		PrivateKey:    clientKey,
		Addresses:     []netip.Addr{clientTunnelAddress},
		MTU:           defaultMTU,
		PeerPublicKey: peer.publicKey,
		Endpoint:      peer.endpoint,
		AllowedIPs:    []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")},
	}
	if dns {
		server.DNS = []netip.Addr{serverTunnelAddress}
	}
	return server
}

func openTestTunnel(t *testing.T, dns bool) *tunnel {
	t.Helper()
	quietLogs(t)
	clientKey := generateKey(t)
	peer := startWireGuardServer(t, clientKey.Public())
	opened, err := openTunnel(context.Background(), clientServer(t, clientKey, peer, dns), time.Now)
	if err != nil {
		t.Fatalf("openTunnel: %v", err)
	}
	t.Cleanup(opened.close)
	return opened
}

func TestTunnelEndToEnd(t *testing.T) {
	opened := openTestTunnel(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if !opened.lastHandshake().IsZero() {
		t.Error("handshake before any traffic")
	}

	addresses, err := opened.LookupIP(ctx, "example.test")
	if err != nil || len(addresses) != 1 || addresses[0] != serverTunnelAddress {
		t.Fatalf("LookupIP through the tunnel = %v, %v", addresses, err)
	}
	if _, err := opened.LookupIP(ctx, "unknown.test"); err == nil {
		t.Error("unknown name resolved")
	}

	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		return opened.Dial(ctx, network, netip.MustParseAddrPort(address))
	}}
	response, err := (&http.Client{Transport: transport}).Get("http://" + serverTunnelAddress.String() + "/")
	if err != nil {
		t.Fatalf("HTTP through the tunnel: %v", err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != "hello from inside the tunnel, "+clientTunnelAddress.String() {
		t.Errorf("body = %q", body)
	}

	if opened.lastHandshake().IsZero() {
		t.Error("no handshake recorded after traffic")
	}
	if opened.idleFor() != 0 {
		t.Error("tunnel counted as idle while a keep-alive connection is open")
	}
	transport.CloseIdleConnections()
	if opened.activeCount() != 0 {
		t.Errorf("active connections = %d after closing, want 0", opened.activeCount())
	}
}

// tunnelProvider offers one tunnel as the exit "test", as the module will.
type tunnelProvider struct{ tunnel *tunnel }

func (provider tunnelProvider) Exits() []string { return []string{"test"} }
func (provider tunnelProvider) Dialer(string) (outbound.Dialer, error) {
	return provider.tunnel, nil
}

func TestTunnelThroughOutboundManager(t *testing.T) {
	opened := openTestTunnel(t, true)

	for _, allowPrivate := range []bool{true, false} {
		manager, err := outbound.New(outbound.Options{AllowPrivateDestinations: allowPrivate, Providers: []outbound.Provider{tunnelProvider{opened}}})
		if err != nil {
			t.Fatal(err)
		}
		client, _ := manager.Client("test")
		response, err := client.Get("http://example.test/")
		if allowPrivate {
			if err != nil {
				t.Fatalf("through the manager: %v", err)
			}
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if !strings.HasPrefix(string(body), "hello from inside the tunnel") {
				t.Errorf("body = %q", body)
			}
			continue
		}
		// The test server's address is private: the guard applies inside
		// tunnels as well.
		if !errors.Is(err, outbound.ErrDestinationBlocked) {
			t.Errorf("private address through a tunnel: err = %v, want ErrDestinationBlocked", err)
		}
	}
}

func TestTunnelWithoutDNS(t *testing.T) {
	opened := openTestTunnel(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := opened.LookupIP(ctx, "example.test"); !errors.Is(err, ErrNoDNS) {
		t.Errorf("LookupIP without DNS: err = %v, want ErrNoDNS", err)
	}
	// Addresses still work.
	connection, err := opened.Dial(ctx, "tcp", netip.AddrPortFrom(serverTunnelAddress, 80))
	if err != nil {
		t.Fatalf("Dial by address: %v", err)
	}
	connection.Close()
}

func TestTunnelWrongPeerKey(t *testing.T) {
	quietLogs(t)
	clientKey := generateKey(t)
	peer := startWireGuardServer(t, clientKey.Public())
	server := clientServer(t, clientKey, peer, false)
	server.PeerPublicKey = generateKey(t).Public() // not the server's key

	opened, err := openTunnel(context.Background(), server, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := opened.Dial(ctx, "tcp", netip.AddrPortFrom(serverTunnelAddress, 80)); err == nil {
		t.Error("dial succeeded with the wrong peer key")
	}
	if !opened.lastHandshake().IsZero() {
		t.Error("handshake recorded with the wrong peer key")
	}
	if opened.activeCount() != 0 {
		t.Errorf("failed dial left active = %d", opened.activeCount())
	}
}

func TestClosedTunnelRefusesUse(t *testing.T) {
	opened := openTestTunnel(t, true)
	opened.close()
	opened.close() // twice is fine
	if _, err := opened.Dial(context.Background(), "tcp", netip.AddrPortFrom(serverTunnelAddress, 80)); !errors.Is(err, errTunnelShut) {
		t.Errorf("dial on a closed tunnel: err = %v", err)
	}
	if _, err := opened.LookupIP(context.Background(), "example.test"); !errors.Is(err, errTunnelShut) {
		t.Errorf("lookup on a closed tunnel: err = %v", err)
	}
	// Without this check the poke packet went into a closed network stack,
	// which panics the process instead of failing (see tunnel.close).
	if err := opened.awaitHandshake(context.Background()); !errors.Is(err, errTunnelShut) {
		t.Errorf("handshake on a closed tunnel: err = %v", err)
	}
}

// A handshake and a close at the same moment: the close must wait for the
// handshake's packet, or writing it kills the process. This is what happened
// live, when the pool closed a tunnel it had just handed out to make room
// (docs/exits.md). Run under -race.
func TestHandshakeRacingClose(t *testing.T) {
	for range 20 {
		opened := openTestTunnel(t, false)
		start := make(chan struct{})
		var waiting sync.WaitGroup
		waiting.Add(2)
		go func() {
			defer waiting.Done()
			<-start
			// The handshake either goes through or is refused because the
			// tunnel is shut. Panicking is not an outcome, and neither is
			// waiting out the handshake timeout on a closed network stack.
			if err := opened.awaitHandshake(context.Background()); err != nil && !errors.Is(err, errTunnelShut) {
				t.Errorf("handshake racing close: err = %v", err)
			}
		}()
		go func() {
			defer waiting.Done()
			<-start
			opened.close()
		}()
		close(start)
		waiting.Wait()
		if closed, deviceClosed := opened.shut(); !closed || !deviceClosed {
			t.Fatalf("after the race: closed = %v, device closed = %v", closed, deviceClosed)
		}
	}
}

func TestResolveEndpoint(t *testing.T) {
	ctx := context.Background()
	for endpoint, want := range map[string]string{
		"198.51.100.7:51820":  "198.51.100.7:51820",
		"[2001:db8::1]:51820": "[2001:db8::1]:51820",
		"localhost:51820":     "127.0.0.1:51820",
	} {
		got, err := resolveEndpoint(ctx, endpoint)
		if err != nil || got.String() != want {
			t.Errorf("resolveEndpoint(%q) = %v, %v; want %s", endpoint, got, err, want)
		}
	}
	if _, err := resolveEndpoint(ctx, "no-such-host.invalid:51820"); err == nil {
		t.Error("unresolvable endpoint accepted")
	}
}

func TestDeviceConfig(t *testing.T) {
	key, peer := generateKey(t), generateKey(t).Public()
	preshared := generateKey(t)
	config := deviceConfig(Server{PrivateKey: key, PeerPublicKey: peer, PresharedKey: &preshared, Keepalive: 25,
		AllowedIPs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}}, netip.MustParseAddrPort("198.51.100.7:51820"))
	for _, want := range []string{"private_key=" + key.hex(), "public_key=" + peer.hex(), "preshared_key=" + preshared.hex(),
		"endpoint=198.51.100.7:51820", "persistent_keepalive_interval=25", "allowed_ip=0.0.0.0/0"} {
		if !strings.Contains(config, want+"\n") {
			t.Errorf("device config lacks %s", want)
		}
	}
}

func TestLookupRetriesALostFirstQuery(t *testing.T) {
	// One lost packet, as seen live with Proton. (Netstack's resolver asks
	// for A and AAAA one after the other, so each dropped query costs one
	// attempt.)
	dnsDrop = 1
	t.Cleanup(func() { dnsDrop = 0 })
	opened := openTestTunnel(t, true)
	dnsDrop = 0

	start := time.Now()
	addresses, err := opened.LookupIP(context.Background(), "example.test")
	elapsed := time.Since(start)
	if err != nil || len(addresses) != 1 {
		t.Fatalf("LookupIP = %v, %v", addresses, err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("lookup took %s; a lost first query should cost about a second, not the resolver's 5", elapsed)
	}

	// A real "no such host" is not retried.
	start = time.Now()
	if _, err := opened.LookupIP(context.Background(), "unknown.test"); err == nil {
		t.Error("unknown name resolved")
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Errorf("NXDOMAIN took %s; it shouldn't be retried", time.Since(start))
	}
}

// TestLookupFallsBackWhenTunnelDNSFails covers the tunnel's resolver not
// answering at all, as Proton's didn't for NRK's CDN host: the fallback
// resolver, over TCP through the tunnel, answers instead.
func TestLookupFallsBackWhenTunnelDNSFails(t *testing.T) {
	dnsDrop = 1 << 30 // the tunnel's resolver never answers
	t.Cleanup(func() { dnsDrop = 0 })
	opened := openTestTunnel(t, true)
	dnsDrop = 0
	opened.fallbackDNS = []netip.Addr{netip.MustParseAddr("10.99.0.54"), fallbackResolverAddress} // the first isn't there
	originalAttempts, originalFallback := dnsAttempts, fallbackAttempt
	dnsAttempts, fallbackAttempt = []time.Duration{100 * time.Millisecond, 200 * time.Millisecond}, 500*time.Millisecond
	t.Cleanup(func() { dnsAttempts, fallbackAttempt = originalAttempts, originalFallback })

	start := time.Now()
	addresses, err := opened.LookupIP(context.Background(), "fallback.test")
	if err != nil || len(addresses) != 1 || addresses[0] != serverTunnelAddress {
		t.Fatalf("LookupIP = %v, %v", addresses, err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("lookup took %s; the tunnel's resolver should get its attempts, then each fallback its own limit", elapsed)
	}

	// The fallback's "no such host" is an answer.
	if _, err := opened.LookupIP(context.Background(), "unknown.test"); !isNotFound(err) {
		t.Errorf("unknown name: err = %v, want not found", err)
	}

	// Every resolver failing names them all.
	opened.fallbackDNS = []netip.Addr{netip.MustParseAddr("10.99.0.54")}
	if _, err := opened.LookupIP(context.Background(), "fallback.test"); err == nil || !strings.Contains(err.Error(), "fallback DNS: 10.99.0.54") {
		t.Errorf("every resolver failing: err = %v", err)
	}

	// A cancelled lookup stops.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := opened.LookupIP(ctx, "fallback.test"); err == nil {
		t.Error("cancelled lookup succeeded")
	}
}

// TestLookupDoesNotFallBackOnAnswers checks that the fallback is only for a
// resolver that fails: an answer, including "no such host", stands.
func TestLookupDoesNotFallBackOnAnswers(t *testing.T) {
	opened := openTestTunnel(t, true)
	opened.fallbackDNS = []netip.Addr{fallbackResolverAddress}
	// fallback.test is only known to the fallback resolver.
	if _, err := opened.LookupIP(context.Background(), "fallback.test"); !isNotFound(err) {
		t.Errorf("err = %v; the tunnel resolver's NXDOMAIN should stand", err)
	}
}

func TestReachableKeepsTheTunnelsFamilies(t *testing.T) {
	names := []string{"192.0.2.10", "2001:db8::10", "not an address"}
	v4 := &tunnel{server: Server{Addresses: []netip.Addr{netip.MustParseAddr("10.2.0.2")}}}
	if kept := v4.reachable(names); !slices.Equal(kept, []string{"192.0.2.10"}) {
		t.Errorf("IPv4 tunnel kept %v", kept)
	}
	both := &tunnel{server: Server{Addresses: []netip.Addr{netip.MustParseAddr("10.2.0.2"), netip.MustParseAddr("fd00::2")}}}
	if kept := both.reachable(names); !slices.Equal(kept, []string{"192.0.2.10", "2001:db8::10"}) {
		t.Errorf("dual-stack tunnel kept %v", kept)
	}
}
