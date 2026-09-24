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

	mutex    sync.Mutex
	active   int       // connections open through the tunnel
	lastUsed time.Time // last dial or connection close
	closed   bool
	now      func() time.Time
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
	return &tunnel{server: server, device: wgDevice, net: tunNet, now: now, lastUsed: now()}, nil
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
	names, err := tunnel.net.LookupContextHost(ctx, host)
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
