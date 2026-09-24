package exits

import (
	"context"
	"errors"
	"fmt"
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
	idle     time.Duration
	now      func() time.Time
	open     func(ctx context.Context, server Server) (*tunnel, error)

	mutex   sync.Mutex
	tunnels map[string]*tunnel // by server name
}

func newPool(provider string, max int, now func() time.Time) *pool {
	if now == nil {
		now = time.Now
	}
	return &pool{
		provider: provider,
		max:      max,
		idle:     idleTimeout,
		now:      now,
		open: func(ctx context.Context, server Server) (*tunnel, error) {
			return openTunnel(ctx, server, now)
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
		pool.tunnels[victim].close()
		delete(pool.tunnels, victim)
	}

	opened, err := pool.open(ctx, server)
	if err != nil {
		return nil, fmt.Errorf("open tunnel to %s: %w", server.Name, err)
	}
	pool.tunnels[server.Name] = opened
	logger.Log.Info("Opened WireGuard tunnel to " + server.Name + " (provider '" + pool.provider + "').")
	return opened, nil
}

// reap closes tunnels that have been idle longer than the idle timeout.
func (pool *pool) reap() {
	pool.mutex.Lock()
	defer pool.mutex.Unlock()
	for name, candidate := range pool.tunnels {
		if candidate.idleFor() >= pool.idle {
			candidate.close()
			delete(pool.tunnels, name)
			logger.Log.Info("Closed idle WireGuard tunnel to " + name + ".")
		}
	}
}

// forget closes and drops one tunnel, e.g. after it failed.
func (pool *pool) forget(serverName string) {
	pool.mutex.Lock()
	defer pool.mutex.Unlock()
	if candidate, ok := pool.tunnels[serverName]; ok {
		candidate.close()
		delete(pool.tunnels, serverName)
	}
}

func (pool *pool) closeAll() {
	pool.mutex.Lock()
	defer pool.mutex.Unlock()
	for name, candidate := range pool.tunnels {
		candidate.close()
		delete(pool.tunnels, name)
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
