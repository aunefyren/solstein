//go:build live

package exits

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"aunefyren/solstein/outbound"
	"aunefyren/solstein/settings"
)

// TestLiveKeyMoveSettleRepeat repeats the same pause several times, for one
// pause length, to see whether keyMoveSettle's noise (docs/wip.md, Proton
// keys and stalls) is a real, if small, per-pause failure rate or one-off
// chance: the original measurement ran each pause once. One key moves
// between the same two servers as before (NO#23, DE#187) — briefly on
// DE#187 to move the key there, a pause, then probing NO#23 for the
// failures a stalled tunnel would show. Temporary; see docs/wip.md.
//
//	LIVE_REPEAT_PAUSE=2m LIVE_REPEAT_COUNT=5 go test -tags live -run LiveKeyMoveSettleRepeat -v -count=1 -timeout 60m ./modules/exits/
func TestLiveKeyMoveSettleRepeat(t *testing.T) {
	keyName := os.Getenv("LIVE_PROBE_KEY")
	if keyName == "" {
		keyName = "PROTON_KEY_1"
	}
	requireKeys(t, keyName)

	pause := 2 * time.Minute
	if value, err := time.ParseDuration(os.Getenv("LIVE_REPEAT_PAUSE")); err == nil {
		pause = value
	}
	repeats := 5
	if value, err := strconv.Atoi(os.Getenv("LIVE_REPEAT_COUNT")); err == nil && value > 0 {
		repeats = value
	}
	awaySeconds := 20
	if value, err := strconv.Atoi(os.Getenv("LIVE_REPEAT_AWAY_SECONDS")); err == nil && value > 0 {
		awaySeconds = value
	}
	probeSeconds := 120
	if value, err := strconv.Atoi(os.Getenv("LIVE_REPEAT_PROBE_SECONDS")); err == nil && value > 0 {
		probeSeconds = value
	}
	t.Logf("pause %s, %d repeats, %ds on DE#187 then %ds probing NO#23, key %s", pause, repeats, awaySeconds, probeSeconds, keyName)

	vpn := settings.VPN{
		Providers: map[string]settings.VPNProvider{"proton": {
			Type: "protonvpn", PrivateKeys: []string{"env:" + keyName},
			Filter: &settings.VPNServerFilter{Countries: []string{"NO", "DE"}},
		}},
		Exits: map[string]settings.VPNExit{
			"home": {Provider: "proton", Locations: []string{"server:NO#23"}, Strict: true},
			"away": {Provider: "proton", Locations: []string{"server:DE#187"}, Strict: true},
		},
	}
	module, warnings := Setup(vpn, t.TempDir(), os.Getenv)
	for _, warning := range warnings {
		t.Log("setup warning:", warning)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { module.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	manager, err := outbound.New(outbound.Options{UserAgent: "Solstein/live-test (+https://github.com/aunefyren/solstein)", Providers: []outbound.Provider{module}})
	if err != nil {
		t.Fatal(err)
	}

	totalRequests, totalFailed := 0, 0
	perRepeat := make([]int, repeats)
	for repeat := range repeats {
		// Move the key to DE#187.
		awayRequests, awayFailed := probeCounting(t, manager, "away", awaySeconds)
		t.Logf("repeat %d: %d requests on away, %d failed (moving the key there)", repeat+1, awayRequests, awayFailed)

		time.Sleep(pause)

		// Probe NO#23 right after the key moves back.
		requests, failed := probeCounting(t, manager, "home", probeSeconds)
		totalRequests += requests
		totalFailed += failed
		perRepeat[repeat] = failed
		t.Logf("repeat %d: pause %s, %d requests on home, %d failed", repeat+1, pause, requests, failed)
	}
	t.Logf("SUMMARY: pause %s, %d repeats, %d/%d failed on home after the move back; per repeat: %v", pause, repeats, totalFailed, totalRequests, perRepeat)
}

// probeCounting sends one small request a second through exit for length
// seconds, and reports how many were sent and how many failed (an error, or
// not a 206).
func probeCounting(t *testing.T, manager *outbound.Manager, exit string, length int) (requests, failed int) {
	t.Helper()
	const target = "https://nrk-pod-pd.telenorcdn.net/podkast/podcastpublisher_prod/loerdagsraadet/387fd908-2668-4f46-bfd9-0826686f466e_1_ID192MP3.mp3"
	client, err := manager.Client(exit)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections() // frees the tunnel for the next exit

	begin := time.Now()
	for time.Since(begin) < time.Duration(length)*time.Second {
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		request.Header.Set("Range", "bytes=0-4095")
		response, err := client.Do(request)
		if err == nil {
			_, err = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if err == nil && response.StatusCode != http.StatusPartialContent {
				err = fmt.Errorf("status %s", response.Status)
			}
		}
		cancel()
		requests++
		if err != nil {
			failed++
		}
		if wait := time.Second - time.Since(start); wait > 0 {
			time.Sleep(wait)
		}
	}
	return requests, failed
}
