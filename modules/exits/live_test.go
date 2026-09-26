//go:build live

// Live tests against real Proton servers and real podcast hosts. They only
// build with -tags live, never in CI, and need PROTON_KEY_1 (and
// PROTON_KEY_2 for the two-key test) in the environment — e.g. through
// docker run --env-file .env. They print countries, sizes and hashes, never
// keys, and never the host's own IP.
//
//	go test -tags live -run Live -v -count=1 ./modules/exits/
package exits

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"aunefyren/solstein/outbound"
	"aunefyren/solstein/rss"
	"aunefyren/solstein/settings"
)

// liveShow carries Norwegian dynamic ads (confirmed by the maintainer).
const liveShow = "https://feeds.acast.com/public/shows/6a85fe2bbaab9cd55fc0be36"

func requireKeys(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if os.Getenv(name) == "" {
			t.Skipf("%s not set; run with --env-file .env", name)
		}
	}
}

// liveManager sets up Proton exits the way main.go does.
func liveManager(t *testing.T, keys []string, maxTunnels int, exitCountries map[string]string) (*outbound.Manager, *Module) {
	t.Helper()
	exitsConfig := map[string]settings.VPNExit{}
	var countries []string
	for exit, country := range exitCountries {
		exitsConfig[exit] = settings.VPNExit{Provider: "proton", Locations: []string{country}, Strict: true}
		countries = append(countries, country)
	}
	vpn := settings.VPN{
		Providers: map[string]settings.VPNProvider{"proton": {
			Type: "protonvpn", PrivateKeys: keys, MaxTunnels: maxTunnels,
			Filter: &settings.VPNServerFilter{Countries: countries},
		}},
		Exits: exitsConfig,
	}
	module, warnings := Setup(vpn, t.TempDir(), os.Getenv)
	for _, warning := range warnings {
		t.Log("setup warning:", warning)
	}
	if module == nil {
		t.Fatal("no module")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { module.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	manager, err := outbound.New(outbound.Options{UserAgent: "Solstein/live-test (+https://github.com/aunefyren/solstein)", Providers: []outbound.Provider{module}})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(module.Summary())
	return manager, module
}

type whereAmI struct {
	Country string `json:"country_iso"`
	IP      string `json:"ip"`
	ASN     string `json:"asn_org"`
}

func lookUp(t *testing.T, manager *outbound.Manager, exit string) (whereAmI, error) {
	t.Helper()
	client, err := manager.Client(exit)
	if err != nil {
		return whereAmI{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://ifconfig.co/json", nil)
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return whereAmI{}, err
	}
	defer response.Body.Close()
	var answer whereAmI
	if err := json.NewDecoder(response.Body).Decode(&answer); err != nil {
		return whereAmI{}, fmt.Errorf("decode ifconfig.co answer (status %s): %w", response.Status, err)
	}
	return answer, nil
}

func TestLiveProtonExit(t *testing.T) {
	requireKeys(t, "PROTON_KEY_1")
	manager, module := liveManager(t, []string{"env:PROTON_KEY_1"}, 0, map[string]string{"sweden": "SE"})

	direct, err := lookUp(t, manager, outbound.DirectExit)
	if err != nil {
		t.Fatalf("direct: %v", err)
	}
	t.Logf("direct: country %s (IP not shown)", direct.Country)

	start := time.Now()
	through, err := lookUp(t, manager, "sweden")
	if err != nil {
		t.Fatalf("through the sweden exit: %v", err)
	}
	t.Logf("sweden exit: country %s, IP %s, network %q, first request %s (tunnel open + handshake)", through.Country, through.IP, through.ASN, time.Since(start).Round(time.Millisecond))
	if through.Country != "SE" {
		t.Errorf("exit IP geolocates to %s, want SE", through.Country)
	}
	if through.IP == direct.IP {
		t.Error("exit IP is the host's own; traffic didn't go through the tunnel")
	}

	start = time.Now()
	if _, err := lookUp(t, manager, "sweden"); err != nil {
		t.Errorf("second request: %v", err)
	}
	t.Logf("second request through the open tunnel: %s", time.Since(start).Round(time.Millisecond))
	t.Logf("server in use: %s", module.states["sweden"].current)
}

// concurrentTunnels checks that two tunnels to different countries work at
// the same time and keep working after each other is set up — the question
// being whether Proton lets one key hold two sessions.
func concurrentTunnels(t *testing.T, keys []string) {
	manager, module := liveManager(t, keys, 2, map[string]string{"sweden": "SE", "germany": "DE"})
	want := map[string]string{"sweden": "SE", "germany": "DE"}

	check := func(label, exit string) {
		answer, err := lookUp(t, manager, exit)
		switch {
		case err != nil:
			t.Errorf("%s: %s: %v", label, exit, err)
		case answer.Country != want[exit]:
			t.Errorf("%s: %s geolocates to %s, want %s", label, exit, answer.Country, want[exit])
		default:
			t.Logf("%s: %s OK (%s, %s)", label, exit, answer.Country, answer.IP)
		}
	}
	check("first", "sweden")
	check("then", "germany")
	check("sweden again, with the German tunnel open", "sweden")

	var wait sync.WaitGroup
	for range 3 {
		for _, exit := range []string{"sweden", "germany"} {
			wait.Go(func() { check("concurrent", exit) })
		}
	}
	wait.Wait()
	check("germany after the concurrent round", "germany")
	t.Logf("open tunnels: %d; key use: %v", module.pools["proton"].openCount(), module.pools["proton"].keyUse)
}

func TestLiveTwoKeysTwoTunnels(t *testing.T) {
	requireKeys(t, "PROTON_KEY_1", "PROTON_KEY_2")
	concurrentTunnels(t, []string{"env:PROTON_KEY_1", "env:PROTON_KEY_2"})
}

// TestLiveAcastRegions downloads the same episode directly (Norway) twice
// and through Sweden twice, and keeps the files for comparison.
func TestLiveAcastRegions(t *testing.T) {
	requireKeys(t, "PROTON_KEY_1")
	// LIVE_HOME_COUNTRY downloads the home side through a Proton exit in
	// that country instead of directly, as a disable_direct setup does.
	exits := map[string]string{"sweden": "SE"}
	home, keys := outbound.DirectExit, []string{"env:PROTON_KEY_1"}
	if country := os.Getenv("LIVE_HOME_COUNTRY"); country != "" {
		// A tunnel each needs a key each: a Proton key works on one server
		// at a time.
		requireKeys(t, "PROTON_KEY_2")
		exits["home"], home, keys = country, "home", append(keys, "env:PROTON_KEY_2")
	}
	manager, _ := liveManager(t, keys, 0, exits)
	output := os.Getenv("LIVE_OUTPUT_DIR")
	if output == "" {
		output = t.TempDir()
	}
	os.MkdirAll(output, 0o755)

	// LIVE_SHOW and LIVE_EPISODE (part of a title) pick another show and
	// episode than the default show's newest.
	show := liveShow
	if value := os.Getenv("LIVE_SHOW"); value != "" {
		show = value
	}
	feedData := fetch(t, manager, outbound.DirectExit, show, filepath.Join(output, "feed.xml"))
	feed, err := rss.Parse(feedData.body)
	if err != nil || len(feed.Items) == 0 || feed.Items[0].Enclosure == nil {
		t.Fatalf("feed: %v", err)
	}
	episode := feed.Items[0]
	if wanted := os.Getenv("LIVE_EPISODE"); wanted != "" {
		index := slices.IndexFunc(feed.Items, func(item rss.Item) bool {
			return item.Enclosure != nil && strings.Contains(strings.ToLower(item.Title), strings.ToLower(wanted))
		})
		if index < 0 {
			t.Fatalf("no episode titled like %q", wanted)
		}
		episode = feed.Items[index]
	}
	t.Logf("show %q, newest episode %q, stated length %d, duration %s", feed.Title, episode.Title, episode.Enclosure.Length, episode.Duration)

	type result struct{ label, exit, file string }
	runs := []result{
		{"direct #1 (NO)", home, "direct-1.mp3"},
		{"sweden #1 (SE)", "sweden", "sweden-1.mp3"},
		{"direct #2 (NO)", home, "direct-2.mp3"},
		{"sweden #2 (SE)", "sweden", "sweden-2.mp3"},
	}
	hashes := map[string]string{}
	for _, run := range runs {
		downloaded := fetch(t, manager, run.exit, episode.Enclosure.URL, filepath.Join(output, run.file))
		hashes[run.label] = downloaded.hash
		t.Logf("%-15s %9d bytes  sha256 %s…  %s  (%s)", run.label, len(downloaded.body), downloaded.hash[:16], downloaded.elapsed.Round(time.Millisecond), downloaded.contentType)
	}
	same := func(a, b string) string {
		if hashes[a] == hashes[b] {
			return "identical"
		}
		return "different"
	}
	t.Logf("NO vs NO: %s | SE vs SE: %s | NO vs SE: %s", same("direct #1 (NO)", "direct #2 (NO)"), same("sweden #1 (SE)", "sweden #2 (SE)"), same("direct #1 (NO)", "sweden #1 (SE)"))
	t.Logf("files kept in %s", output)
}

type fetched struct {
	body        []byte
	hash        string
	elapsed     time.Duration
	contentType string
}

func fetch(t *testing.T, manager *outbound.Manager, exit, url, saveAs string) fetched {
	t.Helper()
	client, err := manager.Client(exit)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	start := time.Now()
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s via %s: %v", shorten(url), exit, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("%s via %s: %s %v", shorten(url), exit, response.Status, err)
	}
	sum := sha256.Sum256(body)
	if saveAs != "" {
		os.WriteFile(saveAs, body, 0o644)
	}
	return fetched{body: body, hash: hex.EncodeToString(sum[:]), elapsed: time.Since(start), contentType: response.Header.Get("Content-Type")}
}

func shorten(url string) string {
	if i := strings.Index(url, "?"); i >= 0 {
		return url[:i]
	}
	return url
}

// TestLiveHandshakeTime measures how long the first handshake takes on a few
// servers, to size handshakeTimeout.
func TestLiveHandshakeTime(t *testing.T) {
	requireKeys(t, "PROTON_KEY_1")
	key, err := settings.ResolveSecret("env:PROTON_KEY_1", os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	private, err := ParseKey(key)
	if err != nil {
		t.Fatal(err)
	}
	servers, _ := protonServers(embeddedProtonList(), Provider{Tier: "plus", Filter: Filter{Countries: []string{"SE", "DE", "NL"}, SecureCore: "exclude", Tor: "exclude"}})
	for i, server := range servers {
		if i%7 != 0 || i > 40 {
			continue
		}
		server.PrivateKey = private
		opened, err := openTunnel(context.Background(), server, time.Now)
		if err != nil {
			t.Errorf("%s: %v", server.Name, err)
			continue
		}
		start := time.Now()
		original := handshakeTimeout
		handshakeTimeout = 15 * time.Second
		err = opened.awaitHandshake(context.Background())
		handshakeTimeout = original
		t.Logf("%-10s %s: handshake after %s (err %v)", server.Name, server.Location.Country, time.Since(start).Round(time.Millisecond), err)
		opened.close()
	}
}

// TestLiveFirstRequestTiming splits the first request into its steps. It is
// how the lost first DNS query was found (see dnsAttempts); a first lookup
// should now take well under two seconds.
func TestLiveFirstRequestTiming(t *testing.T) {
	requireKeys(t, "PROTON_KEY_1")
	key, _ := settings.ResolveSecret("env:PROTON_KEY_1", os.Getenv)
	private, _ := ParseKey(key)
	servers, _ := protonServers(embeddedProtonList(), Provider{Tier: "plus", Filter: Filter{Countries: []string{"SE"}, SecureCore: "exclude", Tor: "exclude"}})
	server := servers[0]
	server.PrivateKey = private
	opened, err := openTunnel(context.Background(), server, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.close()
	ctx := context.Background()
	step := func(label string, run func() error) {
		start := time.Now()
		err := run()
		t.Logf("%-28s %8s  err=%v", label, time.Since(start).Round(time.Millisecond), err)
	}
	step("handshake", func() error { return opened.awaitHandshake(ctx) })
	for _, host := range []string{"ifconfig.co", "ifconfig.co", "feeds.acast.com", "sphinx.acast.com"} {
		step("lookup "+host, func() error {
			addresses, err := opened.LookupIP(ctx, host)
			if err == nil {
				t.Logf("    %d addresses (IPv4: %d)", len(addresses), countV4(addresses))
			}
			return err
		})
	}
}

func countV4(addresses []netip.Addr) int {
	count := 0
	for _, address := range addresses {
		if address.Is4() {
			count++
		}
	}
	return count
}
