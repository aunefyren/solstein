package episodes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aunefyren/solstein/database"
	"aunefyren/solstein/logger"
	"aunefyren/solstein/models"
	"aunefyren/solstein/outbound"

	"github.com/sirupsen/logrus"
)

const audio = "ID3fake-mp3-audio-bytes"

// hits counts requests for /ok.mp3, to tell cache hits from source fetches.
var hits atomic.Int32

// startAudioHost serves the kinds of responses a podcast CDN can give.
func startAudioHost(t *testing.T) *httptest.Server {
	t.Helper()
	host := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/ok.mp3":
			hits.Add(1)
			writer.Header().Set("Content-Type", "audio/mpeg")
			http.ServeContent(writer, request, "", time.Time{}, strings.NewReader(audio)) // supports Range
		case "/untyped":
			writer.Write([]byte(audio)) // no Content-Type: sniffed as octet-stream
		case "/html.mp3":
			writer.Header().Set("Content-Type", "text/html; charset=utf-8")
			writer.Write([]byte("<html>Error</html>"))
		case "/missing.mp3":
			http.NotFound(writer, request)
		case "/broken.mp3":
			http.Error(writer, "upstream down", http.StatusBadGateway)
		case "/empty.mp3":
			writer.Header().Set("Content-Type", "audio/mpeg")
		case "/truncated.mp3":
			writer.Header().Set("Content-Type", "audio/mpeg")
			writer.Header().Set("Content-Length", "1000")
			writer.Write([]byte(audio))
		case "/stall.mp3":
			writer.Header().Set("Content-Type", "audio/mpeg")
			writer.Write([]byte(audio))
			writer.(http.Flusher).Flush()
			select {
			case <-request.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}
	}))
	t.Cleanup(host.Close)
	return host
}

type testClock struct {
	mutex sync.Mutex
	now   time.Time
}

func (clock *testClock) Now() time.Time {
	clock.mutex.Lock()
	defer clock.mutex.Unlock()
	return clock.now
}

func (clock *testClock) advance(duration time.Duration) {
	clock.mutex.Lock()
	defer clock.mutex.Unlock()
	clock.now = clock.now.Add(duration)
}

type testSetup struct {
	store    *database.Store
	cache    Cache
	pipeline *Pipeline
	clock    *testClock
	feed     models.Feed
	host     *httptest.Server
}

func newTestSetup(t *testing.T) *testSetup {
	t.Helper()
	original := logger.Log
	t.Cleanup(func() { logger.Log = original })
	logger.Log = logrus.New()
	logger.Log.SetOutput(&strings.Builder{})

	configDir := t.TempDir()
	store, err := database.Open(configDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	exits, err := outbound.New(outbound.Options{AllowPrivateDestinations: true}) // test host is on loopback
	if err != nil {
		t.Fatal(err)
	}
	cache, err := NewCache(filepath.Join(configDir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	clock := &testClock{now: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)}
	pipeline := NewPipeline(store, exits, cache, Options{DefaultDeliveryMode: "cache", Workers: 2, IdleTimeout: 200 * time.Millisecond, Now: clock.Now})

	feed := models.Feed{SourceURL: "https://example.com/feed", Title: "Show"}
	if err := store.CreateFeed(context.Background(), &feed); err != nil {
		t.Fatal(err)
	}
	return &testSetup{store: store, cache: cache, pipeline: pipeline, clock: clock, feed: feed, host: startAudioHost(t)}
}

func (setup *testSetup) addEpisode(t *testing.T, path string) models.Episode {
	t.Helper()
	episode := models.Episode{FeedID: setup.feed.ID, GUID: path, Title: path, SourceURL: setup.host.URL + path, State: models.EpisodeDiscovered}
	if err := setup.store.CreateEpisode(context.Background(), &episode); err != nil {
		t.Fatal(err)
	}
	return episode
}

func (setup *testSetup) reload(t *testing.T, episode models.Episode) models.Episode {
	t.Helper()
	stored, err := setup.store.GetEpisode(context.Background(), setup.feed.ID, episode.ID)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func (setup *testSetup) processOne(t *testing.T) bool {
	t.Helper()
	worked, err := setup.pipeline.ProcessNext(context.Background())
	if err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}
	return worked
}

func TestDownloadSuccess(t *testing.T) {
	setup := newTestSetup(t)
	for _, path := range []string{"/ok.mp3", "/untyped"} {
		episode := setup.addEpisode(t, path)
		if !setup.processOne(t) {
			t.Fatal("no work found")
		}
		stored := setup.reload(t, episode)
		if stored.State != models.EpisodeReady || stored.CacheSize != int64(len(audio)) || stored.CachedAt == nil || stored.Attempts != 1 {
			t.Fatalf("%s: episode = %+v", path, stored)
		}
		wantFile := setup.feed.ID.String() + "/" + episode.ID.String() + ".mp3"
		if stored.CacheFile != wantFile {
			t.Errorf("%s: cache file = %q, want %q", path, stored.CacheFile, wantFile)
		}
		fullPath, _ := setup.cache.Path(stored.CacheFile)
		data, err := os.ReadFile(fullPath)
		if err != nil || string(data) != audio {
			t.Errorf("%s: cached content = %q, %v", path, data, err)
		}
	}
	if setup.processOne(t) {
		t.Error("found work after everything was cached")
	}
	assertNoPartFiles(t, setup.cache)
}

func TestPermanentFailures(t *testing.T) {
	setup := newTestSetup(t)
	for _, path := range []string{"/html.mp3", "/missing.mp3"} {
		episode := setup.addEpisode(t, path)
		setup.processOne(t)
		stored := setup.reload(t, episode)
		if stored.State != models.EpisodeFailed || stored.LastError == "" || stored.CacheFile != "" {
			t.Errorf("%s: episode = %+v, want failed at once", path, stored)
		}
	}
	assertNoPartFiles(t, setup.cache)
}

func TestTemporaryFailuresRetryWithBackoff(t *testing.T) {
	setup := newTestSetup(t)
	episode := setup.addEpisode(t, "/broken.mp3")

	for attempt := 1; attempt <= len(retryDelays); attempt++ {
		if !setup.processOne(t) {
			t.Fatalf("attempt %d: no work found", attempt)
		}
		stored := setup.reload(t, episode)
		wantNext := setup.clock.Now().Add(retryDelays[attempt-1])
		if stored.State != models.EpisodeDiscovered || stored.Attempts != attempt || stored.NextAttemptAt == nil || !stored.NextAttemptAt.Equal(wantNext) {
			t.Fatalf("attempt %d: episode = %+v, want retry at %v", attempt, stored, wantNext)
		}
		if setup.processOne(t) {
			t.Fatalf("attempt %d: retried before its time", attempt)
		}
		setup.clock.advance(retryDelays[attempt-1])
	}

	// One more failure than there are delays: give up.
	setup.processOne(t)
	if stored := setup.reload(t, episode); stored.State != models.EpisodeFailed || stored.Attempts != len(retryDelays)+1 {
		t.Errorf("final: episode = %+v, want failed", stored)
	}
}

func TestIncompleteDownloadsRetry(t *testing.T) {
	setup := newTestSetup(t)
	for _, path := range []string{"/stall.mp3", "/truncated.mp3", "/empty.mp3"} {
		episode := setup.addEpisode(t, path)
		start := time.Now()
		setup.processOne(t)
		stored := setup.reload(t, episode)
		if stored.State != models.EpisodeDiscovered || stored.NextAttemptAt == nil || stored.CacheFile != "" {
			t.Errorf("%s: episode = %+v, want scheduled retry", path, stored)
		}
		if path == "/stall.mp3" {
			if !strings.Contains(stored.LastError, "no data") || time.Since(start) > 3*time.Second {
				t.Errorf("stall not caught by the idle timeout: %q after %s", stored.LastError, time.Since(start))
			}
		}
	}
	assertNoPartFiles(t, setup.cache)
}

func TestRecover(t *testing.T) {
	setup := newTestSetup(t)
	ctx := context.Background()
	episode := setup.addEpisode(t, "/ok.mp3")
	episode.State = models.EpisodeAcquiring
	if err := setup.store.UpdateEpisode(ctx, &episode); err != nil {
		t.Fatal(err)
	}
	file, _, _, err := setup.cache.create(setup.feed.ID, episode.ID, "mp3")
	if err != nil {
		t.Fatal(err)
	}
	file.Close()

	if err := setup.pipeline.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if stored := setup.reload(t, episode); stored.State != models.EpisodeDiscovered {
		t.Errorf("state = %s, want discovered", stored.State)
	}
	assertNoPartFiles(t, setup.cache)

	// And it downloads normally afterwards.
	setup.processOne(t)
	if stored := setup.reload(t, episode); stored.State != models.EpisodeReady {
		t.Errorf("after recovery: state = %s", stored.State)
	}
}

func TestRunProcessesWorkAndStops(t *testing.T) {
	setup := newTestSetup(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		setup.pipeline.Run(ctx)
		close(done)
	}()

	var episodes []models.Episode
	for i := range 3 {
		episodes = append(episodes, setup.addEpisode(t, "/ok.mp3?n="+strconv.Itoa(i)))
	}
	setup.pipeline.Wake()

	deadline := time.Now().Add(5 * time.Second)
	for _, episode := range episodes {
		for setup.reload(t, episode).State != models.EpisodeReady {
			if time.Now().After(deadline) {
				t.Fatalf("episode %s never became ready", episode.GUID)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestCachePath(t *testing.T) {
	cache := Cache{directory: filepath.Join("base", "cache")}
	if path, err := cache.Path("feed/episode.mp3"); err != nil || path != filepath.Join("base", "cache", "feed", "episode.mp3") {
		t.Errorf("Path = %q, %v", path, err)
	}
	for _, bad := range []string{"", "../config.json", "feed/../../config.json", "/etc/passwd", ".."} {
		if _, err := cache.Path(bad); err == nil {
			t.Errorf("Path(%q) accepted", bad)
		}
	}
}

func assertNoPartFiles(t *testing.T, cache Cache) {
	t.Helper()
	filepath.WalkDir(cache.directory, func(path string, entry os.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(path, partSuffix) {
			t.Errorf("unfinished file left behind: %s", path)
		}
		return nil
	})
}
