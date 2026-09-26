package server

import (
	"context"
	stdcontext "context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"aunefyren/solstein/database"
	"aunefyren/solstein/episodes"
	"aunefyren/solstein/feeds"
	"aunefyren/solstein/logger"
	"aunefyren/solstein/outbound"
	"aunefyren/solstein/rss"
	"aunefyren/solstein/settings"
	"aunefyren/solstein/signing"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

const testToken = "test-token-0123456789"

// captureLog swaps logger.Log for one writing into a buffer at the given level.
func captureLog(t *testing.T, level logrus.Level) *strings.Builder {
	t.Helper()
	original := logger.Log
	t.Cleanup(func() { logger.Log = original })

	var output strings.Builder
	logger.Log = logrus.New()
	logger.Log.SetOutput(&output)
	logger.Log.SetLevel(level)
	return &output
}

const testAudio = "ID3fake-audio"

// startPodcastHost serves a small valid feed at /feed whose one episode's
// audio is at /ep-1.mp3 on the same host, and HTML at /page.
func startPodcastHost(t *testing.T) *httptest.Server {
	t.Helper()
	var host *httptest.Server
	host = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/page":
			fmt.Fprint(writer, "<html><body>Not a feed</body></html>")
		case "/ep-1.mp3":
			writer.Header().Set("Content-Type", "audio/mpeg")
			fmt.Fprint(writer, testAudio)
		default:
			fmt.Fprintf(writer, `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:atom="http://www.w3.org/2005/Atom"><channel><title>Fake Show</title>
<atom:link href="https://source.example.com/feed" rel="self"/>
<item><title>One</title><guid>ep-1</guid><pubDate>Mon, 21 Sep 2026 06:00:00 +0000</pubDate>
<enclosure url="%s/ep-1.mp3" type="audio/mpeg" length="100"/></item>
</channel></rss>`, host.URL)
		}
	}))
	t.Cleanup(host.Close)
	return host
}

func testConfig() settings.Config {
	return settings.Config{
		Port:                  8080,
		LogLevel:              "info",
		AuthToken:             testToken,
		URLSigningKey:         "test-signing-key",
		AllowedClientNetworks: []string{},
		TrustedProxies:        []string{},
		AllowedSourceHosts:    []string{},
		DeliveryMode:          "cache",
		PollIntervalMinutes:   15,
		CacheRetentionDays:    14,
	}
}

func newTestRouter(t *testing.T, modify func(cfg *settings.Config)) *gin.Engine {
	t.Helper()
	router, _ := newTestRouterWithDir(t, modify)
	return router
}

// newTestRouterWithDir also returns the config directory, for tests that
// look at the cache.
func newTestRouterWithDir(t *testing.T, modify func(cfg *settings.Config)) (*gin.Engine, string) {
	t.Helper()
	router, configDir, _ := newTestRouterWithStore(t, modify)
	return router, configDir
}

// newTestRouterWithStore also returns the store, for tests that make it fail.
func newTestRouterWithStore(t *testing.T, modify func(cfg *settings.Config)) (*gin.Engine, string, *database.Store) {
	t.Helper()
	captureLog(t, logrus.DebugLevel)
	cfg := testConfig()
	if modify != nil {
		modify(&cfg)
	}
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
	service := feeds.New(store, exits, feeds.Options{DefaultDeliveryMode: cfg.DeliveryMode, AllowedSourceHosts: cfg.AllowedSourceHosts})
	cache, err := episodes.NewCache(filepath.Join(configDir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	pipeline := episodes.NewPipeline(store, exits, cache, episodes.Options{DefaultDeliveryMode: cfg.DeliveryMode})
	episodeServer := episodes.NewServer(store, exits, cache, service, pipeline, episodes.Options{})
	router, err := newRouter(Options{Config: cfg, Version: "v1.2.3", Feeds: service, Episodes: episodeServer})
	if err != nil {
		t.Fatal(err)
	}
	return router, configDir, store
}

func do(router http.Handler, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, target, reader)
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestHealth(t *testing.T) {
	router := newTestRouter(t, nil)
	recorder := do(router, http.MethodGet, "/api/health", "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" || body["version"] != "v1.2.3" {
		t.Errorf("body = %v", body)
	}
}

func TestSubscribeByPrefix(t *testing.T) {
	host := startPodcastHost(t)
	router := newTestRouter(t, nil)

	for _, source := range []string{host.URL + "/feed", strings.Replace(host.URL, "http://", "http:/", 1) + "/feed"} {
		recorder := do(router, http.MethodGet, "/api/rss/"+testToken+"/"+source, "", nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, body %s", source, recorder.Code, recorder.Body)
		}
		if recorder.Header().Get("Content-Type") != "application/rss+xml; charset=utf-8" {
			t.Errorf("content type = %q", recorder.Header().Get("Content-Type"))
		}
		body := recorder.Body.String()
		if !strings.Contains(body, `<atom:link href="http://example.com/api/feeds/`) || !strings.Contains(body, "sig=") {
			t.Errorf("self link not rewritten to a signed Solstein URL:\n%s", body)
		}
		if strings.Contains(body, host.URL+"/ep-1.mp3") {
			t.Error("original audio URL left in the feed")
		}
	}
}

func TestSubscribeByPrefixRefusals(t *testing.T) {
	host := startPodcastHost(t)
	router := newTestRouter(t, func(cfg *settings.Config) { cfg.AllowedSourceHosts = []string{"127.0.0.1", "acast.com"} })

	cases := []struct {
		name   string
		target string
		status int
	}{
		{"wrong token", "/api/rss/wrong-token-0000000000/" + host.URL + "/feed", http.StatusForbidden},
		{"no token", "/api/rss/" + host.URL + "/feed", http.StatusForbidden},
		{"invalid source", "/api/rss/" + testToken + "/ftp://example.com/feed", http.StatusBadRequest},
		{"host not allowed", "/api/rss/" + testToken + "/https://example.com/feed", http.StatusForbidden},
		{"not RSS", "/api/rss/" + testToken + "/" + host.URL + "/page", http.StatusBadGateway},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if recorder := do(router, http.MethodGet, c.target, "", nil); recorder.Code != c.status {
				t.Errorf("status = %d, want %d (body %s)", recorder.Code, c.status, recorder.Body)
			}
		})
	}
}

// signedFeedPath subscribes through the prefix route and returns the path
// and query of the signed feed URL the served feed points at.
func signedFeedPath(t *testing.T, router http.Handler, source string) string {
	t.Helper()
	recorder := do(router, http.MethodGet, "/api/rss/"+testToken+"/"+source, "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("subscribe: status %d", recorder.Code)
	}
	start := strings.Index(recorder.Body.String(), `href="`) + len(`href="`)
	end := strings.Index(recorder.Body.String()[start:], `"`)
	link, err := url.Parse(strings.ReplaceAll(recorder.Body.String()[start:start+end], "&amp;", "&"))
	if err != nil {
		t.Fatal(err)
	}
	return link.RequestURI()
}

func TestFeedByID(t *testing.T) {
	host := startPodcastHost(t)
	router := newTestRouter(t, nil)
	signed := signedFeedPath(t, router, host.URL+"/feed")
	path, _, _ := strings.Cut(signed, "?")

	recorder := do(router, http.MethodGet, signed, "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("signed URL: status = %d", recorder.Code)
	}
	if _, err := rss.Parse(recorder.Body.Bytes()); err != nil {
		t.Errorf("served feed doesn't parse: %v", err)
	}

	otherID := "00000000-0000-0000-0000-000000000001"
	otherPath := "/api/feeds/" + otherID + ".xml"
	cases := []struct {
		name   string
		target string
		status int
	}{
		{"no signature", path, http.StatusForbidden},
		{"wrong signature", path + "?sig=AAAA", http.StatusForbidden},
		{"signature of another path", otherPath + "?" + strings.SplitN(signed, "?", 2)[1], http.StatusForbidden},
		{"not a UUID", "/api/feeds/abc.xml", http.StatusNotFound},
		{"no .xml", "/api/feeds/" + otherID, http.StatusNotFound},
		{"unknown feed, valid signature", otherPath + "?sig=" + signing.New("test-signing-key").Sign(otherPath), http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if recorder := do(router, http.MethodGet, c.target, "", nil); recorder.Code != c.status {
				t.Errorf("status = %d, want %d", recorder.Code, c.status)
			}
		})
	}
}

func TestAuthDisabled(t *testing.T) {
	host := startPodcastHost(t)
	router := newTestRouter(t, func(cfg *settings.Config) { cfg.DisableAuth = true })

	recorder := do(router, http.MethodGet, "/api/rss/"+host.URL+"/feed", "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("tokenless subscribe: status = %d, body %s", recorder.Code, recorder.Body)
	}
	if strings.Contains(recorder.Body.String(), "sig=") {
		t.Error("URLs signed with auth disabled")
	}
	// The token segment still works when given.
	if recorder := do(router, http.MethodGet, "/api/rss/"+testToken+"/"+host.URL+"/feed", "", nil); recorder.Code != http.StatusOK {
		t.Errorf("subscribe with token: status = %d", recorder.Code)
	}
	if recorder := do(router, http.MethodGet, "/api/v1/feeds", "", nil); recorder.Code != http.StatusOK {
		t.Errorf("API without token: status = %d", recorder.Code)
	}
}

func TestClientNetworkCheck(t *testing.T) {
	router := newTestRouter(t, func(cfg *settings.Config) { cfg.AllowedClientNetworks = []string{"10.0.0.0/8"} })

	// httptest requests come from 192.0.2.1.
	if recorder := do(router, http.MethodGet, "/api/v1/feeds?token="+testToken, "", nil); recorder.Code != http.StatusForbidden {
		t.Errorf("outside network: status = %d, want 403", recorder.Code)
	}
	if recorder := do(router, http.MethodGet, "/api/health", "", nil); recorder.Code != http.StatusOK {
		t.Errorf("health from outside network: status = %d, want 200", recorder.Code)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/feeds?token="+testToken, nil)
	request.RemoteAddr = "10.1.2.3:5555"
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Errorf("inside network: status = %d, want 200", recorder.Code)
	}
}

func TestForwardedHeadersOnlyFromTrustedProxies(t *testing.T) {
	host := startPodcastHost(t)
	headers := map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-Host": "solstein.example.com"}

	untrusted := newTestRouter(t, nil)
	body := do(untrusted, http.MethodGet, "/api/rss/"+testToken+"/"+host.URL+"/feed", "", headers).Body.String()
	if strings.Contains(body, "solstein.example.com") {
		t.Error("forwarded host believed from an untrusted client")
	}

	trusted := newTestRouter(t, func(cfg *settings.Config) { cfg.TrustedProxies = []string{"192.0.2.0/24"} })
	body = do(trusted, http.MethodGet, "/api/rss/"+testToken+"/"+host.URL+"/feed", "", headers).Body.String()
	if !strings.Contains(body, `href="https://solstein.example.com/api/feeds/`) {
		t.Errorf("forwarded headers from a trusted proxy ignored:\n%s", body)
	}

	external := newTestRouter(t, func(cfg *settings.Config) { cfg.ExternalURL = "https://podcasts.example.net" })
	body = do(external, http.MethodGet, "/api/rss/"+testToken+"/"+host.URL+"/feed", "", nil).Body.String()
	if !strings.Contains(body, `href="https://podcasts.example.net/api/feeds/`) {
		t.Error("external_url not used for links")
	}
}

func TestFeedAPI(t *testing.T) {
	host := startPodcastHost(t)
	router := newTestRouter(t, nil)
	bearer := map[string]string{"Authorization": "Bearer " + testToken, "Content-Type": "application/json"}

	if recorder := do(router, http.MethodGet, "/api/v1/feeds", "", nil); recorder.Code != http.StatusUnauthorized {
		t.Errorf("no token: status = %d, want 401", recorder.Code)
	}
	if recorder := do(router, http.MethodGet, "/api/v1/feeds", "", map[string]string{"Authorization": "Bearer nope"}); recorder.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: status = %d, want 401", recorder.Code)
	}

	createBody := `{"source_url": "` + host.URL + `/feed", "delivery_mode": "stream"}`
	recorder := do(router, http.MethodPost, "/api/v1/feeds", createBody, bearer)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create: status = %d, body %s", recorder.Code, recorder.Body)
	}
	var created struct {
		ID                string `json:"id"`
		Title             string `json:"title"`
		DeliveryMode      string `json:"delivery_mode"`
		DeliveryModeInUse string `json:"delivery_mode_in_use"`
		FeedURL           string `json:"feed_url"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Title != "Fake Show" || created.DeliveryMode != "stream" || !strings.Contains(created.FeedURL, "sig=") {
		t.Errorf("created = %+v", created)
	}
	if strings.Contains(recorder.Body.String(), "etag") {
		t.Error("internal fields leaked into the API response")
	}

	if recorder := do(router, http.MethodPost, "/api/v1/feeds", createBody, bearer); recorder.Code != http.StatusOK {
		t.Errorf("create again: status = %d, want 200", recorder.Code)
	}
	if recorder := do(router, http.MethodPost, "/api/v1/feeds", `{}`, bearer); recorder.Code != http.StatusBadRequest {
		t.Errorf("create without source: status = %d, want 400", recorder.Code)
	}

	if recorder := do(router, http.MethodGet, "/api/v1/feeds?token="+testToken, "", nil); recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), created.ID) {
		t.Errorf("list with query token: status = %d", recorder.Code)
	}

	feedPath := "/api/v1/feeds/" + created.ID
	if recorder := do(router, http.MethodPatch, feedPath, `{"delivery_mode": "fax"}`, bearer); recorder.Code != http.StatusBadRequest {
		t.Errorf("invalid patch: status = %d, want 400", recorder.Code)
	}
	recorder = do(router, http.MethodPatch, feedPath, `{"delivery_mode": ""}`, bearer)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"delivery_mode_in_use":"cache"`) {
		t.Errorf("clearing override: status = %d, body %s", recorder.Code, recorder.Body)
	}
	// Region diff isn't running here: a feed can't switch it on, but can
	// switch it off.
	for _, body := range []string{`{"region_diff": "on"}`, `{"region_diff": "yes"}`, `{"region_diff_exits": ["direct", "direct"]}`, `{"region_diff_on_failure": "shrug"}`, `{"region_diff_compare_by_audio": "sometimes"}`} {
		if recorder := do(router, http.MethodPatch, feedPath, body, bearer); recorder.Code != http.StatusBadRequest {
			t.Errorf("patch %s: status = %d, want 400", body, recorder.Code)
		}
	}
	recorder = do(router, http.MethodPatch, feedPath, `{"region_diff": "off", "region_diff_on_failure": "hide"}`, bearer)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"region_diff":"off"`) || !strings.Contains(recorder.Body.String(), `"region_diff_in_use":false`) {
		t.Errorf("region diff off: status = %d, body %s", recorder.Code, recorder.Body)
	}
	if recorder := do(router, http.MethodGet, feedPath, "", bearer); recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"region_diff_on_failure":"hide"`) {
		t.Errorf("get: status = %d, body %s", recorder.Code, recorder.Body)
	}
	if recorder := do(router, http.MethodDelete, feedPath, "", bearer); recorder.Code != http.StatusNoContent {
		t.Errorf("delete: status = %d, want 204", recorder.Code)
	}
	if recorder := do(router, http.MethodGet, feedPath, "", bearer); recorder.Code != http.StatusNotFound {
		t.Errorf("get after delete: status = %d, want 404", recorder.Code)
	}
	if recorder := do(router, http.MethodGet, "/api/v1/feeds/not-a-uuid", "", bearer); recorder.Code != http.StatusNotFound {
		t.Errorf("bad ID: status = %d, want 404", recorder.Code)
	}
}

func TestQueueAPI(t *testing.T) {
	host := startPodcastHost(t)
	router := newTestRouter(t, nil)
	bearer := map[string]string{"Authorization": "Bearer " + testToken, "Content-Type": "application/json"}
	recorder := do(router, http.MethodPost, "/api/v1/feeds", `{"source_url": "`+host.URL+`/feed"}`, bearer)
	var feed struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &feed); err != nil || feed.ID == "" {
		t.Fatalf("create: %d %s", recorder.Code, recorder.Body)
	}
	feedPath := "/api/v1/feeds/" + feed.ID

	cases := []struct {
		name, method, target, body string
		status                     int
		response                   string
	}{
		{"prepare all, no body", http.MethodPost, feedPath + "/prepare", "", http.StatusOK, `{"queued":1}`},
		{"prepare newest", http.MethodPost, feedPath + "/prepare", `{"newest": 1}`, http.StatusOK, `{"queued":1}`},
		{"prepare negative", http.MethodPost, feedPath + "/prepare", `{"newest": -1}`, http.StatusBadRequest, ""},
		{"prepare bad body", http.MethodPost, feedPath + "/prepare", `[`, http.StatusBadRequest, ""},
		{"retry, nothing failed", http.MethodPost, feedPath + "/retry", "", http.StatusOK, `{"queued":0}`},
		{"retry every feed", http.MethodPost, "/api/v1/retry", "", http.StatusOK, `{"queued":0}`},
		{"unknown feed", http.MethodPost, "/api/v1/feeds/00000000-0000-0000-0000-000000000001/retry", "", http.StatusNotFound, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			recorder := do(router, c.method, c.target, c.body, bearer)
			if recorder.Code != c.status || (c.response != "" && recorder.Body.String() != c.response) {
				t.Errorf("%d %s", recorder.Code, recorder.Body)
			}
		})
	}
	if recorder := do(router, http.MethodPost, feedPath+"/retry", "", nil); recorder.Code != http.StatusUnauthorized {
		t.Errorf("without token: %d", recorder.Code)
	}

	// Stream mode: nothing is prepared, so nothing can be queued.
	do(router, http.MethodPatch, feedPath, `{"delivery_mode": "stream"}`, bearer)
	if recorder := do(router, http.MethodPost, feedPath+"/prepare", "", bearer); recorder.Code != http.StatusBadRequest {
		t.Errorf("stream mode: %d %s", recorder.Code, recorder.Body)
	}
}

func TestRequestLoggerLevelsAndRedaction(t *testing.T) {
	cases := []struct {
		name      string
		path      string
		wantLevel string
		wantPath  string
	}{
		{name: "success logs at debug", path: "/api/health", wantLevel: "level=debug", wantPath: "/api/health"},
		{name: "not found logs at warn", path: "/nope", wantLevel: "level=warning", wantPath: "/nope"},
		{name: "token redacted", path: "/api/rss/" + testToken + "/ftp://x", wantLevel: "level=warning", wantPath: "/api/rss/***/ftp://x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			router := newTestRouter(t, nil)
			output := captureLog(t, logrus.DebugLevel)
			do(router, http.MethodGet, c.path, "", nil)

			if !strings.Contains(output.String(), c.wantLevel) || !strings.Contains(output.String(), c.wantPath) {
				t.Errorf("log = %q, want %s entry for %s", output.String(), c.wantLevel, c.wantPath)
			}
			if strings.Contains(output.String(), testToken) {
				t.Errorf("token in log: %q", output.String())
			}
		})
	}
}

func TestRedactPath(t *testing.T) {
	cases := map[string]string{
		"/api/rss/secret/https:/example.com/feed": "/api/rss/***/https:/example.com/feed",
		"/api/rss/secret":                         "/api/rss/***",
		"/api/feeds/abc.xml":                      "/api/feeds/abc.xml",
	}
	for path, want := range cases {
		if got := redactPath(path); got != want {
			t.Errorf("redactPath(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestNewRejectsBadNetworks(t *testing.T) {
	for _, modify := range []func(cfg *settings.Config){
		func(cfg *settings.Config) { cfg.AllowedClientNetworks = []string{"lan"} },
		func(cfg *settings.Config) { cfg.TrustedProxies = []string{"proxy"} },
	} {
		cfg := testConfig()
		modify(&cfg)
		if _, err := New(Options{Config: cfg}); err == nil {
			t.Errorf("expected an error for %+v", cfg)
		}
	}
}

func TestRunShutsDownOnCancel(t *testing.T) {
	captureLog(t, logrus.InfoLevel)

	// Grab a free port, then release it for the server to bind.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	cfg := testConfig()
	cfg.Port = port
	srv, err := New(Options{Config: cfg, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	srv.Addr = "127.0.0.1:" + strconv.Itoa(port)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, srv) }()

	healthURL := "http://" + srv.Addr + "/api/health"
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(healthURL)
		if err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never came up: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want nil after cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRunReportsListenError(t *testing.T) {
	captureLog(t, logrus.InfoLevel)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	srv, err := New(Options{Config: testConfig(), Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	srv.Addr = listener.Addr().String() // already taken

	if err := Run(context.Background(), srv); err == nil {
		t.Error("expected an error when the port is taken")
	}
}

// enclosurePath subscribes and returns the path and query of the first
// episode's signed enclosure URL.
func enclosurePath(t *testing.T, router http.Handler, source string) string {
	t.Helper()
	recorder := do(router, http.MethodGet, "/api/rss/"+testToken+"/"+source, "", nil)
	feed, err := rss.Parse(recorder.Body.Bytes())
	if err != nil || len(feed.Items) == 0 || feed.Items[0].Enclosure == nil {
		t.Fatalf("no enclosure in served feed: %v\n%s", err, recorder.Body)
	}
	link, err := url.Parse(feed.Items[0].Enclosure.URL)
	if err != nil {
		t.Fatal(err)
	}
	return link.RequestURI()
}

func TestEpisodeRoute(t *testing.T) {
	host := startPodcastHost(t)
	router := newTestRouter(t, nil)
	signed := enclosurePath(t, router, host.URL+"/feed")
	path, query, _ := strings.Cut(signed, "?")

	recorder := do(router, http.MethodGet, signed, "", nil)
	if recorder.Code != http.StatusOK || recorder.Body.String() != testAudio {
		t.Fatalf("GET signed episode: %d %q", recorder.Code, recorder.Body)
	}
	// Now cached: served with range support.
	recorder = do(router, http.MethodGet, signed, "", map[string]string{"Range": "bytes=0-2"})
	if recorder.Code != http.StatusPartialContent || recorder.Body.String() != testAudio[:3] {
		t.Errorf("range: %d %q", recorder.Code, recorder.Body)
	}
	if recorder := do(router, http.MethodHead, signed, "", nil); recorder.Code != http.StatusOK || recorder.Body.Len() != 0 {
		t.Errorf("HEAD: %d %q", recorder.Code, recorder.Body)
	}

	feedID := strings.Split(path, "/")[3]
	otherPath := "/api/episodes/" + feedID + "/00000000-0000-0000-0000-000000000001.mp3"
	cases := []struct {
		name   string
		target string
		status int
	}{
		{"no signature", path, http.StatusForbidden},
		{"other extension, same signature", strings.TrimSuffix(path, ".mp3") + ".m4a?" + query, http.StatusForbidden},
		{"bad extension", strings.TrimSuffix(path, ".mp3") + ".mp3x!?" + query, http.StatusNotFound},
		{"not a UUID", "/api/episodes/" + feedID + "/abc.mp3", http.StatusNotFound},
		{"unknown episode, valid signature", otherPath + "?sig=" + signing.New("test-signing-key").Sign(otherPath), http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if recorder := do(router, http.MethodGet, c.target, "", nil); recorder.Code != c.status {
				t.Errorf("status = %d, want %d", recorder.Code, c.status)
			}
		})
	}
}

func TestDeleteFeedRemovesCachedAudio(t *testing.T) {
	host := startPodcastHost(t)
	router, configDir := newTestRouterWithDir(t, nil)
	signed := enclosurePath(t, router, host.URL+"/feed")
	if recorder := do(router, http.MethodGet, signed, "", nil); recorder.Code != http.StatusOK {
		t.Fatalf("GET episode: %d", recorder.Code)
	}
	feedID := strings.Split(signed, "/")[3]
	feedCache := filepath.Join(configDir, "cache", feedID)
	if _, err := os.Stat(feedCache); err != nil {
		t.Fatalf("episode not cached: %v", err)
	}

	bearer := map[string]string{"Authorization": "Bearer " + testToken}
	if recorder := do(router, http.MethodDelete, "/api/v1/feeds/"+feedID, "", bearer); recorder.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", recorder.Code)
	}
	if _, err := os.Stat(feedCache); !os.IsNotExist(err) {
		t.Errorf("cached audio of the deleted feed is still there: %v", err)
	}
}

func TestEpisodeRouteSourceFailure(t *testing.T) {
	host := startPodcastHost(t)
	router := newTestRouter(t, nil)
	signed := enclosurePath(t, router, host.URL+"/feed")
	host.Close() // the source is gone

	if recorder := do(router, http.MethodGet, signed, "", nil); recorder.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", recorder.Code)
	}
}

// A client that gives up mid-request (a podcast client's own download
// timeout, ABS's PODCAST_DOWNLOAD_TIMEOUT) is not a server error: nothing can
// reach it any more. It used to be logged and answered as a 500, which buried
// the real errors under every abandoned download (seen live, 2026-09-26).
func TestEpisodeRouteClientGivesUp(t *testing.T) {
	host := startPodcastHost(t)
	router := newTestRouter(t, nil)
	signed := enclosurePath(t, router, host.URL+"/feed")

	ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
	request := httptest.NewRequest(http.MethodGet, signed, nil).WithContext(ctx)
	cancel() // as if the client had hung up
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code == http.StatusInternalServerError {
		t.Errorf("status = %d, want anything but 500 for a client that gave up", recorder.Code)
	}
}

// TestOpenAPICoversEveryRoute keeps docs/openapi.yaml in step with the
// router: every route and method must be documented there. Gin's path
// syntax differs from OpenAPI's, so the mapping is spelled out; a new route
// fails here until it is added to both.
func TestOpenAPICoversEveryRoute(t *testing.T) {
	documented := map[string]string{
		"/api/health":                   "/api/health",
		"/api/rss/:token/*source":       "/api/rss/{token}/{sourceURL}",
		"/api/feeds/:file":              "/api/feeds/{feedID}.xml",
		"/api/episodes/:feedID/:file":   "/api/episodes/{feedID}/{episodeID}.{extension}",
		"/api/v1/feeds":                 "/api/v1/feeds",
		"/api/v1/feeds/:feedID":         "/api/v1/feeds/{feedID}",
		"/api/v1/feeds/:feedID/retry":   "/api/v1/feeds/{feedID}/retry",
		"/api/v1/feeds/:feedID/prepare": "/api/v1/feeds/{feedID}/prepare",
		"/api/v1/retry":                 "/api/v1/retry",
	}
	spec, err := os.ReadFile(filepath.Join("..", "docs", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// Each path's block runs to the next path (two-space indent) or the
	// next top-level key.
	block := func(path string) string {
		text := string(spec)
		start := strings.Index(text, "\n  "+path+":\n")
		if start < 0 {
			return ""
		}
		rest := text[start+1:]
		end := len(rest)
		for _, marker := range []string{"\n  /", "\n\n" + "security:", "\ncomponents:"} {
			if index := strings.Index(rest[1:], marker); index >= 0 && index+1 < end {
				end = index + 1
			}
		}
		return rest[:end]
	}

	router, _ := newTestRouterWithDir(t, nil)
	for _, route := range router.Routes() {
		specPath, ok := documented[route.Path]
		if !ok {
			t.Errorf("%s %s isn't in docs/openapi.yaml (add it there and to this test's mapping)", route.Method, route.Path)
			continue
		}
		if !strings.Contains(block(specPath), "\n    "+strings.ToLower(route.Method)+":\n") {
			t.Errorf("docs/openapi.yaml has no %s under %s", route.Method, specPath)
		}
	}
}

// createFeed subscribes to source through the API and returns the feed's ID.
func createFeed(t *testing.T, router http.Handler, source string) string {
	t.Helper()
	bearer := map[string]string{"Authorization": "Bearer " + testToken, "Content-Type": "application/json"}
	recorder := do(router, http.MethodPost, "/api/v1/feeds", `{"source_url": "`+source+`"}`, bearer)
	var feed struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &feed); err != nil || feed.ID == "" {
		t.Fatalf("create: %d %s", recorder.Code, recorder.Body)
	}
	return feed.ID
}

func TestFeedAPIPatchFields(t *testing.T) {
	host := startPodcastHost(t)
	router := newTestRouter(t, nil)
	bearer := map[string]string{"Authorization": "Bearer " + testToken, "Content-Type": "application/json"}
	feedPath := "/api/v1/feeds/" + createFeed(t, router, host.URL+"/feed")

	recorder := do(router, http.MethodPatch, feedPath, `{"exit": "", "poll_interval_minutes": 30, "region_diff_trim_break_markers": ""}`, bearer)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"poll_interval_minutes":30`) {
		t.Errorf("patch: %d %s", recorder.Code, recorder.Body)
	}
	if recorder := do(router, http.MethodPatch, feedPath, `[`, bearer); recorder.Code != http.StatusBadRequest {
		t.Errorf("bad body: %d", recorder.Code)
	}
	if recorder := do(router, http.MethodPatch, "/api/v1/feeds/not-a-uuid", `{}`, bearer); recorder.Code != http.StatusNotFound {
		t.Errorf("patch unknown feed: %d", recorder.Code)
	}
	if recorder := do(router, http.MethodDelete, "/api/v1/feeds/not-a-uuid", "", bearer); recorder.Code != http.StatusNotFound {
		t.Errorf("delete unknown feed: %d", recorder.Code)
	}
	if recorder := do(router, http.MethodPost, "/api/v1/feeds/not-a-uuid/prepare", "", bearer); recorder.Code != http.StatusNotFound {
		t.Errorf("prepare unknown feed: %d", recorder.Code)
	}

	// Stream mode prepares nothing, so retrying every feed skips it.
	do(router, http.MethodPatch, feedPath, `{"delivery_mode": "stream"}`, bearer)
	if recorder := do(router, http.MethodPost, "/api/v1/retry", "", bearer); recorder.Code != http.StatusOK || recorder.Body.String() != `{"queued":0}` {
		t.Errorf("retry every feed: %d %s", recorder.Code, recorder.Body)
	}
}

// TestAPIWithoutEpisodes covers a router with no episode server: nothing is
// prepared, so nothing can be queued.
func TestAPIWithoutEpisodes(t *testing.T) {
	host := startPodcastHost(t)
	captureLog(t, logrus.DebugLevel)
	store, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	exits, err := outbound.New(outbound.Options{AllowPrivateDestinations: true})
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	service := feeds.New(store, exits, feeds.Options{DefaultDeliveryMode: cfg.DeliveryMode})
	router, err := newRouter(Options{Config: cfg, Feeds: service})
	if err != nil {
		t.Fatal(err)
	}
	bearer := map[string]string{"Authorization": "Bearer " + testToken, "Content-Type": "application/json"}
	feedPath := "/api/v1/feeds/" + createFeed(t, router, host.URL+"/feed")

	cases := []struct {
		method, target string
		status         int
	}{
		{http.MethodPost, feedPath + "/retry", http.StatusBadRequest},
		{http.MethodPost, feedPath + "/prepare", http.StatusBadRequest},
		{http.MethodPost, "/api/v1/retry", http.StatusOK},
		{http.MethodPatch, feedPath, http.StatusOK},
		{http.MethodDelete, feedPath, http.StatusNoContent},
	}
	for _, c := range cases {
		body := ""
		if c.method == http.MethodPatch {
			body = `{"poll_interval_minutes": 20}`
		}
		if recorder := do(router, c.method, c.target, body, bearer); recorder.Code != c.status {
			t.Errorf("%s %s: %d %s, want %d", c.method, c.target, recorder.Code, recorder.Body, c.status)
		}
	}
}

// TestStoreFailures checks that a failing database gives 500 rather than a
// wrong answer.
func TestStoreFailures(t *testing.T) {
	host := startPodcastHost(t)
	router, _, store := newTestRouterWithStore(t, nil)
	bearer := map[string]string{"Authorization": "Bearer " + testToken, "Content-Type": "application/json"}
	feedSignedPath := signedFeedPath(t, router, host.URL+"/feed")
	episodeSignedPath := enclosurePath(t, router, host.URL+"/feed")
	feedID := strings.Split(episodeSignedPath, "/")[3]
	store.Close()

	cases := []struct {
		method, target string
	}{
		{http.MethodGet, "/api/v1/feeds"},
		{http.MethodGet, "/api/v1/feeds/" + feedID},
		{http.MethodPost, "/api/v1/retry"},
		{http.MethodGet, feedSignedPath},
		{http.MethodGet, episodeSignedPath},
	}
	for _, c := range cases {
		if recorder := do(router, c.method, c.target, "", bearer); recorder.Code != http.StatusInternalServerError {
			t.Errorf("%s %s: %d %s, want 500", c.method, c.target, recorder.Code, recorder.Body)
		}
	}
}
