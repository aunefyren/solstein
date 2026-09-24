package exits

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"aunefyren/solstein/settings"
)

// Test keys: 32 bytes each, base64 like a .conf file's PrivateKey.
var (
	testKeyA = base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	testKeyB = base64.StdEncoding.EncodeToString([]byte("fedcba9876543210fedcba9876543210"))
)

func testEnv(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func validVPN() settings.VPN {
	return settings.VPN{
		Providers: map[string]settings.VPNProvider{
			"proton": {
				Type:        "ProtonVPN",
				PrivateKeys: []string{"env:PROTON_KEY_1", "env:PROTON_KEY_2"},
				Filter:      &settings.VPNServerFilter{Countries: []string{"se", "uk"}, SecureCore: "Include"},
			},
			"vps": {Type: "wireguard", ConfigFile: "/app/config/wireguard/vps.conf", Country: "de", City: "Frankfurt"},
			"mullvad": {
				Type:      "wireguard",
				ConfigDir: "/app/config/wireguard/mullvad",
				Servers:   map[string]settings.VPNServerLocation{"se-sto-wg-001": {Country: "SE", City: "Stockholm"}},
			},
		},
		Exits: map[string]settings.VPNExit{
			"sweden": {Provider: "proton", Locations: []string{"SE"}, Strict: true},
			"nordic": {Provider: "proton", Locations: []string{"se", "DK/Copenhagen", "area:Northern-Europe"}, Exclude: []string{"no"}},
			"any":    {Provider: "mullvad", Selection: "Random"},
			"vps":    {Provider: "vps", Locations: []string{"server:vps"}},
		},
	}
}

var testKeys = map[string]string{"PROTON_KEY_1": testKeyA, "PROTON_KEY_2": testKeyB}

func TestLoadValid(t *testing.T) {
	config, problems := Load(validVPN(), testEnv(testKeys))
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}

	proton := config.Providers["proton"]
	if proton.Type != TypeProtonVPN || len(proton.PrivateKeys) != 2 || proton.Tier != "plus" || proton.MaxTunnels != 2 {
		t.Errorf("proton = %+v", proton)
	}
	if proton.PrivateKeys[0].Bytes() != [32]byte([]byte("0123456789abcdef0123456789abcdef")) {
		t.Error("key not decoded")
	}
	if !reflect.DeepEqual(proton.Filter.Countries, []string{"SE", "GB"}) || proton.Filter.SecureCore != "include" || proton.Filter.Tor != "exclude" {
		t.Errorf("filter = %+v", proton.Filter)
	}

	vps := config.Providers["vps"]
	if !reflect.DeepEqual(vps.ConfigFiles, []string{"/app/config/wireguard/vps.conf"}) || vps.Locations["vps"] != (ServerLocation{Country: "DE", City: "Frankfurt"}) || vps.MaxTunnels != 0 {
		t.Errorf("vps = %+v", vps)
	}
	if mullvad := config.Providers["mullvad"]; mullvad.ConfigDir == "" || mullvad.Locations["se-sto-wg-001"].Country != "SE" {
		t.Errorf("mullvad = %+v", mullvad)
	}

	nordic := config.Exits["nordic"]
	wantLocations := []Location{
		{Kind: LocationCountry, Country: "SE"},
		{Kind: LocationCity, Country: "DK", City: "Copenhagen"},
		{Kind: LocationArea, Name: "northern-europe"},
	}
	if !reflect.DeepEqual(nordic.Locations, wantLocations) || !reflect.DeepEqual(nordic.Exclude, []string{"NO"}) || nordic.Selection != SelectionSticky {
		t.Errorf("nordic = %+v", nordic)
	}
	if config.Exits["any"].Selection != SelectionRandom || !config.Exits["sweden"].Strict {
		t.Errorf("exits = %+v", config.Exits)
	}
}

func TestLoadEmpty(t *testing.T) {
	config, problems := Load(settings.VPN{}, testEnv(nil))
	if len(problems) != 0 || len(config.Providers) != 0 || len(config.Exits) != 0 {
		t.Errorf("empty config: %+v, %v", config, problems)
	}
}

func TestLoadProviderProblems(t *testing.T) {
	cases := []struct {
		name     string
		provider settings.VPNProvider
		wantErr  string
	}{
		{"missing type", settings.VPNProvider{}, "type is missing"},
		{"unknown type", settings.VPNProvider{Type: "openvpn"}, "unknown type"},
		{"proton without keys", settings.VPNProvider{Type: "protonvpn"}, "at least one key"},
		{"key not set", settings.VPNProvider{Type: "protonvpn", PrivateKeys: []string{"env:NOT_SET"}}, "NOT_SET is not set"},
		{"key not a key", settings.VPNProvider{Type: "protonvpn", PrivateKeys: []string{"hunter2"}}, "not a WireGuard key"},
		{"bad tier", settings.VPNProvider{Type: "protonvpn", PrivateKeys: []string{testKeyA}, Tier: "gold"}, "tier"},
		{"bad filter mode", settings.VPNProvider{Type: "protonvpn", PrivateKeys: []string{testKeyA}, Filter: &settings.VPNServerFilter{Tor: "sometimes"}}, "tor"},
		{"bad filter country", settings.VPNProvider{Type: "protonvpn", PrivateKeys: []string{testKeyA}, Filter: &settings.VPNServerFilter{Countries: []string{"Sweden"}}}, "country code"},
		{"proton with conf file", settings.VPNProvider{Type: "protonvpn", PrivateKeys: []string{testKeyA}, ConfigFile: "x.conf"}, "for type wireguard"},
		{"wireguard with keys", settings.VPNProvider{Type: "wireguard", ConfigFile: "x.conf", PrivateKeys: []string{testKeyA}}, "carries its own key"},
		{"wireguard without source", settings.VPNProvider{Type: "wireguard"}, "exactly one"},
		{"wireguard with two sources", settings.VPNProvider{Type: "wireguard", ConfigFile: "x.conf", ConfigDir: "dir"}, "exactly one"},
		{"single-file country on a directory", settings.VPNProvider{Type: "wireguard", ConfigDir: "dir", Country: "SE"}, "single config_file"},
		{"bad server country", settings.VPNProvider{Type: "wireguard", ConfigDir: "dir", Servers: map[string]settings.VPNServerLocation{"a": {Country: "Sverige"}}}, "server 'a'"},
		{"negative max_tunnels", settings.VPNProvider{Type: "wireguard", ConfigFile: "x.conf", MaxTunnels: -1}, "max_tunnels"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			vpn := settings.VPN{Providers: map[string]settings.VPNProvider{"broken": c.provider}}
			config, problems := Load(vpn, testEnv(nil))
			if len(problems) != 1 || !strings.Contains(problems[0].Error(), c.wantErr) || !strings.HasPrefix(problems[0].Error(), "provider 'broken'") {
				t.Fatalf("problems = %v, want one mentioning %q", problems, c.wantErr)
			}
			if _, ok := config.Providers["broken"]; ok {
				t.Error("broken provider kept")
			}
		})
	}

	vpn := settings.VPN{Providers: map[string]settings.VPNProvider{"Bad Name": {Type: "wireguard", ConfigFile: "x.conf"}}}
	if _, problems := Load(vpn, testEnv(nil)); len(problems) != 1 || !strings.Contains(problems[0].Error(), "names are") {
		t.Errorf("bad provider name: %v", problems)
	}
}

func TestLoadExitProblems(t *testing.T) {
	cases := []struct {
		name    string
		exit    settings.VPNExit
		wantErr string
	}{
		{"no provider", settings.VPNExit{}, "provider is missing"},
		{"unknown provider", settings.VPNExit{Provider: "nordvpn"}, "not configured"},
		{"bad location", settings.VPNExit{Provider: "vps", Locations: []string{"Sweden"}}, "location"},
		{"unknown location kind", settings.VPNExit{Provider: "vps", Locations: []string{"planet:mars"}}, "unknown location kind"},
		{"strict without locations", settings.VPNExit{Provider: "vps", Strict: true}, "strict"},
		{"bad exclude", settings.VPNExit{Provider: "vps", Exclude: []string{"Norway"}}, "exclude"},
		{"bad selection", settings.VPNExit{Provider: "vps", Selection: "fastest"}, "selection"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			vpn := settings.VPN{
				Providers: map[string]settings.VPNProvider{"vps": {Type: "wireguard", ConfigFile: "vps.conf"}},
				Exits:     map[string]settings.VPNExit{"broken": c.exit},
			}
			_, problems := Load(vpn, testEnv(nil))
			if len(problems) != 1 || !strings.Contains(problems[0].Error(), c.wantErr) || !strings.HasPrefix(problems[0].Error(), "exit 'broken'") {
				t.Errorf("problems = %v, want one mentioning %q", problems, c.wantErr)
			}
		})
	}

	vpn := settings.VPN{
		Providers: map[string]settings.VPNProvider{"vps": {Type: "wireguard", ConfigFile: "vps.conf"}},
		Exits:     map[string]settings.VPNExit{"direct": {Provider: "vps"}},
	}
	if _, problems := Load(vpn, testEnv(nil)); len(problems) != 1 || !strings.Contains(problems[0].Error(), "built-in") {
		t.Errorf("exit named direct: %v", problems)
	}
}

func TestProblemsStayLocal(t *testing.T) {
	// The proton key is missing: proton and its exits go, the rest stays.
	config, problems := Load(validVPN(), testEnv(map[string]string{"PROTON_KEY_1": testKeyA}))

	if _, ok := config.Providers["proton"]; ok {
		t.Error("proton kept without its second key")
	}
	for _, name := range []string{"vps", "mullvad"} {
		if _, ok := config.Providers[name]; !ok {
			t.Errorf("provider %s lost because of proton's problem", name)
		}
	}
	for _, name := range []string{"sweden", "nordic"} {
		if _, ok := config.Exits[name]; ok {
			t.Errorf("exit %s kept although its provider is disabled", name)
		}
	}
	if _, ok := config.Exits["any"]; !ok {
		t.Error("unrelated exit lost")
	}

	var text []string
	for _, problem := range problems {
		text = append(text, problem.Error())
	}
	joined := strings.Join(text, "\n")
	if len(problems) != 3 || !strings.Contains(joined, "PROTON_KEY_2 is not set") || !strings.Contains(joined, "disabled by an error above") {
		t.Errorf("problems:\n%s", joined)
	}
	if !errors.Is(problems[0], settings.ErrSecretUnavailable) && !errors.Is(problems[len(problems)-1], settings.ErrSecretUnavailable) {
		t.Error("secret problem doesn't wrap ErrSecretUnavailable")
	}
}

func TestKeysNeverPrinted(t *testing.T) {
	config, _ := Load(validVPN(), testEnv(testKeys))
	jsonData, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{
		fmt.Sprint(config), fmt.Sprintf("%v", config), fmt.Sprintf("%+v", config), fmt.Sprintf("%#v", config),
		fmt.Sprint(config.Providers["proton"].PrivateKeys[0]), string(jsonData),
	} {
		if strings.Contains(text, testKeyA) || strings.Contains(text, testKeyB) || strings.Contains(text, "0123456789abcdef") {
			t.Fatalf("key leaked into: %s", text)
		}
	}

	// A malformed key doesn't end up in the error either.
	vpn := settings.VPN{Providers: map[string]settings.VPNProvider{"p": {Type: "protonvpn", PrivateKeys: []string{"not-quite-a-secret-key"}}}}
	if _, problems := Load(vpn, testEnv(nil)); strings.Contains(problems[0].Error(), "not-quite-a-secret-key") {
		t.Errorf("malformed key in error: %v", problems[0])
	}
}

func TestParseLocation(t *testing.T) {
	cases := []struct {
		text string
		want Location
	}{
		{"SE", Location{Kind: LocationCountry, Country: "SE"}},
		{" se ", Location{Kind: LocationCountry, Country: "SE"}},
		{"UK", Location{Kind: LocationCountry, Country: "GB"}},
		{"se/Stockholm", Location{Kind: LocationCity, Country: "SE", City: "Stockholm"}},
		{"server:SE#12", Location{Kind: LocationServer, Name: "SE#12"}},
		{"Server: node-no-05.protonvpn.net", Location{Kind: LocationServer, Name: "node-no-05.protonvpn.net"}},
		{"area:Northern-Europe", Location{Kind: LocationArea, Name: "northern-europe"}},
		{"continent:europe", Location{Kind: LocationContinent, Name: "europe"}},
	}
	for _, c := range cases {
		got, err := ParseLocation(c.text)
		if err != nil || got != c.want {
			t.Errorf("ParseLocation(%q) = %+v, %v; want %+v", c.text, got, err, c.want)
		}
		if err == nil {
			// String() round-trips to an equivalent location.
			if again, err := ParseLocation(got.String()); err != nil || again != got {
				t.Errorf("round trip of %q via %q = %+v, %v", c.text, got.String(), again, err)
			}
		}
	}
	for _, text := range []string{"", "Sweden", "S", "SE/", "server:", "area:", "area:north europe", "planet:mars", "123"} {
		if _, err := ParseLocation(text); err == nil {
			t.Errorf("ParseLocation(%q) accepted", text)
		}
	}
}
