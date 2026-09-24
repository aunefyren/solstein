package exits

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/netip"
	"slices"
	"sort"
	"sync"
	"time"

	"aunefyren/solstein/logger"
	"aunefyren/solstein/outbound"
)

// Module offers the configured exits to the core. It implements
// outbound.Provider: the core asks it for a Dialer per connection, and each
// dial picks the exit's server, opens its tunnel if needed and connects
// through it.
type Module struct {
	config  Config
	servers map[string][]Server // by provider
	pools   map[string]*pool    // by provider
	now     func() time.Time

	mutex  sync.Mutex
	states map[string]*exitState // by exit
	health map[string]*health    // by provider + "/" + server
	random *rand.Rand
}

// New builds the module from a validated config and each provider's servers.
func New(config Config, servers map[string][]Server, now func() time.Time) *Module {
	if now == nil {
		now = time.Now
	}
	module := &Module{
		config:  config,
		servers: servers,
		pools:   map[string]*pool{},
		now:     now,
		states:  map[string]*exitState{},
		health:  map[string]*health{},
		random:  rand.New(rand.NewPCG(uint64(now().UnixNano()), 0x50_4c_53_54)),
	}
	for name, provider := range config.Providers {
		module.pools[name] = newPool(name, provider.MaxTunnels, now)
	}
	for name := range config.Exits {
		module.states[name] = &exitState{}
	}
	return module
}

// Exits lists the exit names, for outbound.Provider.
func (module *Module) Exits() []string {
	names := make([]string, 0, len(module.config.Exits))
	for name := range module.config.Exits {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Dialer returns the dialer for an exit, for outbound.Provider. It fails
// with outbound.ErrExitUnavailable when no server can be used right now.
func (module *Module) Dialer(exitName string) (outbound.Dialer, error) {
	exit, ok := module.config.Exits[exitName]
	if !ok {
		return nil, outbound.ErrUnknownExit
	}
	if _, err := module.pick(exit); err != nil {
		return nil, fmt.Errorf("%w: %w", outbound.ErrExitUnavailable, err)
	}
	return exitDialer{module: module, exit: exit}, nil
}

// Run closes idle tunnels until ctx is cancelled, then closes them all.
func (module *Module) Run(ctx context.Context) {
	var wait sync.WaitGroup
	for _, pool := range module.pools {
		wait.Go(func() { pool.run(ctx) })
	}
	wait.Wait()
}

func (module *Module) pick(exit Exit) (Server, error) {
	module.mutex.Lock()
	defer module.mutex.Unlock()
	healthOf := func(server string) *health { return module.health[exit.Provider+"/"+server] }
	return choose(exit, module.servers[exit.Provider], module.states[exit.Name], healthOf, module.now(), module.random)
}

func (module *Module) reportFailure(provider string, server Server, reason error) {
	module.mutex.Lock()
	key := provider + "/" + server.Name
	h := module.health[key]
	if h == nil {
		h = &health{}
		module.health[key] = h
	}
	bench := h.recordFailure(module.now(), reason)
	module.mutex.Unlock()

	// A fresh tunnel next time: the endpoint may have moved, or the
	// device got into a bad state.
	module.pools[provider].forget(server.Name)
	logger.Log.Warn(fmt.Sprintf("VPN server %s (provider '%s') failed; not using it for %s. Error: %s", server.Name, provider, bench, reason))
}

func (module *Module) reportSuccess(provider string, server Server) {
	module.mutex.Lock()
	defer module.mutex.Unlock()
	if h := module.health[provider+"/"+server.Name]; h != nil {
		h.recordSuccess()
	}
}

// exitDialer is what the core dials through: it resolves the exit's server
// on every call, so a failed server is replaced without the core noticing.
type exitDialer struct {
	module *Module
	exit   Exit
}

// attempts per call: the current server, then one replacement.
const attempts = 2

func (dialer exitDialer) LookupIP(ctx context.Context, host string) ([]netip.Addr, error) {
	var addresses []netip.Addr
	err := dialer.through(ctx, func(tunnel *tunnel) error {
		var err error
		addresses, err = tunnel.LookupIP(ctx, host)
		return err
	})
	return addresses, err
}

func (dialer exitDialer) Dial(ctx context.Context, network string, address netip.AddrPort) (net.Conn, error) {
	var connection net.Conn
	err := dialer.through(ctx, func(tunnel *tunnel) error {
		var err error
		connection, err = tunnel.Dial(ctx, network, address)
		return err
	})
	return connection, err
}

// through runs an operation over the exit's current server's tunnel. If the
// tunnel itself fails, the server is benched and the operation retried once
// on the next choice.
func (dialer exitDialer) through(ctx context.Context, operation func(*tunnel) error) error {
	module := dialer.module
	var lastErr error
	for range attempts {
		server, err := module.pick(dialer.exit)
		if err != nil {
			if lastErr != nil {
				return fmt.Errorf("%w: %w (after: %w)", outbound.ErrExitUnavailable, err, lastErr)
			}
			return fmt.Errorf("%w: %w", outbound.ErrExitUnavailable, err)
		}

		tunnel, err := module.pools[dialer.exit.Provider].get(ctx, server)
		if errors.Is(err, ErrTunnelLimit) {
			return fmt.Errorf("%w: %w", outbound.ErrExitUnavailable, err)
		}
		if err != nil {
			module.reportFailure(dialer.exit.Provider, server, err)
			lastErr = err
			continue
		}

		if err := tunnel.awaitHandshake(ctx); err != nil {
			if ctx.Err() != nil {
				return err
			}
			module.reportFailure(dialer.exit.Provider, server, err)
			lastErr = err
			continue
		}

		err = operation(tunnel)
		switch {
		case err == nil:
			module.reportSuccess(dialer.exit.Provider, server)
			return nil
		case ctx.Err() != nil:
			return err // the caller gave up; nobody's fault
		case tunnel.handshakeFresh():
			// The tunnel works, so the failure lies beyond it: the
			// destination's problem, not the server's.
			return err
		}
		module.reportFailure(dialer.exit.Provider, server, fmt.Errorf("no working WireGuard handshake: %w", err))
		lastErr = err
	}
	return fmt.Errorf("%w: %w", outbound.ErrExitUnavailable, lastErr)
}

// ServerNames lists each provider's server names, for status output.
func (module *Module) ServerNames(provider string) []string {
	var names []string
	for _, server := range module.servers[provider] {
		names = append(names, server.Name)
	}
	slices.Sort(names)
	return names
}
