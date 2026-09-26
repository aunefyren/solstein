package exits

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"aunefyren/solstein/logger"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

var (
	ErrNoDNS      = errors.New("the tunnel has no DNS server")
	errTunnelShut = errors.New("tunnel closed")
)

// tunnel is one WireGuard connection to one server, with its own userspace
// network stack. Nothing touches the host's network: no interface, no
// routes, no NET_ADMIN. It implements outbound.Dialer.
type tunnel struct {
	server Server
	device *device.Device // nil only in tests of the pool
	net    *netstack.Net
	// fallbackDNS are asked, over TCP through the tunnel, when the server's
	// own DNS fails on a name (the provider's fallback_dns).
	fallbackDNS []netip.Addr

	mutex    sync.Mutex
	active   int       // connections open through the tunnel
	lastUsed time.Time // last dial or connection close
	closed   bool
	now      func() time.Time
	// keyIndex is which of the pool's keys the tunnel uses, -1 for its own.
	keyIndex int
}

// openTunnel brings a tunnel up. It returns once the device is configured;
// the WireGuard handshake happens on the first packet, i.e. the first dial.
func openTunnel(ctx context.Context, server Server, now func() time.Time) (*tunnel, error) {
	endpoint, err := resolveEndpoint(ctx, server.Endpoint)
	if err != nil {
		return nil, err
	}

	tunDevice, tunNet, err := netstack.CreateNetTUN(server.Addresses, server.DNS, server.MTU)
	if err != nil {
		return nil, fmt.Errorf("create network stack: %w", err)
	}
	// WireGuard's own logging goes to debug: its errors are mostly retries
	// that sort themselves out, and it would otherwise print to stdout.
	wgLogger := &device.Logger{
		Verbosef: func(string, ...any) {},
		Errorf: func(format string, args ...any) {
			logger.Log.Debug("WireGuard (" + server.Name + "): " + fmt.Sprintf(format, args...))
		},
	}
	wgDevice := device.NewDevice(tunDevice, conn.NewDefaultBind(), wgLogger)

	if err := wgDevice.IpcSet(deviceConfig(server, endpoint)); err != nil {
		wgDevice.Close()
		// The error can't contain key material: IpcSet reports the failing
		// key name, not its value.
		return nil, fmt.Errorf("configure WireGuard: %w", err)
	}
	if err := wgDevice.Up(); err != nil {
		wgDevice.Close()
		return nil, fmt.Errorf("bring WireGuard up: %w", err)
	}
	return &tunnel{server: server, device: wgDevice, net: tunNet, now: now, lastUsed: now(), keyIndex: -1}, nil
}

// deviceConfig is the WireGuard configuration-protocol text for a server.
func deviceConfig(server Server, endpoint netip.AddrPort) string {
	var config strings.Builder
	fmt.Fprintf(&config, "private_key=%s\n", server.PrivateKey.hex())
	config.WriteString("replace_peers=true\n")
	fmt.Fprintf(&config, "public_key=%s\n", server.PeerPublicKey.hex())
	if server.PresharedKey != nil {
		fmt.Fprintf(&config, "preshared_key=%s\n", server.PresharedKey.hex())
	}
	fmt.Fprintf(&config, "endpoint=%s\n", endpoint)
	if server.Keepalive > 0 {
		fmt.Fprintf(&config, "persistent_keepalive_interval=%d\n", server.Keepalive)
	}
	config.WriteString("replace_allowed_ips=true\n")
	for _, prefix := range server.AllowedIPs {
		fmt.Fprintf(&config, "allowed_ip=%s\n", prefix)
	}
	return config.String()
}

// resolveEndpoint turns the peer's host:port into an address. The VPN
// server's own name is resolved through the host: the tunnel isn't up yet.
func resolveEndpoint(ctx context.Context, endpoint string) (netip.AddrPort, error) {
	host, port, err := splitEndpoint(endpoint)
	if err != nil {
		return netip.AddrPort{}, err
	}
	if address, err := netip.ParseAddr(host); err == nil {
		return netip.AddrPortFrom(address, uint16(port)), nil
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addresses) == 0 {
		return netip.AddrPort{}, fmt.Errorf("resolve VPN endpoint %s: %w", host, err)
	}
	// Prefer IPv4: VPN servers answer on it far more reliably.
	for _, address := range addresses {
		if address.Unmap().Is4() {
			return netip.AddrPortFrom(address.Unmap(), uint16(port)), nil
		}
	}
	return netip.AddrPortFrom(addresses[0], uint16(port)), nil
}

// LookupIP resolves a name through the tunnel's own DNS servers, so the
// lookup leaves from the tunnel's region and nothing leaks to the host's
// resolver.
func (tunnel *tunnel) LookupIP(ctx context.Context, host string) ([]netip.Addr, error) {
	if len(tunnel.server.DNS) == 0 {
		return nil, ErrNoDNS
	}
	if err := tunnel.use(); err != nil {
		return nil, err
	}
	defer tunnel.release()
	names, err := tunnel.lookupWithRetries(ctx, host)
	if err != nil {
		return nil, err
	}
	addresses := make([]netip.Addr, 0, len(names))
	for _, name := range names {
		if address, err := netip.ParseAddr(name); err == nil {
			addresses = append(addresses, address)
		}
	}
	return addresses, nil
}

// dnsAttempts are the time limits of successive lookup attempts at the
// tunnel's DNS server; without fallback resolvers, a last attempt gets
// whatever the caller's context allows. Live tests against Proton showed the
// first DNS packet through a new tunnel sometimes going unanswered, which
// cost the resolver's full 5-second timeout. Retrying after a second makes
// that cost a second. TCP needs nothing like it: it retransmits after about
// a second on its own.
var dnsAttempts = []time.Duration{time.Second, 2 * time.Second}

// fallbackAttempt is the time limit of each fallback resolver. Through
// Proton tunnels in the US, public resolvers took at most 5.4 s for names
// Proton's own couldn't resolve at all, mostly under 1.5 s.
var fallbackAttempt = 5 * time.Second

func (tunnel *tunnel) lookupWithRetries(ctx context.Context, host string) ([]string, error) {
	var names []string
	var err error
	for _, limit := range dnsAttempts {
		attempt, cancel := context.WithTimeout(ctx, limit)
		names, err = tunnel.net.LookupContextHost(attempt, host)
		cancel()
		// Anything but an answer is retried: a timeout, and also a
		// failure such as SERVFAIL, which the next attempt may not get.
		if err == nil || ctx.Err() != nil || isNotFound(err) {
			return names, err
		}
	}
	if len(tunnel.fallbackDNS) == 0 {
		return tunnel.net.LookupContextHost(ctx, host)
	}
	// Seen live: Proton's resolver times out or fails on some names (NRK's
	// CDN host, a CNAME chain over three DNS providers) while public ones
	// reached through the same tunnel answer at once.
	names, fallbackErr := tunnel.lookupFallback(ctx, host)
	if isNotFound(fallbackErr) {
		return nil, fallbackErr
	}
	if fallbackErr != nil {
		return nil, fmt.Errorf("%w; fallback DNS: %w", err, fallbackErr)
	}
	logger.Log.Debug("DNS through " + tunnel.server.Name + ": the tunnel's resolver failed on " + host + " (" + err.Error() + "); a fallback resolver answered.")
	return names, nil
}

// lookupFallback asks the fallback resolvers in turn, over TCP through the
// tunnel, so the lookup still leaves from the tunnel's region. TCP because
// the tunnel's connections don't expose net.PacketConn, so Go's resolver
// would frame UDP queries as TCP. It keeps the addresses the tunnel can
// reach: those of the families it has an address in.
func (tunnel *tunnel) lookupFallback(ctx context.Context, host string) ([]string, error) {
	fqdn := host
	if !strings.HasSuffix(fqdn, ".") {
		fqdn += "." // no search domains from the host's resolv.conf
	}
	var errs error
	for _, server := range tunnel.fallbackDNS {
		resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return tunnel.net.DialContextTCPAddrPort(ctx, netip.AddrPortFrom(server, 53))
		}}
		attempt, cancel := context.WithTimeout(ctx, fallbackAttempt)
		names, err := resolver.LookupHost(attempt, fqdn)
		cancel()
		var dnsError *net.DNSError
		if errors.As(err, &dnsError) {
			// Go's resolver names the host's resolv.conf server, not the
			// one it was dialled to.
			dnsError.Server = netip.AddrPortFrom(server, 53).String()
		}
		if err == nil {
			if reachable := tunnel.reachable(names); len(reachable) > 0 {
				return reachable, nil
			}
			err = fmt.Errorf("no address the tunnel can reach among %s", strings.Join(names, ", "))
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if isNotFound(err) {
			return nil, err
		}
		errs = errors.Join(errs, fmt.Errorf("%s: %w", server, err))
	}
	return nil, errs
}

// reachable keeps the addresses of the families the tunnel has an address
// in.
func (tunnel *tunnel) reachable(names []string) []string {
	var v4, v6 bool
	for _, address := range tunnel.server.Addresses {
		if address.Unmap().Is4() {
			v4 = true
		} else {
			v6 = true
		}
	}
	var kept []string
	for _, name := range names {
		address, err := netip.ParseAddr(name)
		if err == nil && (address.Unmap().Is4() && v4 || !address.Unmap().Is4() && v6) {
			kept = append(kept, name)
		}
	}
	return kept
}

// isNotFound reports whether a lookup got a real "no such host" answer.
func isNotFound(err error) bool {
	var dnsError *net.DNSError
	return errors.As(err, &dnsError) && dnsError.IsNotFound
}

// Dial opens a connection through the tunnel. The tunnel counts as in use
// until the connection is closed, so a long download keeps it open.
func (tunnel *tunnel) Dial(ctx context.Context, network string, address netip.AddrPort) (net.Conn, error) {
	if err := tunnel.use(); err != nil {
		return nil, err
	}
	connection, err := tunnel.net.DialContext(ctx, network, address.String())
	if err != nil {
		tunnel.release()
		return nil, err
	}
	return &trackedConn{Conn: connection, release: tunnel.release}, nil
}

func (tunnel *tunnel) use() error {
	tunnel.mutex.Lock()
	defer tunnel.mutex.Unlock()
	if tunnel.closed {
		return errTunnelShut
	}
	tunnel.active++
	tunnel.lastUsed = tunnel.now()
	return nil
}

func (tunnel *tunnel) release() {
	tunnel.mutex.Lock()
	defer tunnel.mutex.Unlock()
	tunnel.active--
	tunnel.lastUsed = tunnel.now()
}

// activeCount is how many connections are open through the tunnel.
func (tunnel *tunnel) activeCount() int {
	tunnel.mutex.Lock()
	defer tunnel.mutex.Unlock()
	return tunnel.active
}

// idleFor reports how long the tunnel has had no open connections, or zero
// while any are open.
func (tunnel *tunnel) idleFor() time.Duration {
	tunnel.mutex.Lock()
	defer tunnel.mutex.Unlock()
	if tunnel.active > 0 {
		return 0
	}
	return tunnel.now().Sub(tunnel.lastUsed)
}

// handshakeFresh reports whether the tunnel handshook recently enough to be
// working: WireGuard re-handshakes every two minutes while traffic flows.
func (tunnel *tunnel) handshakeFresh() bool {
	handshake := tunnel.lastHandshake()
	return !handshake.IsZero() && time.Since(handshake) < staleHandshake
}

// lastHandshake is when the peer last completed a handshake, zero if never.
func (tunnel *tunnel) lastHandshake() time.Time {
	if tunnel.device == nil {
		return time.Time{}
	}
	state, err := tunnel.device.IpcGet()
	if err != nil {
		return time.Time{}
	}
	var seconds, nanoseconds int64
	for _, line := range strings.Split(state, "\n") {
		key, value, _ := strings.Cut(line, "=")
		switch key {
		case "last_handshake_time_sec":
			seconds, _ = strconv.ParseInt(value, 10, 64)
		case "last_handshake_time_nsec":
			nanoseconds, _ = strconv.ParseInt(value, 10, 64)
		}
	}
	if seconds == 0 {
		return time.Time{}
	}
	return time.Unix(seconds, nanoseconds)
}

// handshakeTimeout is how long a tunnel gets to complete a handshake before
// its server counts as failing. A working server answers in well under a
// second. WireGuard resends a lost handshake after five, so six lets one
// lost packet be made up for rather than bench a working server.
var handshakeTimeout = 6 * time.Second

// pokeAddress receives the packet that makes WireGuard start a handshake:
// TEST-NET-1, the discard port. Nothing there answers, and nothing needs to.
var pokeAddress = netip.AddrPortFrom(netip.MustParseAddr("192.0.2.1"), 9)

// awaitHandshake makes sure the tunnel has a live handshake, starting one if
// needed. Without it, a dead server is only noticed when a lookup or dial
// through it times out, which takes far longer.
//
// Handshake times come from WireGuard on the real clock, so they are compared
// with the real clock too.
func (tunnel *tunnel) awaitHandshake(ctx context.Context) error {
	if tunnel.handshakeFresh() {
		return nil
	}
	if tunnel.net == nil {
		return errors.New("no network stack")
	}
	// WireGuard starts a handshake when it has a packet to send.
	if poke, err := tunnel.net.DialUDPAddrPort(netip.AddrPort{}, pokeAddress); err == nil {
		poke.Write([]byte{0})
		poke.Close()
	}
	deadline := time.NewTimer(handshakeTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("no WireGuard handshake within %s; check the keys and that the endpoint is reachable", handshakeTimeout)
		case <-ticker.C:
			if tunnel.handshakeFresh() {
				return nil
			}
		}
	}
}

func (tunnel *tunnel) close() {
	tunnel.mutex.Lock()
	if tunnel.closed {
		tunnel.mutex.Unlock()
		return
	}
	tunnel.closed = true
	tunnel.mutex.Unlock()
	if tunnel.device != nil {
		tunnel.device.Close()
	}
}

// trackedConn tells its tunnel when it is closed.
type trackedConn struct {
	net.Conn
	release func()
	once    sync.Once
}

func (connection *trackedConn) Close() error {
	err := connection.Conn.Close()
	connection.once.Do(connection.release)
	return err
}
