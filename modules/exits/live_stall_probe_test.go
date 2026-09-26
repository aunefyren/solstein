//go:build live

package exits

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"aunefyren/solstein/outbound"
	"aunefyren/solstein/settings"
)

// TestLiveStallProbe looks for whole-tunnel stalls through several Proton
// servers: one small request a second, with the tunnel's handshake time and
// byte counters sampled every 250 ms, so each stall can be matched to a
// re-handshake, lost replies, or nothing leaving at all. Temporary; see
// docs/wip.md.
//
//	LIVE_PROBE_MINUTES=4 go test -tags live -run LiveStallProbe -v -count=1 -timeout 60m ./modules/exits/
func TestLiveStallProbe(t *testing.T) {
	// LIVE_PROBE_KEY picks the key (default PROTON_KEY_1); LIVE_PROBE_SERVERS
	// (comma-separated) the servers, by default the one production used, two
	// other US ones in other states, and NO#23.
	keyName := os.Getenv("LIVE_PROBE_KEY")
	if keyName == "" {
		keyName = "PROTON_KEY_1"
	}
	requireKeys(t, keyName)
	minutes := 4
	if value, err := strconv.Atoi(os.Getenv("LIVE_PROBE_MINUTES")); err == nil && value > 0 {
		minutes = value
	}

	var servers []string
	if list := os.Getenv("LIVE_PROBE_SERVERS"); list != "" {
		servers = strings.Split(list, ",")
	} else {
		_, picker := liveManager(t, []string{"env:" + keyName}, 0, map[string]string{"us": "US"})
		servers = []string{"US-AZ#108"}
		states := map[string]bool{"AZ": true}
		for _, name := range picker.ServerNames("proton") {
			state, _, ok := strings.Cut(strings.TrimPrefix(name, "US-"), "#")
			if ok && !states[state] && !strings.Contains(name, "@") && len(servers) < 3 {
				states[state] = true
				servers = append(servers, name)
			}
		}
		servers = append(servers, "NO#23")
	}
	t.Logf("servers: %v, %d minutes each, key %s", servers, minutes, keyName)

	exits := map[string]settings.VPNExit{}
	for i, server := range servers {
		exits[fmt.Sprintf("s%d", i)] = settings.VPNExit{Provider: "proton", Locations: []string{"server:" + server}, Strict: true}
	}
	vpn := settings.VPN{
		Providers: map[string]settings.VPNProvider{"proton": {
			Type: "protonvpn", PrivateKeys: []string{"env:" + keyName},
			Filter: &settings.VPNServerFilter{Countries: []string{"US", "NO"}},
		}},
		Exits: exits,
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

	for i, server := range servers {
		probeServer(t, manager, module, fmt.Sprintf("s%d", i), server, time.Duration(minutes)*time.Minute)
	}
}

// TestLiveKeySharing runs tunnels to two US servers at once, sharing one
// key (LIVE_PROBE_KEYS=1) or with a key each (2), to see whether a key's
// session moves to whichever server handshook last.
func TestLiveKeySharing(t *testing.T) {
	requireKeys(t, "PROTON_KEY_1", "PROTON_KEY_2")
	keys := []string{"env:PROTON_KEY_1", "env:PROTON_KEY_2"}
	if os.Getenv("LIVE_PROBE_KEYS") == "1" {
		keys = keys[:1]
	}
	minutes := 4
	if value, err := strconv.Atoi(os.Getenv("LIVE_PROBE_MINUTES")); err == nil && value > 0 {
		minutes = value
	}
	servers := []string{"US-CO#147", "US-CA#1057"}
	vpn := settings.VPN{
		Providers: map[string]settings.VPNProvider{"proton": {
			Type: "protonvpn", PrivateKeys: keys, MaxTunnels: 2,
			Filter: &settings.VPNServerFilter{Countries: []string{"US"}},
		}},
		Exits: map[string]settings.VPNExit{
			"a": {Provider: "proton", Locations: []string{"server:" + servers[0]}, Strict: true},
			"b": {Provider: "proton", Locations: []string{"server:" + servers[1]}, Strict: true},
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
	t.Logf("%d key(s) for 2 tunnels, %d minutes", len(keys), minutes)
	var wait sync.WaitGroup
	for i, exit := range []string{"a", "b"} {
		wait.Go(func() { probeServer(t, manager, module, exit, servers[i], time.Duration(minutes)*time.Minute) })
	}
	wait.Wait()
}

type stallSample struct {
	at        time.Time
	handshake time.Time
	tx, rx    int64
}

type stallRequest struct {
	start    time.Time
	duration time.Duration
	err      error
}

func probeServer(t *testing.T, manager *outbound.Manager, module *Module, exit, server string, length time.Duration) {
	const target = "https://nrk-pod-pd.telenorcdn.net/podkast/podcastpublisher_prod/loerdagsraadet/387fd908-2668-4f46-bfd9-0826686f466e_1_ID192MP3.mp3"
	client, err := manager.Client(exit)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections() // frees the tunnel for the next server

	var mutex sync.Mutex
	var samples []stallSample
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
			if sample, ok := sampleTunnel(module, server); ok {
				mutex.Lock()
				samples = append(samples, sample)
				mutex.Unlock()
			}
		}
	}()

	var requests []stallRequest
	begin := time.Now()
	for time.Since(begin) < length {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		request.Header.Set("Range", "bytes=0-4095")
		start := time.Now()
		response, err := client.Do(request)
		if err == nil {
			_, err = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if err == nil && response.StatusCode != http.StatusPartialContent {
				err = fmt.Errorf("status %s", response.Status)
			}
		}
		cancel()
		requests = append(requests, stallRequest{start: start, duration: time.Since(start), err: err})
		if wait := time.Second - time.Since(start); wait > 0 {
			time.Sleep(wait)
		}
	}
	close(stop)
	<-sampled

	// Handshakes seen, as offsets from the start.
	var handshakes []string
	var last time.Time
	for _, sample := range samples {
		if !sample.handshake.Equal(last) && !sample.handshake.IsZero() {
			handshakes = append(handshakes, offset(begin, sample.handshake))
			last = sample.handshake
		}
	}

	slow, failed := 0, 0
	for _, request := range requests {
		if request.err == nil && request.duration < 3*time.Second {
			continue
		}
		if request.err != nil {
			failed++
		} else {
			slow++
		}
		// The counters over the request, and the handshake around it.
		first, final, ok := window(samples, request.start, request.start.Add(request.duration))
		detail := "no samples"
		if ok {
			detail = fmt.Sprintf("tx +%d B, rx +%d B; handshake at start %s, at end %s",
				final.tx-first.tx, final.rx-first.rx, offset(begin, first.handshake), offset(begin, final.handshake))
		}
		message := "ok"
		if request.err != nil {
			message = request.err.Error()
			if len(message) > 140 {
				message = message[:140] + "…"
			}
		}
		t.Logf("%s (%s) stall at %s for %.1fs: %s — %s", exit, server, offset(begin, request.start), request.duration.Seconds(), message, detail)
	}
	t.Logf("%s (%s): %d requests, %d slow (≥3 s), %d failed; handshakes at %s", exit, server, len(requests), slow, failed, strings.Join(handshakes, ", "))
}

// sampleTunnel reads the handshake time and byte counters of the tunnel to
// a server.
func sampleTunnel(module *Module, server string) (stallSample, bool) {
	pool := module.pools["proton"]
	pool.mutex.Lock()
	open := pool.tunnels[server]
	pool.mutex.Unlock()
	if open == nil || open.device == nil {
		return stallSample{}, false
	}
	state, err := open.device.IpcGet()
	if err != nil {
		return stallSample{}, false
	}
	sample := stallSample{at: time.Now()}
	var seconds, nanoseconds int64
	for _, line := range strings.Split(state, "\n") {
		key, value, _ := strings.Cut(line, "=")
		number, _ := strconv.ParseInt(value, 10, 64)
		switch key {
		case "last_handshake_time_sec":
			seconds = number
		case "last_handshake_time_nsec":
			nanoseconds = number
		case "tx_bytes":
			sample.tx = number
		case "rx_bytes":
			sample.rx = number
		}
	}
	if seconds > 0 {
		sample.handshake = time.Unix(seconds, nanoseconds)
	}
	return sample, true
}

// window returns the samples nearest the start and end of a span.
func window(samples []stallSample, from, to time.Time) (first, last stallSample, ok bool) {
	for _, sample := range samples {
		if sample.at.Before(from) || !ok {
			first, ok = sample, true
		}
		if !sample.at.After(to) {
			last = sample
		}
	}
	return first, last, ok
}

func offset(begin, at time.Time) string {
	if at.IsZero() {
		return "never"
	}
	return fmt.Sprintf("%+.1fs", at.Sub(begin).Seconds())
}
