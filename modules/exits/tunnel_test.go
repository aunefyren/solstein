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
	"strings"
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
func startWireGuardServerWith(t *testing.T, clientPublic PublicKey, handler http.Handler) wireGuardServer {
	t.Helper()
	key := generateKey(t)
	tunDevice, tunNet, err := netstack.CreateNetTUN([]netip.Addr{serverTunnelAddress}, nil, defaultMTU)
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
	go serveDNS(dnsConn, map[string]netip.Addr{"example.test.": serverTunnelAddress})

	t.Cleanup(func() {
		listener.Close()
		dnsConn.Close()
		wgDevice.Close()
	})
	return wireGuardServer{endpoint: fmt.Sprintf("127.0.0.1:%d", port), publicKey: key.Public()}
}

// serveDNS answers A queries for the given names and NXDOMAIN otherwise.
func serveDNS(packetConn net.PacketConn, records map[string]netip.Addr) {
	buffer := make([]byte, 1500)
	for {
		size, from, err := packetConn.ReadFrom(buffer)
		if err != nil {
			return
		}
		var parser dnsmessage.Parser
		header, err := parser.Start(buffer[:size])
		if err != nil {
			continue
		}
		question, err := parser.Question()
		if err != nil {
			continue
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
		if err == nil {
			packetConn.WriteTo(packed, from)
		}
	}
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
