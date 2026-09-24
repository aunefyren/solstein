package outbound

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"time"
)

// guardedDial resolves the host through the exit, drops addresses that aren't
// allowed, and connects to the first allowed one that answers. The check runs
// on the IP actually dialled, after resolution, so a public hostname that
// resolves to an internal address (DNS rebinding, or a redirect to such a
// host) is still refused.
func guardedDial(ctx context.Context, dialer Dialer, network, address string, allowPrivate bool) (net.Conn, error) {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("parse address %q: %w", address, err)
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("parse port in %q: %w", address, err)
	}

	var candidates []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		candidates = []netip.Addr{ip}
	} else {
		candidates, err = dialer.LookupIP(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", host, err)
		}
	}

	var dialErrors []error
	blocked := 0
	for _, ip := range candidates {
		ip = ip.Unmap()
		if !matchesNetwork(network, ip) {
			continue
		}
		if !allowPrivate && !isPublic(ip) {
			blocked++
			continue
		}
		conn, err := dialer.Dial(ctx, network, netip.AddrPortFrom(ip, uint16(port)))
		if err == nil {
			return conn, nil
		}
		dialErrors = append(dialErrors, err)
		if ctx.Err() != nil {
			break
		}
	}

	if len(dialErrors) == 0 && blocked > 0 {
		return nil, fmt.Errorf("%w: %s resolves only to non-public addresses", ErrDestinationBlocked, host)
	}
	if len(dialErrors) == 0 {
		return nil, fmt.Errorf("resolve %s: no usable %s address", host, network)
	}
	return nil, fmt.Errorf("connect to %s: %w", host, errors.Join(dialErrors...))
}

func matchesNetwork(network string, ip netip.Addr) bool {
	switch network {
	case "tcp4":
		return ip.Is4()
	case "tcp6":
		return ip.Is6()
	default:
		return true
	}
}

// nonPublicPrefixes are ranges netip.Addr's own predicates don't cover.
var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),   // carrier-grade NAT, also Tailscale
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, includes broadcast
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("100::/64"),        // discard
}

// Prefixes that embed an IPv4 address in an IPv6 one; the embedded address is
// checked too, so a private IPv4 can't be reached through a translator.
var (
	nat64Prefix = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour   = netip.MustParsePrefix("2002::/16")
)

// isPublic reports whether ip is a globally routable unicast address.
func isPublic(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() {
		return false
	}
	for _, prefix := range nonPublicPrefixes {
		if prefix.Contains(ip) {
			return false
		}
	}
	if nat64Prefix.Contains(ip) {
		bytes := ip.As16()
		return isPublic(netip.AddrFrom4([4]byte{bytes[12], bytes[13], bytes[14], bytes[15]}))
	}
	if sixToFour.Contains(ip) {
		bytes := ip.As16()
		return isPublic(netip.AddrFrom4([4]byte{bytes[2], bytes[3], bytes[4], bytes[5]}))
	}
	return true
}

// directDialer goes out through the host's own network and resolver.
type directDialer struct{}

func (directDialer) LookupIP(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

func (directDialer) Dial(ctx context.Context, network string, address netip.AddrPort) (net.Conn, error) {
	dialer := net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	return dialer.DialContext(ctx, network, address.String())
}
