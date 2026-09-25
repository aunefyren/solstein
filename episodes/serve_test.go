package episodes

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"aunefyren/solstein/database"
	"aunefyren/solstein/feeds"
	"aunefyren/solstein/models"
	"aunefyren/solstein/outbound"
)

func newTestServer(t *testing.T, setup *testSetup) *Server {
	t.Helper()
	exits, err := outbound.New(outbound.Options{AllowPrivateDestinations: true})
	if err != nil {
		t.Fatal(err)
	}
	feedService := feeds.New(setup.store, exits, feeds.Options{DefaultDeliveryMode: "cache"})
	return NewServer(setup.store, exits, setup.cache, feedService, setup.pipeline, Options{Now: setup.clock.Now})
}

// serve runs one request and returns the recorder and Serve's error.
func serve(t *testing.T, server *Server, episode models.Episode, method, rangeHeader string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	request := httptest.NewRequest(method, "/episode", nil)
	if rangeHeader != "" {
		request.Header.Set("Range", rangeHeader)
	}
	recorder := httptest.NewRecorder()
	err := server.Serve(recorder, request, episode.FeedID, episode.ID)
	return recorder, err
}

// addBacklog stores a backlog episode (ready, uncached, fetched on demand).
func (setup *testSetup) addBacklog(t *testing.T, path string) models.Episode {
	t.Helper()
	episode := models.Episode{FeedID: setup.feed.ID, GUID: "backlog" + path, Title: "Backlog", SourceURL: setup.host.URL + path, State: models.EpisodeReady, Backlog: true}
	if err := setup.store.CreateEpisode(context.Background(), &episode); err != nil {
		t.Fatal(err)
	}
	return episode
}

func TestServeFromCache(t *testing.T) {
	setup := newTestSetup(t)
	server := newTestServer(t, setup)
	episode := setup.addEpisode(t, "/ok.mp3")
	setup.processOne(t) // downloads into the cache
	before := hits.Load()

	recorder, err := serve(t, server, episode, http.MethodGet, "")
	if err != nil || recorder.Code != http.StatusOK || recorder.Body.String() != audio {
		t.Fatalf("full: %d %q %v", recorder.Code, recorder.Body, err)
	}
	if recorder.Header().Get("Content-Type") != "audio/mpeg" || recorder.Header().Get("Accept-Ranges") != "bytes" {
		t.Errorf("headers = %v", recorder.Header())
	}

	recorder, err = serve(t, server, episode, http.MethodGet, "bytes=0-3")
	if err != nil || recorder.Code != http.StatusPartialContent || recorder.Body.String() != audio[:4] {
		t.Errorf("range: %d %q %v", recorder.Code, recorder.Body, err)
	}

	recorder, err = serve(t, server, episode, http.MethodHead, "")
	if err != nil || recorder.Code != http.StatusOK || recorder.Body.Len() != 0 || recorder.Header().Get("Content-Length") == "" {
		t.Errorf("head: %d %q %v %v", recorder.Code, recorder.Body, err, recorder.Header())
	}

	if hits.Load() != before {
		t.Error("cached episode was fetched from the source")
	}
}

func TestServeBacklogStreamsAndCaches(t *testing.T) {
	setup := newTestSetup(t)
	server := newTestServer(t, setup)
	episode := setup.addBacklog(t, "/ok.mp3")

	recorder, err := serve(t, server, episode, http.MethodGet, "")
	if err != nil || recorder.Code != http.StatusOK || recorder.Body.String() != audio {
		t.Fatalf("stream: %d %q %v", recorder.Code, recorder.Body, err)
	}
	stored := setup.reload(t, episode)
	if stored.CacheFile == "" || stored.CacheSize != int64(len(audio)) {
		t.Fatalf("not cached while streaming: %+v", stored)
	}

	// The next play comes from the cache.
	before := hits.Load()
	if recorder, _ := serve(t, server, episode, http.MethodGet, ""); recorder.Body.String() != audio {
		t.Errorf("second play = %q", recorder.Body)
	}
	if hits.Load() != before {
		t.Error("second play went to the source")
	}
	assertNoPartFiles(t, setup.cache)
}

func TestServeRangeIsForwardedWithoutCaching(t *testing.T) {
	setup := newTestSetup(t)
	server := newTestServer(t, setup)
	episode := setup.addBacklog(t, "/ok.mp3")

	recorder, err := serve(t, server, episode, http.MethodGet, "bytes=3-6")
	if err != nil || recorder.Code != http.StatusPartialContent || recorder.Body.String() != audio[3:7] {
		t.Fatalf("range: %d %q %v", recorder.Code, recorder.Body, err)
	}
	if recorder.Header().Get("Content-Range") == "" {
		t.Error("Content-Range not forwarded")
	}
	if stored := setup.reload(t, episode); stored.CacheFile != "" {
		t.Error("a partial response was cached")
	}
}

func TestServeStreamModeDoesNotCache(t *testing.T) {
	setup := newTestSetup(t)
	setup.feed.DeliveryMode = "stream"
	if err := setup.store.UpdateFeed(context.Background(), &setup.feed); err != nil {
		t.Fatal(err)
	}
	server := newTestServer(t, setup)
	episode := setup.addBacklog(t, "/ok.mp3")

	recorder, err := serve(t, server, episode, http.MethodGet, "")
	if err != nil || recorder.Body.String() != audio {
		t.Fatalf("stream: %d %q %v", recorder.Code, recorder.Body, err)
	}
	if stored := setup.reload(t, episode); stored.CacheFile != "" {
		t.Error("stream mode wrote to the cache")
	}
}

func TestServeFailedEpisodeRecoversByCaching(t *testing.T) {
	setup := newTestSetup(t)
	server := newTestServer(t, setup)
	episode := setup.addEpisode(t, "/ok.mp3")
	episode.State, episode.LastError = models.EpisodeFailed, "earlier failure"
	if err := setup.store.UpdateEpisode(context.Background(), &episode); err != nil {
		t.Fatal(err)
	}

	if _, err := serve(t, server, episode, http.MethodGet, ""); err != nil {
		t.Fatal(err)
	}
	stored := setup.reload(t, episode)
	if stored.State != models.EpisodeReady || stored.CacheFile == "" || stored.LastError != "" {
		t.Errorf("failed episode not recovered: %+v", stored)
	}
}

func TestServeMissingCacheFileFallsBack(t *testing.T) {
	setup := newTestSetup(t)
	server := newTestServer(t, setup)
	episode := setup.addEpisode(t, "/ok.mp3")
	setup.processOne(t)
	stored := setup.reload(t, episode)
	fullPath, _ := setup.cache.Path(stored.CacheFile)
	if err := os.Remove(fullPath); err != nil {
		t.Fatal(err)
	}

	recorder, err := serve(t, server, episode, http.MethodGet, "")
	if err != nil || recorder.Body.String() != audio {
		t.Fatalf("fallback: %d %q %v", recorder.Code, recorder.Body, err)
	}
	if _, err := os.Stat(fullPath); err != nil {
		t.Errorf("cache not rebuilt while streaming: %v", err)
	}
}

func TestServeOriginalModeRedirects(t *testing.T) {
	setup := newTestSetup(t)
	setup.feed.DeliveryMode = "original"
	if err := setup.store.UpdateFeed(context.Background(), &setup.feed); err != nil {
		t.Fatal(err)
	}
	server := newTestServer(t, setup)
	episode := setup.addBacklog(t, "/ok.mp3")

	recorder, err := serve(t, server, episode, http.MethodGet, "")
	if err != nil || recorder.Code != http.StatusFound || recorder.Header().Get("Location") != episode.SourceURL {
		t.Errorf("redirect: %d %v %v", recorder.Code, recorder.Header(), err)
	}
}

func TestServeErrors(t *testing.T) {
	setup := newTestSetup(t)
	server := newTestServer(t, setup)

	for _, path := range []string{"/missing.mp3", "/html.mp3", "/broken.mp3"} {
		episode := setup.addBacklog(t, path)
		recorder, err := serve(t, server, episode, http.MethodGet, "")
		if !errors.Is(err, ErrSourceFailed) {
			t.Errorf("%s: err = %v, want ErrSourceFailed", path, err)
		}
		if recorder.Body.Len() != 0 {
			t.Errorf("%s: something was written before the error: %q", path, recorder.Body)
		}
	}

	unknown := models.Episode{Base: models.Base{ID: setup.feed.ID}, FeedID: setup.feed.ID}
	if _, err := serve(t, server, unknown, http.MethodGet, ""); !errors.Is(err, database.ErrEpisodeNotFound) {
		t.Errorf("unknown episode: err = %v", err)
	}
}

// failingWriter stands in for a listener who disconnects after a few bytes.
type failingWriter struct {
	header  http.Header
	written int
}

func (writer *failingWriter) Header() http.Header { return writer.header }
func (writer *failingWriter) WriteHeader(int)     {}
func (writer *failingWriter) Write(data []byte) (int, error) {
	if writer.written >= 3 {
		return 0, errors.New("connection reset by peer")
	}
	writer.written += len(data)
	return len(data), nil
}

func TestServeCacheSurvivesListenerLeaving(t *testing.T) {
	setup := newTestSetup(t)
	server := newTestServer(t, setup)
	episode := setup.addBacklog(t, "/ok.mp3")

	request := httptest.NewRequest(http.MethodGet, "/episode", nil)
	writer := &failingWriter{header: http.Header{}}
	if err := server.Serve(writer, request, episode.FeedID, episode.ID); err != nil {
		t.Fatal(err)
	}
	stored := setup.reload(t, episode)
	if stored.CacheSize != int64(len(audio)) {
		t.Fatalf("cache not completed after the listener left: %+v", stored)
	}
	fullPath, _ := setup.cache.Path(stored.CacheFile)
	if data, _ := os.ReadFile(fullPath); string(data) != audio {
		t.Errorf("cached content = %q", data)
	}
}

func TestOnlyOneTeePerEpisode(t *testing.T) {
	setup := newTestSetup(t)
	server := newTestServer(t, setup)
	episode := setup.addBacklog(t, "/ok.mp3")

	if !server.startTee(episode.ID) {
		t.Fatal("first tee refused")
	}
	if server.startTee(episode.ID) {
		t.Error("second tee for the same episode allowed")
	}

	// While one tee runs, another listener still gets the audio, uncached.
	recorder, err := serve(t, server, episode, http.MethodGet, "")
	if err != nil || recorder.Body.String() != audio {
		t.Errorf("second listener: %q %v", recorder.Body, err)
	}
	if stored := setup.reload(t, episode); stored.CacheFile != "" {
		t.Error("second listener started its own cache copy")
	}

	server.endTee(episode.ID)
	if !server.startTee(episode.ID) {
		t.Error("tee not allowed after the first ended")
	}
}
