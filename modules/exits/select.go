package exits

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"time"
)

const (
	// benchBase is how long a failing server is left out; it doubles with
	// each failure in a row, up to benchMax.
	benchBase = time.Minute
	benchMax  = 30 * time.Minute
	// failureMemory is how long a failure counts for least-failed selection.
	failureMemory = time.Hour
	// rotation is how often random selection picks a new server anyway.
	rotation = time.Hour
	// staleHandshake: WireGuard re-handshakes every two minutes while traffic
	// flows, so a handshake older than this means the tunnel isn't working.
	staleHandshake = 3 * time.Minute
)

// ErrNoServer means no server of the exit's provider matches its locations
// and is healthy right now.
var ErrNoServer = errors.New("no healthy server matches the exit")

// health is what is known about one server.
type health struct {
	consecutive  int
	benchedUntil time.Time
	failures     []time.Time // within failureMemory, for least-failed
	lastError    string
}

func (h *health) benched(now time.Time) bool {
	return h != nil && now.Before(h.benchedUntil)
}

func (h *health) recentFailures(now time.Time) int {
	if h == nil {
		return 0
	}
	count := 0
	for _, at := range h.failures {
		if now.Sub(at) < failureMemory {
			count++
		}
	}
	return count
}

// recordFailure benches the server, longer for each failure in a row.
func (h *health) recordFailure(now time.Time, reason error) time.Duration {
	h.consecutive++
	bench := benchBase << min(h.consecutive-1, 10)
	if bench > benchMax {
		bench = benchMax
	}
	h.benchedUntil = now.Add(bench)
	h.lastError = reason.Error()
	h.failures = append(slices.DeleteFunc(h.failures, func(at time.Time) bool { return now.Sub(at) >= failureMemory }), now)
	return bench
}

func (h *health) recordSuccess() {
	h.consecutive = 0
	h.benchedUntil = time.Time{}
}

// matches reports whether a server satisfies one location entry.
func matches(server Server, location Location) bool {
	code := server.Location.Country
	switch location.Kind {
	case LocationServer:
		return strings.EqualFold(server.Name, location.Name) ||
			(server.ServerName != "" && strings.EqualFold(server.ServerName, location.Name)) ||
			(server.Hostname != "" && strings.EqualFold(server.Hostname, location.Name))
	case LocationCity:
		return code == location.Country && strings.EqualFold(server.Location.City, location.City)
	case LocationCountry:
		return code == location.Country
	case LocationArea:
		return inArea(code, location.Name)
	case LocationContinent:
		return inContinent(code, location.Name)
	}
	return false
}

// tiers groups a provider's servers by the exit's location preferences:
// tiers[0] matches the first location, and so on. A server appears only in
// the first tier it matches. Excluded countries are left out everywhere. An
// exit without locations has one tier with every server. Strict exits keep
// only the first tier. Within a tier, servers are sorted by name.
func tiers(exit Exit, servers []Server) [][]Server {
	allowed := make([]Server, 0, len(servers))
	for _, server := range servers {
		if !slices.Contains(exit.Exclude, server.Location.Country) {
			allowed = append(allowed, server)
		}
	}
	slices.SortFunc(allowed, func(a, b Server) int { return strings.Compare(a.Name, b.Name) })

	if len(exit.Locations) == 0 {
		return [][]Server{allowed}
	}
	placed := map[string]bool{}
	var result [][]Server
	for _, location := range exit.Locations {
		var tier []Server
		for _, server := range allowed {
			if !placed[server.Name] && matches(server, location) {
				tier = append(tier, server)
				placed[server.Name] = true
			}
		}
		result = append(result, tier)
		if exit.Strict {
			break
		}
	}
	return result
}

// exitState is an exit's current choice.
type exitState struct {
	current  string
	chosenAt time.Time
}

// choose picks the server an exit should use now. It keeps the current one
// while it is healthy and in the best tier that has a healthy server — so an
// exit moves back to a preferred location once it recovers — and otherwise
// picks a replacement from that tier by the exit's selection strategy.
func choose(exit Exit, servers []Server, state *exitState, healthOf func(string) *health, now time.Time, random *rand.Rand) (Server, error) {
	for _, tier := range tiers(exit, servers) {
		var healthy []Server
		for _, server := range tier {
			if !healthOf(server.Name).benched(now) {
				healthy = append(healthy, server)
			}
		}
		if len(healthy) == 0 {
			continue
		}

		keep := state.current != "" && !(exit.Selection == SelectionRandom && now.Sub(state.chosenAt) >= rotation)
		if keep {
			if index := slices.IndexFunc(healthy, func(server Server) bool { return server.Name == state.current }); index >= 0 {
				return healthy[index], nil
			}
		}

		var picked Server
		switch exit.Selection {
		case SelectionRandom:
			// Avoid picking the one being rotated away from, when there's a choice.
			options := slices.DeleteFunc(slices.Clone(healthy), func(server Server) bool { return server.Name == state.current && len(healthy) > 1 })
			picked = options[random.IntN(len(options))]
		case SelectionLeastFailed:
			picked = slices.MinFunc(healthy, func(a, b Server) int {
				return healthOf(a.Name).recentFailures(now) - healthOf(b.Name).recentFailures(now)
			})
		default: // sticky
			picked = healthy[0]
		}
		state.current, state.chosenAt = picked.Name, now
		return picked, nil
	}
	return Server{}, noServerError(exit, servers)
}

// noServerError explains why an exit has no server, which is what a user
// needs to fix their configuration.
func noServerError(exit Exit, servers []Server) error {
	var wanted []string
	for _, location := range exit.Locations {
		wanted = append(wanted, location.String())
	}
	matching := 0
	for _, tier := range tiers(exit, servers) {
		matching += len(tier)
	}
	switch {
	case len(servers) == 0:
		return fmt.Errorf("%w: provider '%s' has no servers", ErrNoServer, exit.Provider)
	case matching == 0 && len(wanted) > 0:
		return fmt.Errorf("%w: no server of provider '%s' is in %s", ErrNoServer, exit.Provider, strings.Join(wanted, ", "))
	case matching == 0:
		return fmt.Errorf("%w: every server of provider '%s' is excluded", ErrNoServer, exit.Provider)
	default:
		return fmt.Errorf("%w: all %d matching servers are failing; retrying as their bench time ends", ErrNoServer, matching)
	}
}
