package exits

import (
	"maps"
	"slices"
	"strings"
	"time"

	"aunefyren/solstein/outbound"
)

// ExitStatus reports the providers, their open tunnels and benched servers,
// and each exit's current server, for outbound.StatusReporter. Keys are
// counted and numbered, never included: in particular the WireGuard device
// state lastHandshake reads holds the private key, and only the handshake
// time is taken from it.
func (module *Module) ExitStatus() ([]outbound.ProviderStatus, []outbound.ExitStatus) {
	now := module.now()

	// What the module itself knows, copied under its lock; the pools are
	// read after, under their own, so the two locks are never held at once.
	module.mutex.Lock()
	current := map[string]string{}
	for name, state := range module.states {
		current[name] = state.current
	}
	servers := map[string][]Server{}
	for name, list := range module.servers {
		servers[name] = list
	}
	benched := map[string][]outbound.BenchedServer{}
	for key, h := range module.health {
		if !h.benched(now) {
			continue
		}
		provider, server, _ := strings.Cut(key, "/")
		benched[provider] = append(benched[provider], outbound.BenchedServer{Server: server, Until: h.benchedUntil})
	}
	serverList := ""
	if module.usesProton() {
		serverList = module.protonSource + ", dated " + module.protonList.UpdatedAt().UTC().Format(time.DateOnly)
	}
	module.mutex.Unlock()

	countryOf := func(provider, server string) string {
		for _, candidate := range servers[provider] {
			if candidate.Name == server {
				return candidate.Location.Country
			}
		}
		return ""
	}

	tunnels := map[string]map[string]outbound.TunnelStatus{} // provider → server → tunnel
	var providers []outbound.ProviderStatus
	for _, name := range slices.Sorted(maps.Keys(module.config.Providers)) {
		config := module.config.Providers[name]
		status := outbound.ProviderStatus{
			Name:       name,
			Type:       config.Type,
			Servers:    len(servers[name]),
			Keys:       len(config.PrivateKeys),
			MaxTunnels: module.pools[name].max,
			Benched:    benched[name],
		}
		if config.Type == TypeProtonVPN {
			status.ServerList = serverList
		}
		tunnels[name] = map[string]outbound.TunnelStatus{}
		for _, tunnel := range module.pools[name].snapshot() {
			report := outbound.TunnelStatus{
				Server:        tunnel.server.Name,
				Country:       tunnel.server.Location.Country,
				Users:         tunnel.activeCount(),
				IdleFor:       tunnel.idleFor(),
				LastHandshake: tunnel.lastHandshake(),
				Key:           tunnel.keyIndex + 1,
			}
			tunnels[name][report.Server] = report
			status.Tunnels = append(status.Tunnels, report)
		}
		slices.SortFunc(status.Tunnels, func(a, b outbound.TunnelStatus) int { return strings.Compare(a.Server, b.Server) })
		slices.SortFunc(status.Benched, func(a, b outbound.BenchedServer) int { return strings.Compare(a.Server, b.Server) })
		providers = append(providers, status)
	}

	var exits []outbound.ExitStatus
	for _, name := range module.Exits() {
		exit := module.config.Exits[name]
		status := outbound.ExitStatus{
			Name:      name,
			Provider:  exit.Provider,
			Strict:    exit.Strict,
			Selection: exit.Selection,
			Server:    current[name],
			Country:   countryOf(exit.Provider, current[name]),
		}
		for _, location := range exit.Locations {
			status.Locations = append(status.Locations, location.String())
		}
		if tunnel, open := tunnels[exit.Provider][status.Server]; open && status.Server != "" {
			status.Tunnel = &tunnel
		}
		exits = append(exits, status)
	}
	return providers, exits
}

// snapshot lists the pool's open tunnels.
func (pool *pool) snapshot() []*tunnel {
	pool.mutex.Lock()
	defer pool.mutex.Unlock()
	return slices.Collect(maps.Values(pool.tunnels))
}
