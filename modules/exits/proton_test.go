package exits

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func protonProvider(tier string, filter Filter) Provider {
	if filter.SecureCore == "" {
		filter.SecureCore = "exclude"
	}
	if filter.Tor == "" {
		filter.Tor = "exclude"
	}
	return Provider{Name: "proton", Type: TypeProtonVPN, Tier: tier, Filter: filter}
}

// rawSnapshot is the embedded list, decompressed.
func rawSnapshot(t *testing.T) []byte {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(protonSnapshot))
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestEmbeddedProtonListMapsEveryCountry(t *testing.T) {
	list := embeddedProtonList()
	names := map[string]bool{}
	for _, server := range list.Servers {
		if server.VPN == "wireguard" {
			names[server.Country] = true
		}
	}
	if len(names) < 100 {
		t.Fatalf("only %d countries in the snapshot", len(names))
	}
	for name := range names {
		if _, ok := countryByName(name); !ok {
			// Add it to protonCountryNames.
			t.Errorf("Proton country %q doesn't map to an ISO code", name)
		}
	}
	for name, want := range map[string]string{"Korea": "KR", "United Kingdom": "GB", "Norway": "NO", "Russian Federation": "RU", "Cote d'Ivoire": "CI"} {
		if got, _ := countryByName(name); got != want {
			t.Errorf("countryByName(%q) = %s, want %s", name, got, want)
		}
	}
}

func TestProtonServersDefaults(t *testing.T) {
	servers, warnings := protonServers(embeddedProtonList(), protonProvider("plus", Filter{}))
	if len(warnings) != 0 {
		t.Errorf("warnings: %v", warnings)
	}
	if len(servers) < 500 {
		t.Fatalf("only %d servers", len(servers))
	}
	seen := map[string]bool{}
	for _, server := range servers {
		if seen[server.Name] {
			t.Fatalf("duplicate server name %s: the pool keys on names", server.Name)
		}
		seen[server.Name] = true
		if !knownCountry(server.Location.Country) || !strings.HasSuffix(server.Endpoint, ":51820") ||
			server.Addresses[0] != protonAddress || server.DNS[0] != protonDNS || server.ServerName == "" {
			t.Fatalf("bad server %+v", server)
		}
		if strings.Contains(server.ServerName, "-TOR") || strings.HasPrefix(server.ServerName, "CH-") && server.Location.Country != "CH" {
			t.Errorf("Tor or Secure Core server %s included by default", server.ServerName)
		}
	}
}

func TestProtonTierAndFilter(t *testing.T) {
	list := embeddedProtonList()

	free, _ := protonServers(list, protonProvider("free", Filter{}))
	if len(free) == 0 || len(free) > 100 {
		t.Errorf("free tier: %d servers", len(free))
	}

	swedish, _ := protonServers(list, protonProvider("plus", Filter{Countries: []string{"SE"}}))
	if len(swedish) == 0 {
		t.Fatal("no Swedish servers")
	}
	for _, server := range swedish {
		if server.Location.Country != "SE" {
			t.Errorf("filter let through %s in %s", server.Name, server.Location.Country)
		}
	}

	secureCore, _ := protonServers(list, protonProvider("plus", Filter{SecureCore: "only"}))
	if len(secureCore) == 0 {
		t.Fatal("no Secure Core servers")
	}
	for _, server := range secureCore {
		if !strings.Contains(server.ServerName, "-") || strings.Contains(server.ServerName, "-TOR") {
			t.Errorf("secure_core only let through %s", server.ServerName)
		}
	}

	tor, _ := protonServers(list, protonProvider("plus", Filter{Tor: "only"}))
	if len(tor) == 0 {
		t.Fatal("no Tor servers")
	}
	for _, server := range tor {
		if !strings.HasSuffix(server.ServerName, "-TOR") {
			t.Errorf("tor only let through %s", server.ServerName)
		}
	}

	one := swedish[0]
	byName, _ := protonServers(list, protonProvider("plus", Filter{Servers: []string{one.ServerName}}))
	byHost, _ := protonServers(list, protonProvider("plus", Filter{Servers: []string{strings.ToUpper(one.Hostname)}}))
	if len(byName) == 0 || len(byHost) == 0 {
		t.Errorf("server filter by name %d, by hostname %d", len(byName), len(byHost))
	}

	if none, warnings := protonServers(list, protonProvider("free", Filter{Countries: []string{"SE"}})); len(none) != 0 || !strings.Contains(strings.Join(warnings, " "), "no Proton server matches") {
		t.Errorf("free Swedish servers: %d, %v", len(none), warnings)
	}
}

func TestProtonSharedNames(t *testing.T) {
	servers, _ := protonServers(embeddedProtonList(), protonProvider("plus", Filter{SecureCore: "include"}))
	var shared []Server
	for _, server := range servers {
		if strings.Contains(server.Name, "@") {
			shared = append(shared, server)
		}
	}
	if len(shared) == 0 {
		t.Fatal("no shared names in the snapshot; the test needs updating")
	}
	example := shared[0]
	if example.Name != example.ServerName+"@"+strings.TrimSuffix(example.Endpoint, ":51820") {
		t.Errorf("shared-name server %q, server name %q", example.Name, example.ServerName)
	}
	// "server:NAME" still matches every server with that Proton name.
	location, _ := ParseLocation("server:" + example.ServerName)
	matching := 0
	for _, server := range servers {
		if matches(server, location) {
			matching++
		}
	}
	if matching < 2 {
		t.Errorf("server:%s matched %d servers, want all sharing the name", example.ServerName, matching)
	}
}

func TestParseProtonListErrors(t *testing.T) {
	raw := rawSnapshot(t)
	if _, err := parseProtonList(raw); err != nil {
		t.Errorf("uncompressed list: %v", err)
	}
	var list map[string]any
	json.Unmarshal(raw, &list)

	list["version"] = 5
	future, _ := json.Marshal(list)
	if _, err := parseProtonList(future); err == nil || !strings.Contains(err.Error(), "format version 5") {
		t.Errorf("future version: %v", err)
	}
	list["version"] = 4
	list["servers"] = list["servers"].([]any)[:10]
	truncated, _ := json.Marshal(list)
	if _, err := parseProtonList(truncated); err == nil || !strings.Contains(err.Error(), "refusing it as broken") {
		t.Errorf("truncated list: %v", err)
	}
	if _, err := parseProtonList([]byte("<html>rate limited</html>")); err == nil {
		t.Error("HTML accepted as a server list")
	}
}

// newerList is the snapshot with a later timestamp, as a refresh would bring.
func newerList(t *testing.T, by time.Duration) []byte {
	t.Helper()
	var list map[string]any
	json.Unmarshal(rawSnapshot(t), &list)
	list["timestamp"] = list["timestamp"].(float64) + by.Seconds()
	data, _ := json.Marshal(list)
	return data
}

func TestLoadProtonList(t *testing.T) {
	embedded := embeddedProtonList()
	configDir := t.TempDir()
	if list, cachedAt, source := loadProtonList(configDir); list.Timestamp != embedded.Timestamp || !cachedAt.IsZero() || source != "built-in list" {
		t.Errorf("no cache: %d %v %s", list.Timestamp, cachedAt, source)
	}

	path := filepath.Join(configDir, protonCacheFile)
	os.MkdirAll(filepath.Dir(path), 0o750)
	os.WriteFile(path, newerList(t, 24*time.Hour), 0o640)
	if list, cachedAt, source := loadProtonList(configDir); list.Timestamp <= embedded.Timestamp || cachedAt.IsZero() || !strings.HasPrefix(source, "cached list") {
		t.Errorf("newer cache not used: %s", source)
	}

	os.WriteFile(path, newerList(t, -24*time.Hour), 0o640)
	if list, _, source := loadProtonList(configDir); list.Timestamp != embedded.Timestamp || !strings.Contains(source, "newer than the cached copy") {
		t.Errorf("older cache used: %s", source)
	}

	os.WriteFile(path, []byte("{broken"), 0o640)
	if list, _, source := loadProtonList(configDir); list.Timestamp != embedded.Timestamp || !strings.Contains(source, "unusable") {
		t.Errorf("broken cache: %s", source)
	}
}

func TestRefreshProton(t *testing.T) {
	quietLogs(t)
	var body []byte
	status := http.StatusOK
	host := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(status)
		writer.Write(body)
	}))
	defer host.Close()

	configDir := t.TempDir()
	provider := protonProvider("plus", Filter{Countries: []string{"SE"}})
	config := Config{Providers: map[string]Provider{"proton": provider}, Exits: map[string]Exit{}}
	servers, _ := protonServers(embeddedProtonList(), provider)
	module := New(config, map[string][]Server{"proton": servers}, nil)
	module.configDir, module.protonList, module.protonListURL = configDir, embeddedProtonList(), host.URL
	module.SetFetchClient(host.Client())
	ctx := context.Background()
	cache := filepath.Join(configDir, protonCacheFile)

	// Same list: nothing changes, nothing is written.
	body = rawSnapshot(t)
	if err := module.refreshProton(ctx); err != nil {
		t.Fatalf("same list: %v", err)
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Error("an unchanged list was written")
	}

	// A broken or failed download keeps the current list.
	for _, bad := range []struct {
		status int
		body   []byte
	}{{http.StatusOK, []byte("{}")}, {http.StatusTooManyRequests, nil}} {
		status, body = bad.status, bad.body
		if err := module.refreshProton(ctx); err == nil {
			t.Errorf("status %d with %q accepted", bad.status, bad.body)
		}
	}
	if len(module.serversOf("proton")) != len(servers) {
		t.Error("servers changed after a failed refresh")
	}

	// A newer list replaces it, and is cached for the next start.
	status, body = http.StatusOK, newerList(t, 30*24*time.Hour)
	if err := module.refreshProton(ctx); err != nil {
		t.Fatalf("newer list: %v", err)
	}
	if module.protonList.Timestamp != embeddedProtonList().Timestamp+int64(30*24*time.Hour/time.Second) {
		t.Error("list not replaced")
	}
	if list, _, source := loadProtonList(configDir); !strings.HasPrefix(source, "cached list") || list.Timestamp != module.protonList.Timestamp {
		t.Errorf("refreshed list not cached: %s", source)
	}
	for _, server := range module.serversOf("proton") {
		if server.Location.Country != "SE" {
			t.Fatalf("rebuilt servers ignore the provider's filter: %s", server.Name)
		}
	}
}

func TestPoolHandsOutKeys(t *testing.T) {
	quietLogs(t)
	keys := []Key{generateKey(t), generateKey(t)}
	clock := &fakeClock{now: time.Now()}
	pool := newPool("proton", 3, keys, nil, clock.Now)
	var opened []Server
	pool.open = func(ctx context.Context, server Server) (*tunnel, error) {
		opened = append(opened, server)
		return &tunnel{server: server, now: clock.Now, lastUsed: clock.Now(), keyIndex: -1}, nil
	}
	ctx := context.Background()
	for _, name := range []string{"a", "b", "c"} {
		if _, err := pool.get(ctx, Server{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	if opened[0].PrivateKey != keys[0] || opened[1].PrivateKey != keys[1] {
		t.Error("the first two tunnels don't have a key each")
	}
	if pool.keyUse[0]+pool.keyUse[1] != 3 || pool.keyUse[0] < 1 || pool.keyUse[1] < 1 {
		t.Errorf("key use = %v", pool.keyUse)
	}

	pool.forget("a")
	pool.forget("b")
	pool.get(ctx, Server{Name: "d"})
	if pool.keyUse[0]+pool.keyUse[1] != 2 {
		t.Errorf("keys not freed on close: %v", pool.keyUse)
	}
	pool.closeAll()
	if pool.keyUse[0] != 0 || pool.keyUse[1] != 0 {
		t.Errorf("keys in use after closeAll: %v", pool.keyUse)
	}
}

func TestProtonListAgeWarning(t *testing.T) {
	list := protonList{Timestamp: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC).Unix()}
	fresh := list.UpdatedAt().Add(protonListMaxAge)
	stale := fresh.Add(24 * time.Hour)
	_, formatErr := parseProtonList([]byte(`{"version": 5}`))
	cases := []struct {
		name string
		now  time.Time
		err  error
		want string // "" for no warning
	}{
		{"recent", fresh, nil, ""},
		{"recent, refresh failing", fresh, errors.New("timeout"), ""},
		{"old, source not updated", stale, nil, "hasn't been updated"},
		{"old, refresh failing", stale, errors.New("timeout"), "Refreshing it fails"},
		{"old, new format", stale, formatErr, "a newer Solstein is needed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			warning := protonListAgeWarning(list, c.now, c.err)
			if c.want == "" && warning != "" || c.want != "" && (!strings.Contains(warning, c.want) || !strings.Contains(warning, "2026-07-01, 61 days old")) {
				t.Errorf("warning %q", warning)
			}
		})
	}
}
