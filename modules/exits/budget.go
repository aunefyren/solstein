package exits

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// ExitUse is one reason an exit can be in use at a given moment: region
// diff's pair, a fallback exit, the default exit, a feed's own exit. Each
// exit in use needs a tunnel of its own, since an exit is one server at a
// time; several episodes through the same exit share that tunnel.
type ExitUse struct {
	Exit   string
	Reason string
}

// TunnelBudget compares the exits that can be in use at the same moment with
// how many tunnels each of their providers can hold, and returns a warning
// for every provider that can't hold them all. Short of tunnels, the pool
// closes and reopens tunnels to make room — which moves a key between
// servers, and a moved key can leave the new tunnel stalling for minutes —
// and refuses (ErrTunnelLimit) once every tunnel is busy.
//
// The count is the worst case: it assumes every use can fall together, which
// a burst of episodes makes likely (measured live, docs/exits.md). Exits this
// module doesn't have, and "direct", are left out: neither takes a tunnel.
func (module *Module) TunnelBudget(uses []ExitUse) []string {
	// Reasons per exit, then exits per provider, both in a stable order.
	reasons := map[string][]string{}
	var order []string
	for _, use := range uses {
		exit, ok := module.config.Exits[use.Exit]
		if !ok || module.pools[exit.Provider] == nil {
			continue
		}
		if _, seen := reasons[use.Exit]; !seen {
			order = append(order, use.Exit)
		}
		if use.Reason != "" && !slices.Contains(reasons[use.Exit], use.Reason) {
			reasons[use.Exit] = append(reasons[use.Exit], use.Reason)
		}
	}
	byProvider := map[string][]string{}
	for _, name := range order {
		provider := module.config.Exits[name].Provider
		byProvider[provider] = append(byProvider[provider], name)
	}

	var warnings []string
	for _, name := range slices.Sorted(maps.Keys(byProvider)) {
		wanted := byProvider[name]
		limit := module.pools[name].max
		if limit <= 0 || len(wanted) <= limit {
			continue // no limit, or room for every exit
		}
		described := make([]string, 0, len(wanted))
		for _, exit := range slices.Sorted(slices.Values(wanted)) {
			if reason := reasons[exit]; len(reason) > 0 {
				described = append(described, exit+" ("+strings.Join(reason, ", ")+")")
				continue
			}
			described = append(described, exit)
		}
		warnings = append(warnings, fmt.Sprintf("provider '%s' can hold %s, but %d of its exits can be in use at the same moment: %s. Tunnels are then closed and reopened to make room, which moves keys between servers and can stall a tunnel for a few minutes, and downloads fail with 'tunnel limit reached' while every tunnel is busy. %s",
			name, module.capacityPhrase(name, limit), len(wanted), strings.Join(described, ", "), module.roomAdvice(name, len(wanted)-limit)))
	}
	return warnings
}

// capacityPhrase says what a provider's tunnel limit comes from, since for
// Proton it is the number of keys and elsewhere max_tunnels.
func (module *Module) capacityPhrase(provider string, limit int) string {
	tunnels := "tunnels"
	if limit == 1 {
		tunnels = "tunnel"
	}
	if module.config.Providers[provider].Type == TypeProtonVPN {
		return fmt.Sprintf("%d %s at once (one per private key)", limit, tunnels)
	}
	return fmt.Sprintf("%d %s at once (max_tunnels)", limit, tunnels)
}

// roomAdvice says how to make room for the exits that don't fit.
func (module *Module) roomAdvice(provider string, short int) string {
	if module.config.Providers[provider].Type == TypeProtonVPN {
		keys := "keys"
		if short == 1 {
			keys = "key"
		}
		return fmt.Sprintf("Add %d more private %s (generated in Proton's dashboard) so each exit has its own tunnel, or use fewer exits at once (region_diff's exits and fallback_exits, default_exit, and feeds' own exits).", short, keys)
	}
	return fmt.Sprintf("Raise max_tunnels by %d so each exit has its own tunnel, or use fewer exits at once (region_diff's exits and fallback_exits, default_exit, and feeds' own exits).", short)
}

// ExitsFit reports whether every one of these exits can have a tunnel at the
// same time, for outbound.Budgeter. Exits this module doesn't have are left
// out; a provider without a limit always fits.
func (module *Module) ExitsFit(exitNames []string) (bool, string) {
	wanted := map[string][]string{}
	for _, name := range exitNames {
		exit, ok := module.config.Exits[name]
		if !ok || module.pools[exit.Provider] == nil {
			continue
		}
		if !slices.Contains(wanted[exit.Provider], name) {
			wanted[exit.Provider] = append(wanted[exit.Provider], name)
		}
	}
	for _, provider := range slices.Sorted(maps.Keys(wanted)) {
		limit := module.pools[provider].max
		exits := slices.Sorted(slices.Values(wanted[provider]))
		if limit <= 0 || len(exits) <= limit {
			continue
		}
		return false, fmt.Sprintf("provider '%s' can hold %s, and %s need %d",
			provider, module.capacityPhrase(provider, limit), strings.Join(exits, ", "), len(exits))
	}
	return true, ""
}
