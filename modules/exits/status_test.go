package exits

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestExitStatus(t *testing.T) {
	quietLogs(t)
	clock := &fakeClock{now: time.Now()}
	norway, sweden := mustLocation(t, "NO"), mustLocation(t, "SE")
	config := Config{
		Providers: map[string]Provider{"proton": {Name: "proton", Type: TypeProtonVPN, PrivateKeys: []Key{generateKey(t), generateKey(t)}, MaxTunnels: 2}},
		Exits: map[string]Exit{
			"norway": {Name: "norway", Provider: "proton", Locations: []Location{norway}, Strict: true, Selection: SelectionSticky},
			"sweden": {Name: "sweden", Provider: "proton", Locations: []Location{sweden}, Selection: SelectionSticky},
		},
	}
	servers := map[string][]Server{"proton": {
		{Name: "NO#1", Location: ServerLocation{Country: "NO"}},
		{Name: "SE#1", Location: ServerLocation{Country: "SE"}},
	}}
	module := New(config, servers, clock.Now)
	module.pools["proton"].open = func(ctx context.Context, server Server) (*tunnel, error) {
		return &tunnel{server: server, now: clock.Now, lastUsed: clock.Now(), keyIndex: -1}, nil
	}

	// norway is in use through its tunnel; sweden's only server failed.
	server, err := module.pick(config.Exits["norway"])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := module.pools["proton"].get(context.Background(), server); err != nil {
		t.Fatal(err)
	}
	module.reportFailure("proton", servers["proton"][1], errors.New("handshake timed out"))

	providers, exits := module.ExitStatus()
	if len(providers) != 1 {
		t.Fatalf("providers = %+v", providers)
	}
	provider := providers[0]
	if provider.Name != "proton" || provider.Type != TypeProtonVPN || provider.Servers != 2 || provider.Keys != 2 || provider.MaxTunnels != 2 {
		t.Errorf("provider = %+v", provider)
	}
	if len(provider.Tunnels) != 1 || provider.Tunnels[0].Server != "NO#1" || provider.Tunnels[0].Country != "NO" || provider.Tunnels[0].Users != 1 || provider.Tunnels[0].Key < 1 {
		t.Errorf("tunnels = %+v, want NO#1 in use with a numbered key", provider.Tunnels)
	}
	if len(provider.Benched) != 1 || provider.Benched[0].Server != "SE#1" || !provider.Benched[0].Until.After(clock.Now()) {
		t.Errorf("benched = %+v, want SE#1", provider.Benched)
	}

	if len(exits) != 2 || exits[0].Name != "norway" || exits[1].Name != "sweden" {
		t.Fatalf("exits = %+v", exits)
	}
	if got := exits[0]; got.Server != "NO#1" || got.Country != "NO" || got.Tunnel == nil || got.Tunnel.Users != 1 || !got.Strict || len(got.Locations) != 1 || got.Locations[0] != "NO" {
		t.Errorf("norway = %+v", got)
	}
	if got := exits[1]; got.Server != "" || got.Tunnel != nil || got.Strict {
		t.Errorf("sweden = %+v, want no server chosen yet and no tunnel", got)
	}
}

func mustLocation(t *testing.T, text string) Location {
	t.Helper()
	location, err := ParseLocation(text)
	if err != nil {
		t.Fatal(err)
	}
	return location
}

// TestExitStatusWhileTunnelsChange reads the status while tunnels open,
// close and fail, as the web UI does on a busy Solstein; run under -race.
func TestExitStatusWhileTunnelsChange(t *testing.T) {
	quietLogs(t)
	clock := &fakeClock{now: time.Now()}
	config := Config{
		Providers: map[string]Provider{"proton": {Name: "proton", Type: TypeProtonVPN, PrivateKeys: []Key{generateKey(t)}}},
		Exits:     map[string]Exit{"norway": {Name: "norway", Provider: "proton", Locations: []Location{mustLocation(t, "NO")}, Selection: SelectionSticky}},
	}
	servers := map[string][]Server{"proton": {{Name: "NO#1", Location: ServerLocation{Country: "NO"}}, {Name: "NO#2", Location: ServerLocation{Country: "NO"}}}}
	module := New(config, servers, clock.Now)
	module.pools["proton"].open = func(ctx context.Context, server Server) (*tunnel, error) {
		return &tunnel{server: server, now: clock.Now, lastUsed: clock.Now(), keyIndex: -1}, nil
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 200 {
			server, err := module.pick(config.Exits["norway"])
			if err != nil {
				continue
			}
			if opened, err := module.pools["proton"].get(context.Background(), server); err == nil {
				opened.release()
			}
			if i%10 == 0 {
				module.reportFailure("proton", server, errors.New("handshake timed out"))
			}
		}
	}()
	for {
		select {
		case <-done:
			return
		default:
			module.ExitStatus()
		}
	}
}
