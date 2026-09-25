// Package outbound is the only way Solstein makes outgoing requests. It hands
// out an HTTP client per exit: the built-in "direct" exit, plus any exits a
// module (such as the VPN module) provides. Every client is built here, on top
// of the exit's Dialer, so the safeguards — the private-address block, the
// timeouts, the User-Agent, ignoring proxy environment variables — apply to
// every exit the same way and a module can't bypass them.
package outbound

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"
)

// DirectExit is the name of the built-in exit that goes out through the host's
// own network. It exists unless DisableDirect is set.
const DirectExit = "direct"

var (
	ErrUnknownExit        = errors.New("unknown exit")
	ErrExitUnavailable    = errors.New("exit unavailable")
	ErrDestinationBlocked = errors.New("destination address is not allowed")
	// ErrDirectDisabled is returned for the direct exit when disable_direct
	// is on. It wraps ErrUnknownExit, since to everything else the exit
	// simply doesn't exist.
	ErrDirectDisabled = fmt.Errorf("%w: the direct exit is disabled (disable_direct)", ErrUnknownExit)
)

// Dialer is one route out: how to resolve names and open connections through
// it. For a VPN exit both go through the tunnel, so DNS doesn't leak to the
// host's resolver.
type Dialer interface {
	LookupIP(ctx context.Context, host string) ([]netip.Addr, error)
	Dial(ctx context.Context, network string, address netip.AddrPort) (net.Conn, error)
}

// Provider is implemented by modules that add exits. Dialer returns an error
// wrapping ErrExitUnavailable when the exit exists but can't be used right
// now (e.g. its tunnel is down).
type Provider interface {
	Exits() []string
	Dialer(exit string) (Dialer, error)
}

// Locator is implemented by providers that know where their exits come out.
// Region diff uses it to avoid comparing two exits in the same country.
type Locator interface {
	// ExitCountries lists every country (ISO 3166-1 alpha-2) the exit may
	// use; known is false when that can't be told.
	ExitCountries(exit string) (countries []string, known bool)
	// ExitCountry is the country the exit's next connection goes out in.
	ExitCountry(exit string) (country string, known bool)
}

// Options configures a Manager.
type Options struct {
	// UserAgent is sent on every request that doesn't set its own.
	UserAgent string
	// AllowPrivateDestinations lifts the block on loopback, private and other
	// non-public addresses, e.g. for a feed hosted on the LAN.
	AllowPrivateDestinations bool
	Providers                []Provider
	// DefaultExit is used for requests that name no exit. Empty means
	// direct.
	DefaultExit string
	// DisableDirect removes the direct exit, so nothing goes out on the
	// host's own connection. It requires a DefaultExit.
	DisableDirect bool
}

// Manager hands out HTTP clients per exit. It is safe for concurrent use.
type Manager struct {
	options   Options
	providers map[string]Provider // exit name → provider; nil for direct
	direct    Dialer

	mutex   sync.Mutex
	clients map[string]*http.Client
}

// New builds a Manager. It fails if two providers claim the same exit name,
// one claims "direct", DefaultExit names no exit, or DisableDirect is set
// without a DefaultExit. The last two are errors rather than a quiet fall
// back to direct, which would leak the host's address against the
// operator's explicit wish.
func New(options Options) (*Manager, error) {
	manager := &Manager{
		options:   options,
		providers: map[string]Provider{DirectExit: nil},
		direct:    directDialer{},
		clients:   map[string]*http.Client{},
	}
	for _, provider := range options.Providers {
		for _, exit := range provider.Exits() {
			if _, taken := manager.providers[exit]; taken {
				return nil, fmt.Errorf("exit name %q is used more than once", exit)
			}
			manager.providers[exit] = provider
		}
	}
	if options.DisableDirect {
		delete(manager.providers, DirectExit)
		if options.DefaultExit == "" {
			return nil, errors.New("disable_direct needs a default_exit: every request without an exit of its own has to go somewhere")
		}
	}
	if options.DefaultExit != "" {
		if _, ok := manager.providers[options.DefaultExit]; !ok {
			return nil, fmt.Errorf("default_exit %q is not an available exit (available: %s)", options.DefaultExit, strings.Join(manager.Exits(), ", "))
		}
	}
	return manager, nil
}

// Exits lists every exit name, sorted, including "direct" unless it is
// disabled.
func (manager *Manager) Exits() []string {
	exits := make([]string, 0, len(manager.providers))
	for exit := range manager.providers {
		exits = append(exits, exit)
	}
	sort.Strings(exits)
	return exits
}

// Client returns the HTTP client for an exit; an empty name means the
// default exit ("direct" unless DefaultExit is set). It returns
// ErrUnknownExit for names no provider has, and ErrDirectDisabled for
// "direct" when it is disabled. Availability is checked
// per connection, so a client for a VPN exit whose tunnel is down fails its
// requests with ErrExitUnavailable rather than failing here.
//
// Clients have no overall timeout, because episode downloads are large:
// callers bound each request with a context deadline instead.
func (manager *Manager) Client(exit string) (*http.Client, error) {
	if exit == "" {
		exit = manager.DefaultExit()
	}
	if exit == DirectExit && manager.options.DisableDirect {
		return nil, ErrDirectDisabled
	}
	if _, ok := manager.providers[exit]; !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownExit, exit)
	}

	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	if client, ok := manager.clients[exit]; ok {
		return client, nil
	}
	client := manager.newClient(exit)
	manager.clients[exit] = client
	return client, nil
}

// DefaultExit is the exit used for requests that name none.
func (manager *Manager) DefaultExit() string {
	if manager.options.DefaultExit != "" {
		return manager.options.DefaultExit
	}
	return DirectExit
}

// ExitCountries lists every country an exit may come out in, when its
// provider can tell (see Locator). The direct exit's country is unknown:
// finding it would take an outside geolocation service.
func (manager *Manager) ExitCountries(exit string) ([]string, bool) {
	if locator, ok := manager.providers[exit].(Locator); ok {
		return locator.ExitCountries(exit)
	}
	return nil, false
}

// ExitCountry is the country an exit's next connection goes out in, when
// its provider can tell.
func (manager *Manager) ExitCountry(exit string) (string, bool) {
	if locator, ok := manager.providers[exit].(Locator); ok {
		return locator.ExitCountry(exit)
	}
	return "", false
}

// dialerFor is looked up on every connection rather than once per client, so
// a module can swap servers or bring a tunnel back up without the core
// rebuilding clients.
func (manager *Manager) dialerFor(exit string) (Dialer, error) {
	provider := manager.providers[exit]
	if provider == nil {
		return manager.direct, nil
	}
	dialer, err := provider.Dialer(exit)
	if err != nil {
		return nil, fmt.Errorf("exit %q: %w", exit, err)
	}
	return dialer, nil
}

const maxRedirects = 10

func (manager *Manager) newClient(exit string) *http.Client {
	transport := &http.Transport{
		// Never HTTP_PROXY/HTTPS_PROXY from the environment: a proxy would
		// carry the traffic out of a route other than the chosen exit.
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			dialer, err := manager.dialerFor(exit)
			if err != nil {
				return nil, err
			}
			return guardedDial(ctx, dialer, network, address, manager.options.AllowPrivateDestinations)
		},
		// A custom DialContext turns HTTP/2 off unless this is set.
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
	}

	return &http.Client{
		Transport: userAgentTransport{next: transport, userAgent: manager.options.UserAgent},
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			return checkScheme(request)
		},
	}
}

// checkScheme rejects anything but http and https. The transport would refuse
// other schemes anyway; checking explicitly keeps the rule visible and gives
// a clear error.
func checkScheme(request *http.Request) error {
	if request.URL.Scheme != "http" && request.URL.Scheme != "https" {
		return fmt.Errorf("%w: scheme %q", ErrDestinationBlocked, request.URL.Scheme)
	}
	return nil
}

// userAgentTransport sets the User-Agent on requests that don't set one, and
// enforces the scheme rule on the first request (CheckRedirect covers the
// rest).
type userAgentTransport struct {
	next      http.RoundTripper
	userAgent string
}

func (transport userAgentTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if err := checkScheme(request); err != nil {
		return nil, err
	}
	if transport.userAgent != "" && request.Header.Get("User-Agent") == "" {
		// RoundTrippers must not modify the caller's request.
		request = request.Clone(request.Context())
		request.Header.Set("User-Agent", transport.userAgent)
	}
	return transport.next.RoundTrip(request)
}
