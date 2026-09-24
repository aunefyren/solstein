package outbound

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
)

func TestIsPublic(t *testing.T) {
	cases := []struct {
		ip     string
		public bool
	}{
		{"93.184.216.34", true},
		{"1.1.1.1", true},
		{"2606:4700:4700::1111", true},
		{"10.0.0.1", false},
		{"172.16.5.4", false},
		{"192.168.1.1", false},
		{"127.0.0.1", false},
		{"0.0.0.0", false},
		{"169.254.169.254", false},
		{"100.64.0.1", false},
		{"198.18.0.1", false},
		{"192.0.2.1", false},
		{"224.0.0.1", false},
		{"255.255.255.255", false},
		{"::1", false},
		{"::", false},
		{"fe80::1", false},
		{"fd00::1", false},
		{"ff02::1", false},
		{"2001:db8::1", false},
		{"::ffff:10.0.0.1", false},     // IPv4-mapped private
		{"::ffff:93.184.216.34", true}, // IPv4-mapped public
		{"64:ff9b::a00:1", false},      // NAT64 of 10.0.0.1
		{"64:ff9b::5db8:d822", true},   // NAT64 of 93.184.216.34
		{"2002:c0a8:0101::1", false},   // 6to4 of 192.168.1.1
		{"2002:5db8:d822::1", true},    // 6to4 of 93.184.216.34
	}
	for _, c := range cases {
		if got := isPublic(netip.MustParseAddr(c.ip)); got != c.public {
			t.Errorf("isPublic(%s) = %v, want %v", c.ip, got, c.public)
		}
	}
}

func TestMatchesNetwork(t *testing.T) {
	v4, v6 := netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("2606:4700::1")
	if !matchesNetwork("tcp4", v4) || matchesNetwork("tcp4", v6) {
		t.Error("tcp4 filtering wrong")
	}
	if !matchesNetwork("tcp6", v6) || matchesNetwork("tcp6", v4) {
		t.Error("tcp6 filtering wrong")
	}
	if !matchesNetwork("tcp", v4) || !matchesNetwork("tcp", v6) {
		t.Error("tcp should accept both")
	}
}

// failingDialer resolves to public addresses but every connection fails.
type failingDialer struct{ ips []netip.Addr }

func (dialer failingDialer) LookupIP(ctx context.Context, host string) ([]netip.Addr, error) {
	return dialer.ips, nil
}

func (failingDialer) Dial(ctx context.Context, network string, address netip.AddrPort) (net.Conn, error) {
	return nil, errors.New("connection refused")
}

func TestGuardedDialErrors(t *testing.T) {
	ctx := context.Background()
	public := failingDialer{ips: []netip.Addr{netip.MustParseAddr("93.184.216.34")}}

	if _, err := guardedDial(ctx, public, "tcp", "no-port", false); err == nil {
		t.Error("expected an error for an address without a port")
	}
	if _, err := guardedDial(ctx, public, "tcp", "host:99999", false); err == nil {
		t.Error("expected an error for an out-of-range port")
	}
	if _, err := guardedDial(ctx, public, "tcp", "host:80", false); err == nil || errors.Is(err, ErrDestinationBlocked) {
		t.Errorf("connection failure: err = %v, want a connect error, not blocked", err)
	}
	if _, err := guardedDial(ctx, public, "tcp6", "host:80", false); err == nil || errors.Is(err, ErrDestinationBlocked) {
		t.Errorf("no IPv6 address: err = %v, want a no-usable-address error", err)
	}
	unresolvable := &fakeDialer{hosts: map[string][]netip.Addr{}}
	if _, err := guardedDial(ctx, unresolvable, "tcp", "missing.example:80", false); err == nil {
		t.Error("expected a resolve error")
	}
}
