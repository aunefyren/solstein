package outbound

import (
	"slices"
	"strings"
	"time"
)

// StatusReporter is implemented by providers that can say how their exits
// and tunnels stand right now, for the web UI's exits page. What it returns
// never holds key material: keys are counted and numbered, never shown.
type StatusReporter interface {
	ExitStatus() ([]ProviderStatus, []ExitStatus)
}

// Status is every exit and provider as they stand right now.
type Status struct {
	Providers []ProviderStatus
	Exits     []ExitStatus
}

// ProviderStatus is one VPN provider as it stands right now.
type ProviderStatus struct {
	Name string
	Type string
	// Servers is how many servers the provider has to choose from; for a
	// server-list provider, ServerList says where the list came from.
	Servers    int
	ServerList string
	// Keys is how many private keys the pool hands to tunnels (zero when
	// each server carries its own), MaxTunnels how many tunnels may be open
	// at once (zero for no limit).
	Keys       int
	MaxTunnels int
	Tunnels    []TunnelStatus
	// Benched are servers not used for now after failing.
	Benched []BenchedServer
}

// TunnelStatus is one open tunnel.
type TunnelStatus struct {
	Server  string
	Country string
	// Users are its open connections and operations; IdleFor is how long it
	// has had none (zero while in use). It closes after 5 minutes idle.
	Users   int
	IdleFor time.Duration
	// LastHandshake is when WireGuard last completed a handshake, zero if
	// never.
	LastHandshake time.Time
	// Key is which of the provider's keys the tunnel uses, from 1; zero when
	// the server carries its own.
	Key int
}

// BenchedServer is a server left out after failing, until Until.
type BenchedServer struct {
	Server string
	Until  time.Time
}

// ExitStatus is one exit as it stands right now.
type ExitStatus struct {
	Name string
	// Provider is empty for the direct exit.
	Provider string
	// Locations are the exit's preference list as configured ("NO",
	// "server:US-AZ#108"); Strict means it never falls back to another
	// location.
	Locations []string
	Strict    bool
	Selection string
	// Server is the server its next connection goes to, and Country where
	// that is; both empty before the exit's first use.
	Server  string
	Country string
	// Tunnel is the open tunnel to Server, nil if none is open.
	Tunnel *TunnelStatus
}

// Status gathers the providers' reports, plus the direct exit while it
// exists, sorted by name.
func (manager *Manager) Status() Status {
	var status Status
	for _, provider := range manager.options.Providers {
		reporter, ok := provider.(StatusReporter)
		if !ok {
			continue
		}
		providers, exits := reporter.ExitStatus()
		status.Providers = append(status.Providers, providers...)
		status.Exits = append(status.Exits, exits...)
	}
	if !manager.options.DisableDirect {
		status.Exits = append(status.Exits, ExitStatus{Name: DirectExit, Country: manager.options.HomeCountry})
	}
	slices.SortFunc(status.Providers, func(a, b ProviderStatus) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(status.Exits, func(a, b ExitStatus) int { return strings.Compare(a.Name, b.Name) })
	return status
}
