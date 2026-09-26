//go:build live

package exits

import (
	"context"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestLiveDNSProbe resolves NRK's hosts through a Proton exit and fetches
// the start of an episode, as production does: Proton's resolver fails on
// NRK's CDN host through US servers, and the fallback resolvers should
// answer instead. Temporary; see docs/wip.md.
//
//	LIVE_PROBE_EXIT=US go test -tags live -run LiveDNSProbe -v -count=1 ./modules/exits/
func TestLiveDNSProbe(t *testing.T) {
	requireKeys(t, "PROTON_KEY_1")
	country := os.Getenv("LIVE_PROBE_EXIT")
	if country == "" {
		country = "US"
	}
	manager, module := liveManager(t, []string{"env:PROTON_KEY_1"}, 0, map[string]string{"probe": country})
	dialer, err := module.Dialer("probe")
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"podkast.nrk.no", "nrk-pod-pd.telenorcdn.net", "feeds.acast.com"}
	const audio = "https://podkast.nrk.no/fil/loerdagsraadet/387fd908-2668-4f46-bfd9-0826686f466e_1_ID192MP3.mp3"
	client, err := manager.Client("probe")
	if err != nil {
		t.Fatal(err)
	}

	failures := 0
	for round := 1; round <= 4; round++ {
		for _, name := range names {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			start := time.Now()
			addresses, err := dialer.LookupIP(ctx, name)
			cancel()
			if err != nil {
				failures++
			}
			t.Logf("%s round %d: %-27s %6.2fs %d addresses, err %v", country, round, name, time.Since(start).Seconds(), len(addresses), err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, audio, nil)
		request.Header.Set("Range", "bytes=0-1023")
		start := time.Now()
		response, err := client.Do(request)
		if err != nil {
			failures++
			t.Logf("%s round %d: audio request failed after %.2fs: %v", country, round, time.Since(start).Seconds(), err)
		} else {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			t.Logf("%s round %d: audio request %s in %.2fs, from %s", country, round, response.Status, time.Since(start).Seconds(), response.Request.URL.Host)
		}
		cancel()
		time.Sleep(25 * time.Second) // let the short TTLs run out
	}
	if failures > 0 {
		t.Errorf("%d lookups or requests failed", failures)
	}
}

// TestLiveHTTPProbe fetches the start of an NRK episode repeatedly through a
// Proton exit, first on a new connection each time, then reusing one with
// short pauses, to tell a flaky path from stale reused connections.
func TestLiveHTTPProbe(t *testing.T) {
	requireKeys(t, "PROTON_KEY_1")
	country := os.Getenv("LIVE_PROBE_EXIT")
	if country == "" {
		country = "US"
	}
	manager, _ := liveManager(t, []string{"env:PROTON_KEY_1"}, 0, map[string]string{"probe": country})
	client, err := manager.Client("probe")
	if err != nil {
		t.Fatal(err)
	}
	const audio = "https://podkast.nrk.no/fil/loerdagsraadet/387fd908-2668-4f46-bfd9-0826686f466e_1_ID192MP3.mp3"
	for _, phase := range []struct {
		name  string
		fresh bool
		pause time.Duration
	}{{"new connection", true, 2 * time.Second}, {"reused, 5 s apart", false, 5 * time.Second}, {"reused, 20 s apart", false, 20 * time.Second}} {
		for i := 1; i <= 6; i++ {
			if phase.fresh {
				client.CloseIdleConnections()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			request, _ := http.NewRequestWithContext(ctx, http.MethodGet, audio, nil)
			request.Header.Set("Range", "bytes=0-65535")
			start := time.Now()
			response, err := client.Do(request)
			if err != nil {
				t.Logf("%s %s #%d: failed after %.2fs: %q", country, phase.name, i, time.Since(start).Seconds(), err.Error())
			} else {
				n, readErr := io.Copy(io.Discard, response.Body)
				response.Body.Close()
				t.Logf("%s %s #%d: %s, %d bytes in %.2fs (%s, %s), read err %v", country, phase.name, i, response.Status, n, time.Since(start).Seconds(), response.Proto, response.Request.URL.Host, readErr)
			}
			cancel()
			time.Sleep(phase.pause)
		}
	}
}
