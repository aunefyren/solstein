package feeds

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aunefyren/solstein/database"
	"aunefyren/solstein/models"
	"aunefyren/solstein/outbound"
	"aunefyren/solstein/rss"
	"aunefyren/solstein/signing"
)

// fakeHost plays a podcast host serving one feed whose items can be changed.
type fakeHost struct {
	server   *httptest.Server
	requests atomic.Int32

	mutex  sync.Mutex
	items  []string // item XML
	status int
	body   string // overrides the feed when set
	etag   string
}

func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	host := &fakeHost{status: http.StatusOK}
	host.server = httptest.NewServer(http.HandlerFunc(host.serve))
	t.Cleanup(host.server.Close)
	host.addItem("ep-1", "Mon, 21 Sep 2026 06:00:00 +0000")
	return host
}

func (host *fakeHost) addItem(guid, pubDate string) {
	host.mutex.Lock()
	defer host.mutex.Unlock()
	host.items = append([]string{fmt.Sprintf(
		`<item><title>%s</title><guid>%s</guid><pubDate>%s</pubDate><enclosure url="https://media.example.com/%s.mp3" type="audio/mpeg" length="100"/></item>`,
		guid, guid, pubDate, guid)}, host.items...)
}

func (host *fakeHost) serve(writer http.ResponseWriter, request *http.Request) {
	host.requests.Add(1)
	host.mutex.Lock()
	defer host.mutex.Unlock()
	if host.etag != "" {
		if request.Header.Get("If-None-Match") == host.etag {
			writer.WriteHeader(http.StatusNotModified)
			return
		}
		writer.Header().Set("ETag", host.etag)
	}
	writer.WriteHeader(host.status)
	if host.body != "" {
		fmt.Fprint(writer, host.body)
		return
	}
	fmt.Fprintf(writer, `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:atom="http://www.w3.org/2005/Atom"><channel><title>Fake Show</title>
<atom:link href="%s" rel="self"/>
%s
</channel></rss>`, host.server.URL, strings.Join(host.items, "\n"))
}

func (host *fakeHost) set(change func(host *fakeHost)) {
	host.mutex.Lock()
	defer host.mutex.Unlock()
	change(host)
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

func newTestService(t *testing.T, options Options) (*Service, *database.Store) {
	t.Helper()
	store, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	// The fake host listens on loopback.
	exits, err := outbound.New(outbound.Options{AllowPrivateDestinations: true})
	if err != nil {
		t.Fatal(err)
	}
	if options.DefaultDeliveryMode == "" {
		options.DefaultDeliveryMode = "cache"
	}
	if options.Now == nil {
		clock := &testClock{now: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)}
		options.Now = clock.Now
	}
	return New(store, exits, options), store
}

var testURLs = URLs{Base: "https://solstein.example.com", Signer: signing.New("key")}

func TestSubscribe(t *testing.T) {
	host := newFakeHost(t)
	service, store := newTestService(t, Options{})
	ctx := context.Background()

	feed, created, err := service.Subscribe(ctx, host.server.URL, Settings{})
	if err != nil || !created {
		t.Fatalf("Subscribe = %+v, %v, %v", feed, created, err)
	}
	if feed.Title != "Fake Show" || feed.LastSuccessAt == nil {
		t.Errorf("feed = %+v", feed)
	}
	episodes, _ := store.ListEpisodes(ctx, feed.ID)
	if len(episodes) != 1 || !episodes[0].Backlog || episodes[0].State != models.EpisodeReady {
		t.Errorf("initial episodes should be ready backlog: %+v", episodes)
	}

	// Subscribing again, even with a differently written URL, returns the
	// same feed without fetching.
	requests := host.requests.Load()
	again, created, err := service.Subscribe(ctx, strings.Replace(host.server.URL, "http://", "HTTP://", 1)+"#x", Settings{})
	if err != nil || created || again.ID != feed.ID {
		t.Errorf("second Subscribe = %+v, %v, %v", again, created, err)
	}
	if host.requests.Load() != requests {
		t.Error("existing subscription fetched the source again")
	}
}

func TestSubscribeRejects(t *testing.T) {
	ctx := context.Background()

	t.Run("not RSS", func(t *testing.T) {
		host := newFakeHost(t)
		host.set(func(host *fakeHost) { host.body = "<html><body>Not a feed</body></html>" })
		service, store := newTestService(t, Options{})
		if _, _, err := service.Subscribe(ctx, host.server.URL, Settings{}); !errors.Is(err, ErrFetchFailed) || !errors.Is(err, rss.ErrNotRSS) {
			t.Errorf("err = %v, want ErrFetchFailed wrapping ErrNotRSS", err)
		}
		if feeds, _ := store.ListFeeds(ctx); len(feeds) != 0 {
			t.Error("invalid feed was stored")
		}
	})

	t.Run("source error", func(t *testing.T) {
		host := newFakeHost(t)
		host.set(func(host *fakeHost) { host.status = http.StatusNotFound })
		service, _ := newTestService(t, Options{})
		if _, _, err := service.Subscribe(ctx, host.server.URL, Settings{}); !errors.Is(err, ErrFetchFailed) {
			t.Errorf("err = %v, want ErrFetchFailed", err)
		}
	})

	t.Run("host not allowed", func(t *testing.T) {
		service, _ := newTestService(t, Options{AllowedSourceHosts: []string{"acast.com"}})
		if _, _, err := service.Subscribe(ctx, "https://example.com/feed", Settings{}); !errors.Is(err, ErrSourceNotAllowed) {
			t.Errorf("err = %v, want ErrSourceNotAllowed", err)
		}
	})

	t.Run("invalid URL", func(t *testing.T) {
		service, _ := newTestService(t, Options{})
		if _, _, err := service.Subscribe(ctx, "ftp://example.com/feed", Settings{}); !errors.Is(err, ErrInvalidSourceURL) {
			t.Errorf("err = %v, want ErrInvalidSourceURL", err)
		}
	})

	t.Run("bad settings", func(t *testing.T) {
		host := newFakeHost(t)
		service, _ := newTestService(t, Options{})
		for _, feedSettings := range []Settings{{Exit: "nowhere"}, {DeliveryMode: "fax"}, {PollIntervalMinutes: -1}} {
			if _, _, err := service.Subscribe(ctx, host.server.URL, feedSettings); !errors.Is(err, ErrInvalidSettings) {
				t.Errorf("%+v: err = %v, want ErrInvalidSettings", feedSettings, err)
			}
		}
	})
}

func TestRefresh(t *testing.T) {
	host := newFakeHost(t)
	service, store := newTestService(t, Options{})
	ctx := context.Background()
	feed, _, err := service.Subscribe(ctx, host.server.URL, Settings{})
	if err != nil {
		t.Fatal(err)
	}

	host.addItem("ep-2", "Tue, 22 Sep 2026 06:00:00 +0000")
	added, err := service.Refresh(ctx, &feed)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if len(added) != 1 || added[0].GUID != "ep-2" || added[0].Backlog || added[0].State != models.EpisodeDiscovered {
		t.Errorf("added = %+v, want ep-2 discovered, not backlog", added)
	}

	// Nothing new: nothing added, existing episodes untouched.
	added, err = service.Refresh(ctx, &feed)
	if err != nil || len(added) != 0 {
		t.Errorf("second Refresh = %+v, %v", added, err)
	}
	if episodes, _ := store.ListEpisodes(ctx, feed.ID); len(episodes) != 2 {
		t.Errorf("episodes = %d, want 2", len(episodes))
	}
}

func TestRefreshStreamModeIsReadyAtOnce(t *testing.T) {
	host := newFakeHost(t)
	service, _ := newTestService(t, Options{DefaultDeliveryMode: "stream"})
	ctx := context.Background()
	feed, _, _ := service.Subscribe(ctx, host.server.URL, Settings{})

	host.addItem("ep-2", "Tue, 22 Sep 2026 06:00:00 +0000")
	added, err := service.Refresh(ctx, &feed)
	if err != nil || len(added) != 1 || added[0].State != models.EpisodeReady {
		t.Errorf("added = %+v, %v; want ep-2 ready", added, err)
	}
}

func TestRefreshConditional(t *testing.T) {
	host := newFakeHost(t)
	host.set(func(host *fakeHost) { host.etag = `"v1"` })
	service, _ := newTestService(t, Options{})
	ctx := context.Background()
	feed, _, _ := service.Subscribe(ctx, host.server.URL, Settings{})
	if feed.ETag != `"v1"` {
		t.Fatalf("ETag not stored: %q", feed.ETag)
	}

	added, err := service.Refresh(ctx, &feed)
	if err != nil || added != nil {
		t.Errorf("not-modified Refresh = %+v, %v", added, err)
	}
	if feed.LastError != "" || feed.LastSuccessAt == nil {
		t.Errorf("not-modified should count as success: %+v", feed)
	}
}

func TestRefreshFailureKeepsLastGoodDocument(t *testing.T) {
	host := newFakeHost(t)
	service, store := newTestService(t, Options{})
	ctx := context.Background()
	feed, _, _ := service.Subscribe(ctx, host.server.URL, Settings{})

	host.set(func(host *fakeHost) { host.status = http.StatusBadGateway })
	if _, err := service.Refresh(ctx, &feed); !errors.Is(err, ErrFetchFailed) {
		t.Fatalf("err = %v, want ErrFetchFailed", err)
	}
	stored, _ := store.GetFeed(ctx, feed.ID)
	if !strings.Contains(stored.LastError, "502") {
		t.Errorf("failure not recorded: %q", stored.LastError)
	}

	// The last good copy is still served.
	output, err := service.Render(ctx, stored, testURLs)
	if err != nil || !strings.Contains(string(output), "ep-1") {
		t.Errorf("Render after failed poll = %v", err)
	}

	host.set(func(host *fakeHost) { host.status = http.StatusOK })
	if _, err := service.Refresh(ctx, &feed); err != nil {
		t.Fatal(err)
	}
	if feed.LastError != "" {
		t.Errorf("error not cleared after recovery: %q", feed.LastError)
	}
}

func TestRender(t *testing.T) {
	host := newFakeHost(t)
	service, store := newTestService(t, Options{})
	ctx := context.Background()
	feed, _, _ := service.Subscribe(ctx, host.server.URL, Settings{})
	host.addItem("ep-2", "Tue, 22 Sep 2026 06:00:00 +0000")
	if _, err := service.Refresh(ctx, &feed); err != nil {
		t.Fatal(err)
	}

	output, err := service.Render(ctx, feed, testURLs)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	parsed, err := rss.Parse(output)
	if err != nil {
		t.Fatalf("rendered feed doesn't parse: %v", err)
	}

	// ep-2 is still waiting for its download, so only the backlog ep-1 shows.
	if len(parsed.Items) != 1 || parsed.Items[0].GUID != "ep-1" {
		t.Fatalf("items = %+v, want only ep-1", parsed.Items)
	}
	if strings.Contains(string(output), "media.example.com") {
		t.Error("original audio URL left in the rendered feed")
	}
	if !strings.Contains(string(output), `href="`+testURLs.Feed(feed.ID)+`"`) {
		t.Error("self link doesn't point at the signed feed URL")
	}

	// The enclosure URL is signed over its path.
	enclosure, err := url.Parse(parsed.Items[0].Enclosure.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(enclosure.Path, ".mp3") || !testURLs.Signer.Verify(enclosure.Path, enclosure.Query().Get(signing.QueryParameter)) {
		t.Errorf("enclosure URL %q is not a valid signed episode URL", enclosure)
	}

	// Once ep-2 is cached it appears, with its cached size.
	episodes, _ := store.ListEpisodes(ctx, feed.ID)
	for _, episode := range episodes {
		if episode.GUID == "ep-2" {
			episode.State = models.EpisodeReady
			episode.CacheSize = 4242
			if err := store.UpdateEpisode(ctx, &episode); err != nil {
				t.Fatal(err)
			}
		}
	}
	output, _ = service.Render(ctx, feed, testURLs)
	parsed, _ = rss.Parse(output)
	if len(parsed.Items) != 2 || parsed.Items[0].GUID != "ep-2" || parsed.Items[0].Enclosure.Length != 4242 {
		t.Errorf("after ready: items = %+v", parsed.Items)
	}
}

func TestRenderOriginalModeKeepsAudioURLs(t *testing.T) {
	host := newFakeHost(t)
	service, _ := newTestService(t, Options{})
	ctx := context.Background()
	feed, _, _ := service.Subscribe(ctx, host.server.URL, Settings{DeliveryMode: "original"})

	output, err := service.Render(ctx, feed, testURLs)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), `url="https://media.example.com/ep-1.mp3"`) {
		t.Errorf("original mode changed the audio URL:\n%s", output)
	}
	if !strings.Contains(string(output), testURLs.Feed(feed.ID)) {
		t.Error("original mode must still point the self link at Solstein")
	}
}

func TestRenderUnsigned(t *testing.T) {
	host := newFakeHost(t)
	service, _ := newTestService(t, Options{})
	ctx := context.Background()
	feed, _, _ := service.Subscribe(ctx, host.server.URL, Settings{})

	output, err := service.Render(ctx, feed, URLs{Base: "http://solstein:8080", Unsigned: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(output), "sig=") {
		t.Error("signatures present with auth disabled")
	}
}

func TestFeedManagement(t *testing.T) {
	host := newFakeHost(t)
	service, _ := newTestService(t, Options{})
	ctx := context.Background()
	feed, _, _ := service.Subscribe(ctx, host.server.URL, Settings{})

	feeds, err := service.List(ctx)
	if err != nil || len(feeds) != 1 {
		t.Fatalf("List = %+v, %v", feeds, err)
	}

	feed.DeliveryMode = "stream"
	if err := service.Update(ctx, &feed); err != nil {
		t.Fatal(err)
	}
	stored, _ := service.Feed(ctx, feed.ID)
	if service.DeliveryMode(stored) != "stream" {
		t.Errorf("delivery mode = %q", service.DeliveryMode(stored))
	}
	feed.DeliveryMode = "fax"
	if err := service.Update(ctx, &feed); !errors.Is(err, ErrInvalidSettings) {
		t.Errorf("bad update: err = %v", err)
	}

	if err := service.Delete(ctx, feed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Feed(ctx, feed.ID); !errors.Is(err, database.ErrFeedNotFound) {
		t.Errorf("after delete: err = %v", err)
	}
}

func TestRenderDatesLateEpisodesFromTheirRelease(t *testing.T) {
	host := newFakeHost(t)
	clock := &testClock{now: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)}
	service, store := newTestService(t, Options{Now: clock.Now})
	ctx := context.Background()
	feed, _, _ := service.Subscribe(ctx, host.server.URL, Settings{})

	// Published at the source at 11:55, found by Solstein at 12:00, held
	// back until cached at 12:30.
	host.addItem("ep-2", "Thu, 24 Sep 2026 11:55:00 +0000")
	if _, err := service.Refresh(ctx, &feed); err != nil {
		t.Fatal(err)
	}
	service.Render(ctx, feed, testURLs) // hidden: not released
	clock.advance(30 * time.Minute)
	episodes, _ := store.ListEpisodes(ctx, feed.ID)
	for _, episode := range episodes {
		if episode.GUID == "ep-2" {
			episode.State = models.EpisodeReady
			store.UpdateEpisode(ctx, &episode)
		}
	}

	dates := func() map[string]time.Time {
		output, err := service.Render(ctx, feed, testURLs)
		if err != nil {
			t.Fatal(err)
		}
		parsed, _ := rss.Parse(output)
		result := map[string]time.Time{}
		for _, item := range parsed.Items {
			result[item.GUID] = *item.PublishedAt
		}
		return result
	}

	first := dates()
	releasedAt := time.Date(2026, 9, 24, 12, 30, 0, 0, time.UTC)
	if !first["ep-2"].Equal(releasedAt) {
		t.Errorf("late episode dated %v, want its release %v", first["ep-2"], releasedAt)
	}
	if !first["ep-1"].Equal(time.Date(2026, 9, 21, 6, 0, 0, 0, time.UTC)) {
		t.Errorf("backlog episode's date changed to %v", first["ep-1"])
	}

	// The date is fixed at the first release, not moved on every render.
	clock.advance(time.Hour)
	if later := dates(); !later["ep-2"].Equal(releasedAt) {
		t.Errorf("date moved to %v on a later render", later["ep-2"])
	}
}

func TestRenderKeepsDatesAfterRelease(t *testing.T) {
	host := newFakeHost(t)
	// Solstein saw the episode before its stated time (a feed dated in the
	// future): the source date is kept.
	clock := &testClock{now: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)}
	service, _ := newTestService(t, Options{Now: clock.Now, DefaultDeliveryMode: "stream"})
	ctx := context.Background()
	feed, _, _ := service.Subscribe(ctx, host.server.URL, Settings{})
	host.addItem("ep-2", "Thu, 24 Sep 2026 18:00:00 +0000")
	service.Refresh(ctx, &feed)

	output, _ := service.Render(ctx, feed, testURLs)
	if !strings.Contains(string(output), "Thu, 24 Sep 2026 18:00:00 +0000") {
		t.Errorf("future-dated episode's date changed:\n%s", output)
	}
}

// A feed a processor handles holds new episodes back until processed, even
// in stream mode, and serves the processed file's length and duration.
func TestRenderProcessedFeed(t *testing.T) {
	host := newFakeHost(t)
	service, store := newTestService(t, Options{DefaultDeliveryMode: "stream", Processed: func(models.Feed) bool { return true }})
	ctx := context.Background()
	feed, _, _ := service.Subscribe(ctx, host.server.URL, Settings{})
	host.set(func(host *fakeHost) {
		host.items = append([]string{`<item><title>ep-2</title><guid>ep-2</guid><pubDate>Tue, 22 Sep 2026 06:00:00 +0000</pubDate>` +
			`<itunes:duration xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd">39:52</itunes:duration>` +
			`<enclosure url="https://media.example.com/ep-2.mp3" type="audio/mpeg" length="100"/></item>`}, host.items...)
	})
	added, err := service.Refresh(ctx, &feed)
	if err != nil || len(added) != 1 || added[0].State != models.EpisodeDiscovered || added[0].SourceSeconds != 2392 {
		t.Fatalf("added = %+v, %v; want ep-2 waiting for processing, stated 39:52", added, err)
	}

	output, _ := service.Render(ctx, feed, testURLs)
	if parsed, _ := rss.Parse(output); len(parsed.Items) != 1 {
		t.Fatalf("items = %+v, want ep-2 held back", parsed.Items)
	}

	episode := added[0]
	episode.State, episode.CacheSize, episode.CacheSeconds = models.EpisodeReady, 4242, 2406
	if err := store.UpdateEpisode(ctx, &episode); err != nil {
		t.Fatal(err)
	}
	output, _ = service.Render(ctx, feed, testURLs)
	parsed, _ := rss.Parse(output)
	if len(parsed.Items) != 2 || parsed.Items[0].Enclosure.Length != 4242 || parsed.Items[0].Duration != "40:06" {
		t.Errorf("after processing: items = %+v", parsed.Items)
	}
}
