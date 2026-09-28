package outbound

import "testing"

// reportingProvider is a provider that reports its status.
type reportingProvider struct {
	fakeProvider
	providers []ProviderStatus
	exits     []ExitStatus
}

func (provider reportingProvider) ExitStatus() ([]ProviderStatus, []ExitStatus) {
	return provider.providers, provider.exits
}

func TestManagerStatus(t *testing.T) {
	vpn := reportingProvider{
		fakeProvider: fakeProvider{dialers: map[string]Dialer{"sweden": &fakeDialer{}, "norway": &fakeDialer{}}},
		providers:    []ProviderStatus{{Name: "proton", Keys: 2}},
		exits:        []ExitStatus{{Name: "sweden", Provider: "proton"}, {Name: "norway", Provider: "proton", Tunnel: &TunnelStatus{Server: "NO#1"}}},
	}
	quiet := fakeProvider{dialers: map[string]Dialer{"lan": &fakeDialer{}}} // reports nothing

	manager, err := New(Options{Providers: []Provider{vpn, quiet}, HomeCountry: "NO"})
	if err != nil {
		t.Fatal(err)
	}
	status := manager.Status()
	// Once per provider, not once per exit it has.
	if len(status.Providers) != 1 || status.Providers[0].Name != "proton" {
		t.Errorf("providers = %+v", status.Providers)
	}
	var names []string
	for _, exit := range status.Exits {
		names = append(names, exit.Name)
	}
	if len(names) != 3 || names[0] != "direct" || names[1] != "norway" || names[2] != "sweden" {
		t.Errorf("exits = %v, want direct, norway, sweden sorted", names)
	}
	if status.Exits[0].Country != "NO" || status.Exits[0].Provider != "" {
		t.Errorf("direct = %+v, want the home country and no provider", status.Exits[0])
	}

	manager, err = New(Options{Providers: []Provider{vpn}, DisableDirect: true, DefaultExit: "norway"})
	if err != nil {
		t.Fatal(err)
	}
	for _, exit := range manager.Status().Exits {
		if exit.Name == DirectExit {
			t.Error("direct reported while it's off")
		}
	}
}
