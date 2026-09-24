package exits

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"aunefyren/solstein/outbound"
)

// newTestModule builds a module with one provider whose servers are the
// given ones, and one exit over them.
func newTestModule(t *testing.T, exit Exit, servers ...Server) *Module {
	t.Helper()
	if exit.Name == "" {
		exit.Name = "test"
	}
	if exit.Provider == "" {
		exit.Provider = "local"
	}
	if exit.Selection == "" {
		exit.Selection = SelectionSticky
	}
	config := Config{
		Providers: map[string]Provider{exit.Provider: {Name: exit.Provider, Type: TypeWireGuard}},
		Exits:     map[string]Exit{exit.Name: exit},
	}
	module := New(config, map[string][]Server{exit.Provider: servers}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		module.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return module
}

// realServer starts an in-process WireGuard peer and returns the Server a
// .conf for it would describe, named and located as given.
func realServer(t *testing.T, name, country string) Server {
	t.Helper()
	clientKey := generateKey(t)
	peer := startWireGuardServer(t, clientKey.Public())
	server := clientServer(t, clientKey, peer, true)
	server.Name, server.Location = name, ServerLocation{Country: country}
	return server
}

func get(t *testing.T, module *Module, exit string) (string, error) {
	t.Helper()
	manager, err := outbound.New(outbound.Options{AllowPrivateDestinations: true, Providers: []outbound.Provider{module}})
	if err != nil {
		t.Fatal(err)
	}
	client, _ := manager.Client(exit)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	request, _ := httpRequest(ctx, "http://example.test/")
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return string(body), nil
}

func TestModuleServesThroughTunnel(t *testing.T) {
	quietLogs(t)
	module := newTestModule(t, Exit{Locations: mustLocations(t, "SE")}, realServer(t, "se-1", "SE"))

	if names := module.Exits(); len(names) != 1 || names[0] != "test" {
		t.Errorf("Exits = %v", names)
	}
	body, err := get(t, module, "test")
	if err != nil || !strings.HasPrefix(body, "hello from inside the tunnel") {
		t.Fatalf("GET through the exit = %q, %v", body, err)
	}
	if module.pools["local"].openCount() != 1 {
		t.Errorf("open tunnels = %d, want 1", module.pools["local"].openCount())
	}
}

func TestModuleFailsOverToTheNextServer(t *testing.T) {
	quietLogs(t)
	original := handshakeTimeout
	handshakeTimeout = time.Second
	t.Cleanup(func() { handshakeTimeout = original })
	// "se-1" sorts first, so sticky tries it first; its peer key is wrong,
	// so no handshake ever completes.
	broken := realServer(t, "se-1", "SE")
	broken.PeerPublicKey = generateKey(t).Public()
	working := realServer(t, "se-2", "SE")
	module := newTestModule(t, Exit{Locations: mustLocations(t, "SE")}, broken, working)

	body, err := get(t, module, "test")
	if err != nil || !strings.HasPrefix(body, "hello from inside the tunnel") {
		t.Fatalf("GET = %q, %v; want it to fail over to se-2", body, err)
	}
	if h := module.health["local/se-1"]; h == nil || !h.benched(time.Now()) {
		t.Error("broken server not benched")
	}
	if module.states["test"].current != "se-2" {
		t.Errorf("exit now uses %s, want se-2", module.states["test"].current)
	}
	if !strings.Contains(module.health["local/se-1"].lastError, "no WireGuard handshake") {
		t.Errorf("failure reason = %q", module.health["local/se-1"].lastError)
	}
}

func TestModuleDestinationErrorsDontBenchServers(t *testing.T) {
	quietLogs(t)
	module := newTestModule(t, Exit{}, realServer(t, "se-1", "SE"))
	if _, err := get(t, module, "test"); err != nil {
		t.Fatal(err)
	}

	// Port 81 isn't listening inside the tunnel: refused by the destination,
	// over a working tunnel.
	dialer, err := module.Dialer("test")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := dialer.Dial(ctx, "tcp", netip.AddrPortFrom(serverTunnelAddress, 81)); err == nil {
		t.Fatal("dial to a closed port succeeded")
	}
	if h := module.health["local/se-1"]; h != nil && h.benched(time.Now()) {
		t.Error("server benched for a destination's refusal")
	}
}

func TestModuleUnavailableExit(t *testing.T) {
	quietLogs(t)
	module := newTestModule(t, Exit{Locations: mustLocations(t, "JP"), Strict: true}, testServer("se-1", "SE", ""))
	if _, err := module.Dialer("test"); !errors.Is(err, outbound.ErrExitUnavailable) || !errors.Is(err, ErrNoServer) {
		t.Errorf("err = %v, want ErrExitUnavailable wrapping ErrNoServer", err)
	}
	if _, err := module.Dialer("nope"); !errors.Is(err, outbound.ErrUnknownExit) {
		t.Errorf("unknown exit: err = %v", err)
	}
	// Through the core, the failure surfaces as the exit being unavailable.
	if _, err := get(t, module, "test"); !errors.Is(err, outbound.ErrExitUnavailable) {
		t.Errorf("GET through an unavailable exit: err = %v", err)
	}
}

func httpRequest(ctx context.Context, url string) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
}
