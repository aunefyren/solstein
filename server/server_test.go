package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"aunefyren/solstein/database"
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

// startPodcastHost serves a small valid feed at /feed, and HTML at /page.
func startPodcastHost(t *testing.T) *httptest.Server {
	t.Helper()
	host := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/page" {
			fmt.Fprint(writer, "<html><body>Not a feed</body></html>")
			return
		}
		fmt.Fprint(writer, `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:atom="http://www.w3.org/2005/Atom"><channel><title>Fake Show</title>
<atom:link href="https://source.example.com/feed" rel="self"/>
<item><title>One</title><guid>ep-1</guid><pubDate>Mon, 21 Sep 2026 06:00:00 +0000</pubDate>
<enclosure url="https://media.example.com/ep-1.mp3" type="audio/mpeg" length="100"/></item>
</channel></rss>`)
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
	captureLog(t, logrus.DebugLevel)
	cfg := testConfig()
	if modify != nil {
		modify(&cfg)
	}
	store, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	exits, err := outbound.New(outbound.Options{AllowPrivateDestinations: true}) // test host is on loopback
	if err != nil {
		t.Fatal(err)
	}
	service := feeds.New(store, exits, feeds.Options{DefaultDeliveryMode: cfg.DeliveryMode, AllowedSourceHosts: cfg.AllowedSourceHosts})
	router, err := newRouter(Options{Config: cfg, Version: "v1.2.3", Feeds: service})
	if err != nil {
		t.Fatal(err)
	}
	return router
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
		if strings.Contains(body, "media.example.com") {
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
	if recorder := do(router, http.MethodGet, feedPath, "", bearer); recorder.Code != http.StatusOK {
		t.Errorf("get: status = %d", recorder.Code)
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
