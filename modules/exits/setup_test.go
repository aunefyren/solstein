package exits

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aunefyren/solstein/database"
	"aunefyren/solstein/feeds"
	"aunefyren/solstein/outbound"
	"aunefyren/solstein/rss"
	"aunefyren/solstein/settings"
)

// writeConf writes the .conf a VPN provider would give for the test peer.
func writeConf(t *testing.T, path string, clientKey Key, peer wireGuardServer) {
	t.Helper()
	conf := fmt.Sprintf("[Interface]\nPrivateKey = %s\nAddress = %s/32\nDNS = %s\n\n[Peer]\nPublicKey = %s\nAllowedIPs = 0.0.0.0/0\nEndpoint = %s\n",
		encodeKey(clientKey), clientTunnelAddress, serverTunnelAddress, peer.publicKey, peer.endpoint)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSetupNothingConfigured(t *testing.T) {
	module, warnings := Setup(settings.VPN{}, t.TempDir(), testEnv(nil))
	if module != nil || len(warnings) != 0 {
		t.Errorf("empty vpn block: module %v, warnings %v", module, warnings)
	}
}

func TestSetupWarnings(t *testing.T) {
	quietLogs(t)
	configDir := t.TempDir()
	clientKey := generateKey(t)
	writeConf(t, filepath.Join(configDir, "wireguard", "se-sto-1.conf"), clientKey, wireGuardServer{publicKey: generateKey(t).Public(), endpoint: "198.51.100.7:51820"})
	os.WriteFile(filepath.Join(configDir, "wireguard", "broken.conf"), []byte("[Interface]\n"), 0o600)

	vpn := settings.VPN{
		Providers: map[string]settings.VPNProvider{
			"local":  {Type: "wireguard", ConfigDir: "wireguard", Servers: map[string]settings.VPNServerLocation{"se-sto-1": {Country: "SE"}}},
			"proton": {Type: "protonvpn", PrivateKeys: []string{testKeyA}},
		},
		Exits: map[string]settings.VPNExit{
			"sweden": {Provider: "local", Locations: []string{"SE"}},
			"japan":  {Provider: "local", Locations: []string{"JP"}, Strict: true},
			"proton": {Provider: "proton"},
		},
	}
	module, warnings := Setup(vpn, configDir, testEnv(nil))
	if module == nil {
		t.Fatalf("module not built; warnings: %v", warnings)
	}
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{
		"provider 'local': broken: [Interface] has no PrivateKey",
		"exit 'japan': no server of provider 'local' is in JP",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings lack %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "Proton servers from") {
		t.Error("where the Proton list came from is information, not a warning")
	}
	if got := strings.Join(module.Exits(), ","); got != "japan,proton,sweden" {
		t.Errorf("exits = %s (japan stays: servers may appear later)", got)
	}
	summary := module.Summary()
	if !strings.Contains(summary, "local (wireguard, 1 servers)") || !strings.Contains(summary, "Proton servers from the built-in list, dated 2026-08-06") {
		t.Errorf("summary = %s", summary)
	}
}

func TestSetupEverythingBroken(t *testing.T) {
	vpn := settings.VPN{Providers: map[string]settings.VPNProvider{"p": {Type: "openvpn"}}, Exits: map[string]settings.VPNExit{"e": {Provider: "p"}}}
	module, warnings := Setup(vpn, t.TempDir(), testEnv(nil))
	if module != nil || !strings.Contains(strings.Join(warnings, "\n"), "the VPN module is off") {
		t.Errorf("module %v, warnings %v", module, warnings)
	}
}

// TestFeedThroughVPNExit subscribes to a feed whose host exists only inside
// a WireGuard tunnel, the way a geo-blocked or region-specific feed would
// only be reachable through a VPN exit.
func TestFeedThroughVPNExit(t *testing.T) {
	quietLogs(t)
	configDir := t.TempDir()
	clientKey := generateKey(t)
	peer := startWireGuardServerWith(t, clientKey.Public(), http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fmt.Fprint(writer, `<?xml version="1.0"?><rss version="2.0"><channel><title>Tunnel Show</title>`+
			`<item><title>One</title><guid>t-1</guid><enclosure url="http://example.test/one.mp3" type="audio/mpeg" length="5"/></item>`+
			`</channel></rss>`)
	}))
	writeConf(t, filepath.Join(configDir, "wireguard", "vps.conf"), clientKey, peer)

	vpn := settings.VPN{
		Providers: map[string]settings.VPNProvider{"vps": {Type: "wireguard", ConfigFile: "wireguard/vps.conf", Country: "DE"}},
		Exits:     map[string]settings.VPNExit{"germany": {Provider: "vps", Locations: []string{"DE"}}},
	}
	module, warnings := Setup(vpn, configDir, testEnv(nil))
	if module == nil {
		t.Fatalf("no module: %v", warnings)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go module.Run(ctx)

	// The test peer's addresses are private, so private destinations are
	// allowed here; with a real VPN the sources are public.
	manager, err := outbound.New(outbound.Options{AllowPrivateDestinations: true, Providers: []outbound.Provider{module}})
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.Open(configDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := feeds.New(store, manager, feeds.Options{DefaultDeliveryMode: "cache"})

	subscribeCtx, cancelSubscribe := context.WithTimeout(ctx, 20*time.Second)
	defer cancelSubscribe()
	feed, created, err := service.Subscribe(subscribeCtx, "http://example.test/feed.xml", feeds.Settings{Exit: "germany"})
	if err != nil || !created || feed.Title != "Tunnel Show" {
		t.Fatalf("subscribe through the exit = %+v, %v, %v", feed, created, err)
	}
	if _, err := service.Refresh(subscribeCtx, &feed); err != nil {
		t.Errorf("poll through the exit: %v", err)
	}
	output, err := service.Render(subscribeCtx, feed, feeds.URLs{Base: "http://solstein", Unsigned: true})
	if parsed, _ := rss.Parse(output); err != nil || len(parsed.Items) != 1 {
		t.Errorf("render: %v", err)
	}

	// With the tunnel as the default exit and direct disabled, a feed that
	// names no exit goes through the tunnel, and direct is refused.
	noLeak, err := outbound.New(outbound.Options{AllowPrivateDestinations: true, Providers: []outbound.Provider{module}, DefaultExit: "germany", DisableDirect: true})
	if err != nil {
		t.Fatal(err)
	}
	noLeakService := feeds.New(store, noLeak, feeds.Options{DefaultDeliveryMode: "cache"})
	if feed, _, err := noLeakService.Subscribe(subscribeCtx, "http://example.test/default.xml", feeds.Settings{}); err != nil || feed.Title != "Tunnel Show" {
		t.Errorf("subscribe through the default exit = %+v, %v", feed, err)
	}
	if _, _, err := noLeakService.Subscribe(subscribeCtx, "http://example.test/direct.xml", feeds.Settings{Exit: "direct"}); !errors.Is(err, feeds.ErrInvalidSettings) || !errors.Is(err, outbound.ErrDirectDisabled) {
		t.Errorf("direct with disable_direct: err = %v", err)
	}

	// The same host doesn't exist outside the tunnel.
	if _, _, err := service.Subscribe(subscribeCtx, "http://example.test/other.xml", feeds.Settings{}); err == nil {
		t.Error("the tunnel-only host was reachable through direct")
	}
	// An exit that isn't configured is rejected up front.
	if _, _, err := service.Subscribe(subscribeCtx, "http://example.test/x.xml", feeds.Settings{Exit: "sweden"}); !errors.Is(err, feeds.ErrInvalidSettings) {
		t.Errorf("unknown exit: err = %v", err)
	}
}
