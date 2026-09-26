package exits

import (
	"strings"
	"testing"
)

// budgetModule is a module with one Proton provider of keyCount keys and one
// WireGuard provider limited by max_tunnels, with the named exits over them.
func budgetModule(t *testing.T, keyCount, maxTunnels int, protonExits, fileExits []string) *Module {
	t.Helper()
	keys := make([]Key, 0, keyCount)
	for range keyCount {
		keys = append(keys, generateKey(t))
	}
	config := Config{
		Providers: map[string]Provider{
			"proton": {Name: "proton", Type: TypeProtonVPN, PrivateKeys: keys, MaxTunnels: len(keys)},
			"files":  {Name: "files", Type: TypeWireGuard, MaxTunnels: maxTunnels},
		},
		Exits: map[string]Exit{},
	}
	for _, name := range protonExits {
		config.Exits[name] = Exit{Name: name, Provider: "proton"}
	}
	for _, name := range fileExits {
		config.Exits[name] = Exit{Name: name, Provider: "files"}
	}
	return New(config, nil, nil)
}

// Production's shape when the crash and the tunnel-limit failures were found:
// four Proton exits (region diff's pair and two fallbacks, the home exit also
// the default) over three keys.
func TestTunnelBudgetShortOfKeys(t *testing.T) {
	module := budgetModule(t, 3, 0, []string{"norway", "germany", "us", "sweden"}, nil)
	warnings := module.TunnelBudget([]ExitUse{
		{Exit: "norway", Reason: "the default exit"},
		{Exit: "norway", Reason: "region diff's home exit"},
		{Exit: "germany", Reason: "region diff's partner exit"},
		{Exit: "us", Reason: "a fallback exit"},
		{Exit: "sweden", Reason: "a fallback exit"},
	})
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want one", warnings)
	}
	for _, want := range []string{
		"provider 'proton' can hold 3 tunnels at once (one per private key)",
		"4 of its exits can be in use at the same moment",
		"norway (the default exit, region diff's home exit)",
		"germany (region diff's partner exit)",
		"Add 1 more private key",
	} {
		if !strings.Contains(warnings[0], want) {
			t.Errorf("warning lacks %q:\n%s", want, warnings[0])
		}
	}
}

func TestTunnelBudgetWithRoom(t *testing.T) {
	// A key per exit, and the same exit used for several things.
	module := budgetModule(t, 4, 0, []string{"norway", "germany", "us", "sweden"}, nil)
	uses := []ExitUse{
		{Exit: "norway", Reason: "the default exit"},
		{Exit: "norway", Reason: "region diff's home exit"},
		{Exit: "germany", Reason: "region diff's partner exit"},
		{Exit: "us", Reason: "a fallback exit"},
		{Exit: "sweden", Reason: "a fallback exit"},
		// Neither of these takes a tunnel from the provider.
		{Exit: "direct", Reason: "a feed"},
		{Exit: "gone", Reason: "a feed"},
	}
	if warnings := module.TunnelBudget(uses); len(warnings) != 0 {
		t.Errorf("warnings with a key per exit: %v", warnings)
	}

	// Several episodes through the same exits share their tunnels, so
	// repeating a use changes nothing.
	if warnings := module.TunnelBudget(append(uses, uses...)); len(warnings) != 0 {
		t.Errorf("warnings for repeated uses: %v", warnings)
	}
}

func TestTunnelBudgetPerProvider(t *testing.T) {
	// proton has room (2 keys, 2 exits); files doesn't (max_tunnels 1, 2 exits).
	module := budgetModule(t, 2, 1, []string{"norway", "germany"}, []string{"vps", "office"})
	warnings := module.TunnelBudget([]ExitUse{
		{Exit: "norway", Reason: "region diff's home exit"},
		{Exit: "germany", Reason: "region diff's partner exit"},
		{Exit: "vps", Reason: "feed 'a'"},
		{Exit: "office", Reason: "feed 'b'"},
	})
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want only the one about 'files'", warnings)
	}
	for _, want := range []string{
		"provider 'files' can hold 1 tunnel at once (max_tunnels)",
		"Raise max_tunnels by 1",
	} {
		if !strings.Contains(warnings[0], want) {
			t.Errorf("warning lacks %q:\n%s", want, warnings[0])
		}
	}

	// No limit at all: nothing to warn about.
	unlimited := budgetModule(t, 2, 0, nil, []string{"vps", "office"})
	if warnings := unlimited.TunnelBudget([]ExitUse{{Exit: "vps"}, {Exit: "office"}}); len(warnings) != 0 {
		t.Errorf("warnings without max_tunnels: %v", warnings)
	}
}

func TestExitsFit(t *testing.T) {
	module := budgetModule(t, 3, 1, []string{"norway", "germany", "us", "sweden"}, []string{"vps", "office"})
	cases := []struct {
		name  string
		exits []string
		fits  bool
		says  string
	}{
		{"the pair alone", []string{"norway", "germany"}, true, ""},
		{"the pair and one fallback", []string{"norway", "germany", "us"}, true, ""},
		{"the pair and both fallbacks", []string{"norway", "germany", "us", "sweden"}, false,
			"provider 'proton' can hold 3 tunnels at once (one per private key), and germany, norway, sweden, us need 4"},
		{"the same exit twice", []string{"norway", "norway", "germany"}, true, ""},
		{"exits this module doesn't have", []string{"norway", "germany", "direct", "gone"}, true, ""},
		{"one provider short", []string{"vps", "office"}, false, "provider 'files' can hold 1 tunnel at once (max_tunnels), and office, vps need 2"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			fits, reason := module.ExitsFit(test.exits)
			if fits != test.fits {
				t.Errorf("fits = %v, want %v (reason %q)", fits, test.fits, reason)
			}
			if reason != test.says {
				t.Errorf("reason = %q, want %q", reason, test.says)
			}
		})
	}
}
