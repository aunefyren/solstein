package exits

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"aunefyren/solstein/settings"
)

// Setup turns config.json's vpn block into a running-ready module. It
// returns nil when there is nothing usable (no providers, or every exit
// disabled), and warnings for everything it had to leave out, for main.go to
// log. Relative .conf paths are taken from configDir.
func Setup(vpn settings.VPN, configDir string, getenv func(string) string) (*Module, []string) {
	config, problems := Load(vpn, getenv)
	var warnings []string
	for _, problem := range problems {
		warnings = append(warnings, problem.Error())
	}

	servers := map[string][]Server{}
	var (
		list      protonList
		cachedAt  time.Time
		source    string
		listSetup bool
	)
	for _, name := range slices.Sorted(maps.Keys(config.Providers)) {
		provider := config.Providers[name]
		switch provider.Type {
		case TypeWireGuard:
			provider = withBaseDir(provider, configDir)
			loaded, fileProblems := LoadConfs(provider)
			for _, problem := range fileProblems {
				warnings = append(warnings, fmt.Sprintf("provider '%s': %s", name, problem))
			}
			for _, server := range loaded {
				if server.Location.Country == "" {
					warnings = append(warnings, fmt.Sprintf("provider '%s': server %s has no country; only exits naming it (server:%s) or without locations can use it", name, server.Name, server.Name))
				}
			}
			servers[name] = loaded
		case TypeProtonVPN:
			// Proton holds a key's session on one server at a time: a
			// second tunnel on the same key knocks the first one's session
			// out, and they take turns stalling (measured live, see
			// docs/exits.md). So one tunnel per key.
			if provider.MaxTunnels > len(provider.PrivateKeys) {
				warnings = append(warnings, fmt.Sprintf("provider '%s': max_tunnels %d is more than its %d private keys; limited to %d, since a Proton key works on one server at a time. Add a key (generated in Proton's dashboard) for each tunnel wanted at once.",
					name, provider.MaxTunnels, len(provider.PrivateKeys), len(provider.PrivateKeys)))
				provider.MaxTunnels = len(provider.PrivateKeys)
				config.Providers[name] = provider
			}
			if !listSetup {
				list, cachedAt, source = loadProtonList(configDir)
				listSetup = true
			}
			loaded, listWarnings := protonServers(list, provider)
			for _, warning := range listWarnings {
				warnings = append(warnings, fmt.Sprintf("provider '%s': %s", name, warning))
			}
			servers[name] = loaded
		}
	}

	// Exits whose provider went away above, or that can never match a server.
	for _, name := range slices.Sorted(maps.Keys(config.Exits)) {
		exit := config.Exits[name]
		if _, ok := config.Providers[exit.Provider]; !ok {
			warnings = append(warnings, fmt.Sprintf("exit '%s': its provider '%s' is disabled", name, exit.Provider))
			delete(config.Exits, name)
			continue
		}
		matching := 0
		for _, tier := range tiers(exit, servers[exit.Provider]) {
			matching += len(tier)
		}
		if matching == 0 {
			// Kept, not dropped: servers may appear later (a refreshed server
			// list), and the error names what's missing when it's used.
			warnings = append(warnings, fmt.Sprintf("exit '%s': %s", name, strings.TrimPrefix(noServerError(exit, servers[exit.Provider]).Error(), ErrNoServer.Error()+": ")))
		}
	}

	if len(config.Exits) == 0 {
		if len(vpn.Exits) > 0 || len(vpn.Providers) > 0 {
			warnings = append(warnings, "no usable VPN exits; the VPN module is off")
		}
		return nil, warnings
	}
	module := New(config, servers, nil)
	module.configDir, module.protonList, module.protonCachedAt, module.protonSource = configDir, list, cachedAt, source
	return module, warnings
}

// withBaseDir makes relative .conf paths relative to the config directory,
// so config.json can say "wireguard/vps.conf".
func withBaseDir(provider Provider, configDir string) Provider {
	resolve := func(path string) string {
		if path == "" || filepath.IsAbs(path) {
			return path
		}
		return filepath.Join(configDir, path)
	}
	files := make([]string, len(provider.ConfigFiles))
	for i, path := range provider.ConfigFiles {
		files[i] = resolve(path)
	}
	provider.ConfigFiles = files
	provider.ConfigDir = resolve(provider.ConfigDir)
	return provider
}

// Summary describes the module for the start-up log.
func (module *Module) Summary() string {
	var parts []string
	for _, name := range slices.Sorted(maps.Keys(module.config.Providers)) {
		parts = append(parts, fmt.Sprintf("%s (%s, %d servers)", name, module.config.Providers[name].Type, len(module.serversOf(name))))
	}
	summary := fmt.Sprintf("exits %s over providers %s", strings.Join(module.Exits(), ", "), strings.Join(parts, ", "))
	if module.usesProton() {
		summary += fmt.Sprintf("; Proton servers from the %s, dated %s, refreshed daily", module.protonSource, module.protonList.UpdatedAt().UTC().Format(time.DateOnly))
	}
	return summary
}
