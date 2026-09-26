package exits

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"aunefyren/solstein/logger"
)

const (
	// idleTimeout closes a tunnel no connection has used for this long.
	idleTimeout  = 5 * time.Minute
	reapInterval = 30 * time.Second
)

// ErrTunnelLimit means max_tunnels are open and all of them are in use.
var ErrTunnelLimit = errors.New("tunnel limit reached; all tunnels are in use")

// pool keeps one provider's tunnels: opened on first use, closed when idle,
// at most max open at once.
type pool struct {
	provider string
	max      int // zero is no limit
	// keys are handed to tunnels of servers that don't carry their own
	// (server-list providers): each opening tunnel gets the least used key,
	// so with max_tunnels equal to the number of keys every tunnel has its
	// own. keyUse counts open tunnels per key.
	keys   []Key
	keyUse []int
	// keyLast is where and when each key was last used, for key affinity
	// (see pickKey).
	keyLast []keyPlace
	idle    time.Duration
	now     func() time.Time
	open    func(ctx context.Context, server Server) (*tunnel, error)

	mutex   sync.Mutex
	tunnels map[string]*tunnel // by server name
}

func newPool(provider string, max int, keys []Key, fallbackDNS []netip.Addr, now func() time.Time) *pool {
	if now == nil {
		now = time.Now
	}
	return &pool{
		provider: provider,
		max:      max,
		keys:     keys,
		keyUse:   make([]int, len(keys)),
		keyLast:  make([]keyPlace, len(keys)),
		idle:     idleTimeout,
		now:      now,
		open: func(ctx context.Context, server Server) (*tunnel, error) {
			opened, err := openTunnel(ctx, server, now)
			if err == nil {
				opened.fallbackDNS = fallbackDNS
			}
			return opened, err
		},
		tunnels: map[string]*tunnel{},
	}
}

// get returns the tunnel to a server, opening it if needed. At the limit it
// first closes the least recently used idle tunnel; if every tunnel is busy
// it returns ErrTunnelLimit rather than cut someone's download.
func (pool *pool) get(ctx context.Context, server Server) (*tunnel, error) {
	pool.mutex.Lock()
	defer pool.mutex.Unlock()

	if existing, ok := pool.tunnels[server.Name]; ok {
		return existing, nil
	}
	if pool.max > 0 && len(pool.tunnels) >= pool.max {
		victim := ""
		var longest time.Duration = -1
		for name, candidate := range pool.tunnels {
			// Never close a tunnel something is using.
			if candidate.activeCount() > 0 {
				continue
			}
			if idle := candidate.idleFor(); idle > longest {
				victim, longest = name, idle
			}
		}
		if victim == "" {
			return nil, fmt.Errorf("provider '%s': %w (max_tunnels %d)", pool.provider, ErrTunnelLimit, pool.max)
		}
		logger.Log.Debug("Closing idle tunnel to " + victim + " to make room for " + server.Name + ".")
		pool.remove(victim)
	}

	keyIndex := -1
	if len(pool.keys) > 0 {
		keyIndex = pool.pickKey(server.Name)
		if last := pool.keyLast[keyIndex]; last.server != "" && last.server != server.Name && pool.now().Sub(last.at) < keyMoveSettle {
			logger.Log.Info(fmt.Sprintf("Provider '%s': key %d moves from %s to %s after %s; the new tunnel may stall now and then for a few minutes, as the provider moves the key's session. More keys avoid this.",
				pool.provider, keyIndex+1, last.server, server.Name, pool.now().Sub(last.at).Round(time.Second)))
		}
		server.PrivateKey = pool.keys[keyIndex]
	}
	opened, err := pool.open(ctx, server)
	if err != nil {
		return nil, fmt.Errorf("open tunnel to %s: %w", server.Name, err)
	}
	opened.keyIndex = keyIndex
	if keyIndex >= 0 {
		pool.keyUse[keyIndex]++
		pool.keyLast[keyIndex] = keyPlace{server: server.Name, at: pool.now()}
	}
	pool.tunnels[server.Name] = opened
	logger.Log.Info("Opened WireGuard tunnel to " + server.Name + " (provider '" + pool.provider + "').")
	return opened, nil
}

// keyMoveSettle is how long after a key was last used on one server that
// moving it to another is taken to be safe. A Proton key works on one
// server at a time: moved sooner, the new tunnel was seen to stall
// repeatedly (docs/exits.md). Three minutes is a guess from WireGuard's
// 180-second session lifetime, not a measurement.
const keyMoveSettle = 3 * time.Minute

// keyPlace is the server a key was last used on, and when.
type keyPlace struct {
	server string
	at     time.Time
}

// pickKey chooses the key for a new tunnel to a server: the least used
// (so tunnels get a key each while there are enough); among those, one last
// used on the same server; then one not used elsewhere for keyMoveSettle,
// or never used; then the one that left another server longest ago. The
// caller holds the mutex.
func (pool *pool) pickKey(serverName string) int {
	now := pool.now()
	rank := func(i int) (uses, class int, since time.Duration) {
		last := pool.keyLast[i]
		switch {
		case last.server == serverName:
			class = 0
		case last.server == "" || now.Sub(last.at) >= keyMoveSettle:
			class = 1
		default:
			class = 2
		}
		since = time.Duration(1<<63 - 1) // never used: as long ago as can be
		if last.server != "" {
			since = now.Sub(last.at)
		}
		return pool.keyUse[i], class, since
	}
	best := 0
	for i := 1; i < len(pool.keys); i++ {
		uses, class, since := rank(i)
		bestUses, bestClass, bestSince := rank(best)
		if uses != bestUses {
			if uses < bestUses {
				best = i
			}
			continue
		}
		if class != bestClass {
			if class < bestClass {
				best = i
			}
			continue
		}
		if since > bestSince {
			best = i
		}
	}
	return best
}

// reap closes tunnels that have been idle longer than the idle timeout.
func (pool *pool) reap() {
	pool.mutex.Lock()
	defer pool.mutex.Unlock()
	for name, candidate := range pool.tunnels {
		if candidate.idleFor() >= pool.idle {
			pool.remove(name)
			logger.Log.Info("Closed idle WireGuard tunnel to " + name + ".")
		}
	}
}

// remove closes a tunnel and frees its key. The caller holds the mutex.
func (pool *pool) remove(serverName string) {
	candidate, ok := pool.tunnels[serverName]
	if !ok {
		return
	}
	candidate.close()
	if candidate.keyIndex >= 0 {
		pool.keyUse[candidate.keyIndex]--
		pool.keyLast[candidate.keyIndex] = keyPlace{server: serverName, at: pool.now()}
	}
	delete(pool.tunnels, serverName)
}

// forget closes and drops one tunnel, e.g. after it failed.
func (pool *pool) forget(serverName string) {
	pool.mutex.Lock()
	defer pool.mutex.Unlock()
	pool.remove(serverName)
}

func (pool *pool) closeAll() {
	pool.mutex.Lock()
	defer pool.mutex.Unlock()
	for name := range pool.tunnels {
		pool.remove(name)
	}
}

// openCount is how many tunnels are open.
func (pool *pool) openCount() int {
	pool.mutex.Lock()
	defer pool.mutex.Unlock()
	return len(pool.tunnels)
}

// run reaps idle tunnels until ctx is cancelled, then closes them all.
func (pool *pool) run(ctx context.Context) {
	ticker := time.NewTicker(reapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			pool.closeAll()
			return
		case <-ticker.C:
			pool.reap()
		}
	}
}
