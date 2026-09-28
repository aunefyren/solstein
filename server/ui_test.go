package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aunefyren/solstein/auth"
	"aunefyren/solstein/database"
	"aunefyren/solstein/episodes"
	"aunefyren/solstein/feeds"
	"aunefyren/solstein/models"
	"aunefyren/solstein/outbound"
	"aunefyren/solstein/settings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
)

// newUITestRouter is a router with the web UI on, signed in: every request
// without a Cookie header gets a session of the user "tester". processed,
// when set, stands in for region diff (feeds.Options.Processed), so its
// setting is offered.
func newUITestRouter(t *testing.T, processed func(models.Feed) bool) (http.Handler, *database.Store) {
	t.Helper()
	return newUITestRouterWith(t, processed, nil)
}

// newUITestRouterWith is newUITestRouter with config.json changed first.
func newUITestRouterWith(t *testing.T, processed func(models.Feed) bool, modify func(cfg *settings.Config)) (http.Handler, *database.Store) {
	t.Helper()
	router, store, service, _ := newSignInTestRouter(t, processed, modify)
	session := signInTester(t, service)
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Cookie") == "" {
			request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
		}
		router.ServeHTTP(writer, request)
	}), store
}

const testerPassword = "tester's own password"

// signInTester adds the user "tester" with testerPassword and returns a
// session for them.
func signInTester(t *testing.T, service *auth.Service) string {
	t.Helper()
	ctx := context.Background()
	_, oneTime, err := service.AddUser(ctx, "tester")
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.SignIn(ctx, "tester", oneTime, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	result, err = service.SetPassword(ctx, result.Token, testerPassword, testerPassword)
	if err != nil {
		t.Fatal(err)
	}
	return result.Token
}

// testClock is sign-in's clock in tests, so a test can wait out a TOTP step
// without waiting.
type testClock struct {
	mutex sync.Mutex
	now   time.Time
}

func (clock *testClock) Now() time.Time {
	clock.mutex.Lock()
	defer clock.mutex.Unlock()
	return clock.now
}

func (clock *testClock) advance(by time.Duration) {
	clock.mutex.Lock()
	defer clock.mutex.Unlock()
	clock.now = clock.now.Add(by)
}

// newSignInTestRouter is a router with the web UI on and nobody signed in.
func newSignInTestRouter(t *testing.T, processed func(models.Feed) bool, modify func(cfg *settings.Config)) (http.Handler, *database.Store, *auth.Service, *testClock) {
	t.Helper()
	captureLog(t, logrus.DebugLevel)
	cfg := testConfig()
	cfg.WebUI.Enabled = true
	if modify != nil {
		modify(&cfg)
	}
	configDir := t.TempDir()
	store, err := database.Open(configDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	exits, err := outbound.New(outbound.Options{AllowPrivateDestinations: true})
	if err != nil {
		t.Fatal(err)
	}
	service := feeds.New(store, exits, feeds.Options{DefaultDeliveryMode: cfg.DeliveryMode, Processed: processed, RegionDiffAvailable: processed != nil})
	cache, err := episodes.NewCache(filepath.Join(configDir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	pipeline := episodes.NewPipeline(store, exits, cache, episodes.Options{DefaultDeliveryMode: cfg.DeliveryMode})
	episodeServer := episodes.NewServer(store, exits, cache, service, pipeline, episodes.Options{})
	clock := &testClock{now: time.Now()}
	signIn, err := auth.New(store, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	router, err := newRouter(Options{Config: cfg, Version: "v1.2.3", Feeds: service, Episodes: episodeServer, Instance: testInstance, Auth: signIn})
	if err != nil {
		t.Fatal(err)
	}
	return router, store, signIn, clock
}

// testInstance is what start-up would hand the server.
var testInstance = Instance{
	StartedAt:   time.Date(2026, 9, 28, 9, 30, 0, 0, time.Local),
	Exits:       []string{"direct", "norway"},
	DefaultExit: "norway",
	Modules: []Module{
		{Key: ModuleExits, Name: "Exits (VPN)", On: true, Summary: "Exits norway over providers proton."},
		{Key: ModuleRegionDiff, Name: "Region diff", Summary: "Not set up: region_diff has no exits."},
	},
	VPN:          true,
	TunnelBudget: []string{"provider 'proton' can hold 1 tunnel at once (one per private key), but 2 of its exits can be in use at the same moment."},
	ExitStatus: func() outbound.Status {
		return outbound.Status{
			Providers: []outbound.ProviderStatus{{
				Name: "proton", Type: "protonvpn", Servers: 603, ServerList: "built-in list, dated 2026-08-06", Keys: 1, MaxTunnels: 1,
				Tunnels: []outbound.TunnelStatus{{Server: "NO#23", Country: "NO", Users: 2, LastHandshake: time.Now().Add(-40 * time.Second), Key: 1}},
				Benched: []outbound.BenchedServer{{Server: "SE#12", Until: time.Now().Add(10 * time.Minute)}},
			}},
			Exits: []outbound.ExitStatus{
				{Name: "direct", Country: "NO"},
				{Name: "norway", Provider: "proton", Locations: []string{"NO"}, Strict: true, Server: "NO#23", Country: "NO", Tunnel: &outbound.TunnelStatus{Server: "NO#23", Users: 2, LastHandshake: time.Now().Add(-40 * time.Second), Key: 1}},
				{Name: "sweden", Provider: "proton", Locations: []string{"SE", "NO"}},
			},
		}
	},
}

// regionDiffLike processes a feed that switches it on, as region diff with
// enabled: false does.
func regionDiffLike(feed models.Feed) bool {
	return feed.RegionDiff == "on"
}

func postForm(router http.Handler, target string, values url.Values, headers map[string]string) (code int, location, body string) {
	all := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	for key, value := range headers {
		all[key] = value
	}
	recorder := do(router, http.MethodPost, target, values.Encode(), all)
	return recorder.Code, recorder.Header().Get("Location"), recorder.Body.String()
}

func storedFeed(t *testing.T, store *database.Store, id string) models.Feed {
	t.Helper()
	feed, err := store.GetFeed(context.Background(), uuid.MustParse(id))
	if err != nil {
		t.Fatal(err)
	}
	return feed
}

func TestUIOffByDefault(t *testing.T) {
	router := newTestRouter(t, nil)
	for _, target := range []string{"/", "/ui", "/ui/feeds", "/ui/login", "/ui/static/style.css"} {
		if code := do(router, http.MethodGet, target, "", nil).Code; code != http.StatusNotFound {
			t.Errorf("GET %s with the UI off = %d, want 404", target, code)
		}
	}
}

func TestUIListsFeeds(t *testing.T) {
	router, store := newUITestRouter(t, nil)
	host := startPodcastHost(t)
	id := createFeed(t, router, host.URL+"/feed?key=private-token")

	if location := do(router, http.MethodGet, "/", "", nil).Header().Get("Location"); location != "/ui" {
		t.Errorf("/ redirects to %q, want /ui", location)
	}
	if location := do(router, http.MethodGet, "/ui", "", nil).Header().Get("Location"); location != "/ui/feeds" {
		t.Errorf("/ui redirects to %q, want /ui/feeds", location)
	}
	if location := do(router, http.MethodGet, "/ui/", "", nil).Header().Get("Location"); location != "/ui" {
		t.Errorf("/ui/ redirects to %q, want /ui", location)
	}

	recorder := do(router, http.MethodGet, "/ui/feeds", "", nil)
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, "Fake Show") || !strings.Contains(body, `action="/ui/feeds/`+id+`"`) {
		t.Fatalf("GET /ui/feeds = %d, want the feed and its settings form:\n%s", recorder.Code, body)
	}
	if !strings.Contains(body, `<a href="/ui/account">tester</a>`) || !strings.Contains(body, `action="/ui/logout"`) {
		t.Error("the header doesn't show who is signed in, and a way out")
	}
	if !strings.Contains(recorder.Header().Get("Content-Security-Policy"), "default-src 'none'") || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("headers = %v, want the UI's content security policy and no-store", recorder.Header())
	}
	// Without a processor, region diff is neither shown nor offered.
	if strings.Contains(body, "Region diff") {
		t.Error("region diff is offered without a processor")
	}
	// Nothing secret: the token, signed URLs, or the feed's own path and query.
	for _, secret := range []string{testToken, "sig=", "private-token", "/feed?"} {
		if strings.Contains(body, secret) {
			t.Errorf("the page shows %q", secret)
		}
	}

	// A title from the source is escaped, never markup.
	feed := storedFeed(t, store, id)
	feed.Title = `<script>alert(1)</script>`
	if err := store.UpdateFeed(context.Background(), &feed); err != nil {
		t.Fatal(err)
	}
	if body := do(router, http.MethodGet, "/ui/feeds", "", nil).Body.String(); strings.Contains(body, "<script>") || !strings.Contains(body, "&lt;script&gt;") {
		t.Error("the feed title isn't escaped")
	}
}

func TestUIEmptyFeedList(t *testing.T) {
	router, _ := newUITestRouter(t, nil)
	if body := do(router, http.MethodGet, "/ui/feeds", "", nil).Body.String(); !strings.Contains(body, "No feeds yet") {
		t.Errorf("empty list: %s", body)
	}
}

func TestUIChangesFeedSettings(t *testing.T) {
	router, store := newUITestRouter(t, regionDiffLike)
	id := createFeed(t, router, startPodcastHost(t).URL+"/feed")

	body := do(router, http.MethodGet, "/ui/feeds", "", nil).Body.String()
	if !strings.Contains(body, `name="region_diff"`) || !strings.Contains(body, "Default (off)") {
		t.Fatalf("region diff isn't offered with its default:\n%s", body)
	}

	code, location, _ := postForm(router, "/ui/feeds/"+id, url.Values{"region_diff": {"on"}, "prepare_ahead": {"off"}}, nil)
	if code != http.StatusSeeOther || location != "/ui/feeds?saved="+id+"#feed-"+id {
		t.Fatalf("POST = %d to %q, want 303 back to the list", code, location)
	}
	if feed := storedFeed(t, store, id); feed.RegionDiff != "on" || feed.PrepareAhead != "off" {
		t.Errorf("stored region diff %q, prepare ahead %q; want on, off", feed.RegionDiff, feed.PrepareAhead)
	}
	// A browser follows the redirect without its #fragment.
	page, _, _ := strings.Cut(location, "#")
	if body := do(router, http.MethodGet, page, "", nil).Body.String(); !strings.Contains(body, "Saved the settings of &#39;Fake Show&#39;.") {
		t.Errorf("no saved notice:\n%s", body)
	}

	// Back to the default; a field the form didn't send is left alone.
	if code, _, _ := postForm(router, "/ui/feeds/"+id, url.Values{"region_diff": {""}}, nil); code != http.StatusSeeOther {
		t.Fatalf("POST = %d", code)
	}
	if feed := storedFeed(t, store, id); feed.RegionDiff != "" || feed.PrepareAhead != "off" {
		t.Errorf("stored region diff %q, prepare ahead %q; want default, off", feed.RegionDiff, feed.PrepareAhead)
	}
}

func TestUIRefusesBadSettings(t *testing.T) {
	router, store := newUITestRouter(t, nil)
	id := createFeed(t, router, startPodcastHost(t).URL+"/feed")

	code, _, body := postForm(router, "/ui/feeds/"+id, url.Values{"prepare_ahead": {"maybe"}}, nil)
	if code != http.StatusBadRequest || !strings.Contains(body, "can only be Default, On or Off") {
		t.Errorf("bad value = %d:\n%s", code, body)
	}
	// Without a processor a region diff setting is ignored, not stored.
	if code, _, _ := postForm(router, "/ui/feeds/"+id, url.Values{"region_diff": {"on"}}, nil); code != http.StatusSeeOther {
		t.Errorf("region diff without a processor = %d, want 303", code)
	}
	if feed := storedFeed(t, store, id); feed.PrepareAhead != "" || feed.RegionDiff != "" {
		t.Errorf("stored %q, %q; want nothing changed", feed.PrepareAhead, feed.RegionDiff)
	}

	for _, target := range []string{"/ui/feeds/not-an-id", "/ui/feeds/" + uuid.NewString()} {
		if code, _, _ := postForm(router, target, url.Values{"prepare_ahead": {"on"}}, nil); code != http.StatusNotFound {
			t.Errorf("POST %s = %d, want 404", target, code)
		}
	}
}

// TestUIRefusesCrossOriginPosts: without sign-in, a page on another site
// mustn't be able to change settings through a visitor's browser.
func TestUIRefusesCrossOriginPosts(t *testing.T) {
	router, store := newUITestRouter(t, nil)
	id := createFeed(t, router, startPodcastHost(t).URL+"/feed")

	for _, headers := range []map[string]string{
		{"Sec-Fetch-Site": "cross-site"},
		{"Origin": "https://evil.example"},
	} {
		if code, _, _ := postForm(router, "/ui/feeds/"+id, url.Values{"prepare_ahead": {"on"}}, headers); code != http.StatusForbidden {
			t.Errorf("POST with %v = %d, want 403", headers, code)
		}
	}
	if feed := storedFeed(t, store, id); feed.PrepareAhead != "" {
		t.Errorf("a cross-origin post changed prepare ahead to %q", feed.PrepareAhead)
	}
	if code, _, _ := postForm(router, "/ui/feeds/"+id, url.Values{"prepare_ahead": {"on"}}, map[string]string{"Sec-Fetch-Site": "same-origin"}); code != http.StatusSeeOther {
		t.Errorf("same-origin POST = %d, want 303", code)
	}
}

func TestUIStaticFiles(t *testing.T) {
	router, _ := newUITestRouter(t, nil)
	recorder := do(router, http.MethodGet, "/ui/static/style.css", "", nil)
	if recorder.Code != http.StatusOK || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/css") || !strings.Contains(recorder.Body.String(), "--color-accent") {
		t.Errorf("style.css = %d %q", recorder.Code, recorder.Header().Get("Content-Type"))
	}
	for _, target := range []string{"/ui/static/missing.css", "/ui/static/..%2ftemplates%2flayout.html"} {
		if code := do(router, http.MethodGet, target, "", nil).Code; code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", target, code)
		}
	}
}

// refuseEveryone stands in for sign-in with nobody signed in.
type refuseEveryone struct{}

func (refuseEveryone) authenticate(context *gin.Context) (uiUser, bool) {
	context.Status(http.StatusUnauthorized)
	return uiUser{}, false
}

// TestUIAuthenticatorGuardsPages makes sure every page but the static files
// goes through the authenticator, so adding sign-in there covers them all,
// including pages added later.
func TestUIAuthenticatorGuardsPages(t *testing.T) {
	captureLog(t, logrus.DebugLevel)
	router := gin.New()
	if err := (&handlers{}).registerUI(router, "v1.2.3", refuseEveryone{}); err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, route := range router.Routes() {
		if !strings.HasPrefix(route.Path, uiPrefix) || strings.HasPrefix(route.Path, uiPrefix+"/static/") || strings.HasPrefix(route.Path, loginPath) {
			continue
		}
		target := strings.ReplaceAll(route.Path, ":feedID", uuid.NewString())
		if code := do(router, route.Method, target, "", nil).Code; code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d without a user, want the authenticator's 401", route.Method, route.Path, code)
		}
		checked++
	}
	if checked < 3 {
		t.Errorf("checked %d pages, want at least the feed list, its form and /ui/", checked)
	}
}

func TestUIInstancePage(t *testing.T) {
	router, _ := newUITestRouter(t, nil)
	createFeed(t, router, startPodcastHost(t).URL+"/feed")

	recorder := do(router, http.MethodGet, "/ui/instance", "", nil)
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /ui/instance = %d:\n%s", recorder.Code, body)
	}
	for _, want := range []string{
		`href="/ui/instance" aria-current="page"`,
		"v1.2.3",
		"28 Sep 2026 09:30",
		"<dt>Feeds</dt>",
		"<dd>1</dd>",
		// The exits page has the VPN module in full.
		`<a href="/ui/exits">2 exits, default norway: see Exits.</a>`,
		"Not set up: region_diff has no exits.",
		"Every 15 minutes",
		"Kept 14 days, no size cap",
		// Warned about, as worth a look.
		"External URL</dt>\n    <dd><span class=\"badge badge--warn\">Check</span>Not set",
		"Any address.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Exits norway over providers proton.") || strings.Contains(body, "Default exit") {
		t.Error("the instance page repeats what the exits page shows")
	}
	// Region diff's feed defaults only when the module runs.
	if strings.Contains(body, "An episode that can&#39;t be cleaned") {
		t.Error("region diff's failure policy shown without region diff")
	}
	for _, secret := range []string{testToken, "test-signing-key"} {
		if strings.Contains(body, secret) {
			t.Errorf("the page shows %q", secret)
		}
	}
}

// TestUIInstancePageNeverShowsVPNKeys: neither a key nor the env:/file:
// reference naming it reaches the page, whatever config.json holds.
func TestUIInstancePageNeverShowsVPNKeys(t *testing.T) {
	router, _ := newUITestRouterWith(t, nil, func(cfg *settings.Config) {
		cfg.VPN.Providers = map[string]settings.VPNProvider{"proton": {Type: "protonvpn", PrivateKeys: []string{"env:PROTON_KEY_1", "literal-private-key"}}}
	})
	body := do(router, http.MethodGet, "/ui/instance", "", nil).Body.String()
	for _, secret := range []string{"PROTON_KEY_1", "literal-private-key", "private_keys"} {
		if strings.Contains(body, secret) {
			t.Errorf("the page shows %q", secret)
		}
	}
}

func TestUIExitsPage(t *testing.T) {
	router, _ := newUITestRouter(t, nil)
	recorder := do(router, http.MethodGet, "/ui/exits", "", nil)
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /ui/exits = %d:\n%s", recorder.Code, body)
	}
	for _, want := range []string{
		`href="/ui/exits" aria-current="page"`,
		"as of ",
		// The exits, with where they go out, their server and tunnel.
		"This host&#39;s own connection", "Where this host is (NO)", "Not tunnelled",
		"NO only", `<span class="mono">NO#23 (NO)</span>`, `<span class="badge badge--on">Open</span> 2 users`, "Handshake 40 s ago · Key 1",
		"SE, then NO", "Not chosen yet", `<span class="badge badge--off">Closed</span> Opens on first use`,
		"Default exit",
		// The provider.
		"<h2>Provider proton</h2>", "603, from the built-in list, dated 2026-08-06", "1 key: each tunnel", "1 at once", "1: NO#23",
		"Left out after failing: SE#12 for ", // benched
		// The start-up tunnel check, and the setup moved here from the instance page.
		"Provider &#39;proton&#39; can hold 1 tunnel at once", "Direct exit",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q:\n%s", want, body)
		}
	}
}

func TestUIExitsPageWithoutVPN(t *testing.T) {
	router, _ := newSignInTestRouterPlain(t)
	body := do(router, http.MethodGet, "/ui/exits", "", nil).Body.String()
	if !strings.Contains(body, "None set up") || !strings.Contains(body, "No exits.") || strings.Contains(body, "Tunnels") {
		t.Errorf("without a VPN:\n%s", body)
	}
}

// newSignInTestRouterPlain is a signed-in router whose Instance is empty, as
// with no VPN.
func newSignInTestRouterPlain(t *testing.T) (http.Handler, *database.Store) {
	t.Helper()
	saved := testInstance
	testInstance = Instance{}
	t.Cleanup(func() { testInstance = saved })
	return newUITestRouter(t, nil)
}

func TestFormatDuration(t *testing.T) {
	for _, c := range []struct {
		duration time.Duration
		want     string
	}{
		{-time.Second, "0 s"},
		{40 * time.Second, "40 s"},
		{3*time.Minute + 59*time.Second, "3 min"},
		{2 * time.Hour, "2 h"},
		{5 * 24 * time.Hour, "5 days"},
	} {
		if got := formatDuration(c.duration); got != c.want {
			t.Errorf("formatDuration(%v) = %q, want %q", c.duration, got, c.want)
		}
	}
}

// totpAt is what an authenticator app shows for a base32 secret at a moment
// (RFC 6238: HMAC-SHA1, 6 digits, 30 s).
func totpAt(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		t.Fatal(err)
	}
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(at.Unix()/30))
	mac := hmac.New(sha1.New, key)
	mac.Write(counter[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	return fmt.Sprintf("%06d", (binary.BigEndian.Uint32(sum[offset:offset+4])&0x7fffffff)%1000000)
}

// cookieFrom is the value a response sets for a cookie, and the cookie.
func cookieFrom(t *testing.T, recorder *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}

func TestUISignIn(t *testing.T) {
	router, _, service, clock := newSignInTestRouter(t, nil, nil)
	ctx := context.Background()

	// Signed out, every page leads to signing in, and back afterwards.
	recorder := do(router, http.MethodGet, "/ui/exits", "", nil)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/ui/login?next=%2Fui%2Fexits" {
		t.Fatalf("signed out: %d to %q", recorder.Code, recorder.Header().Get("Location"))
	}
	if body := do(router, http.MethodGet, "/ui/login", "", nil).Body.String(); !strings.Contains(body, "Nobody can sign in yet") || strings.Contains(body, `class="site-nav"`) {
		t.Errorf("sign-in page with no users, or with the nav:\n%s", body)
	}

	_, oneTime, err := service.AddUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	form := func(values url.Values) string { return values.Encode() }
	headers := func(cookies ...*http.Cookie) map[string]string {
		h := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
		var parts []string
		for _, cookie := range cookies {
			parts = append(parts, cookie.Name+"="+cookie.Value)
		}
		if len(parts) > 0 {
			h["Cookie"] = strings.Join(parts, "; ")
		}
		return h
	}

	recorder = do(router, http.MethodPost, "/ui/login?next=%2Fui%2Fexits", form(url.Values{"username": {"alice"}, "password": {"wrong"}}), headers())
	if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Body.String(), "Wrong username or password.") {
		t.Errorf("wrong password: %d", recorder.Code)
	}

	// The one-time password: then choosing one's own.
	recorder = do(router, http.MethodPost, "/ui/login?next=%2Fui%2Fexits", form(url.Values{"username": {"Alice"}, "password": {oneTime}}), headers())
	signIn := cookieFrom(t, recorder, signInCookie)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/ui/login/password?next=%2Fui%2Fexits" || signIn == nil {
		t.Fatalf("one-time password: %d to %q", recorder.Code, recorder.Header().Get("Location"))
	}
	if !signIn.HttpOnly || signIn.SameSite != http.SameSiteLaxMode || signIn.Path != "/ui/login" || signIn.Secure {
		t.Errorf("sign-in cookie %+v", signIn)
	}
	recorder = do(router, http.MethodPost, "/ui/login/password?next=%2Fui%2Fexits", form(url.Values{"password": {"alice's password"}, "repeat": {"alice's password"}}), headers(signIn))
	session := cookieFrom(t, recorder, sessionCookie)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/ui/exits" || session == nil || !session.HttpOnly || session.Path != "/ui" {
		t.Fatalf("choosing a password: %d to %q, session %+v", recorder.Code, recorder.Header().Get("Location"), session)
	}
	if code := do(router, http.MethodGet, "/ui/exits", "", headers(session)).Code; code != http.StatusOK {
		t.Errorf("signed in: %d", code)
	}

	// Setting up an authenticator: a POST starts it, the page shows it.
	recorder = do(router, http.MethodPost, "/ui/account/totp/begin", "", headers(session))
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/ui/account/totp" {
		t.Fatalf("begin: %d", recorder.Code)
	}
	page := do(router, http.MethodGet, "/ui/account/totp", "", headers(session)).Body.String()
	user, _ := service.Users(ctx)
	secret, _, _, _ := service.PendingTOTP(ctx, user[0].ID)
	if !strings.Contains(page, `<svg class="qr"`) || !strings.Contains(page, `href="otpauth://totp/Solstein:alice?`) || strings.Contains(page, "ZgotmplZ") || !strings.Contains(page, secret[:4]+" "+secret[4:8]) {
		t.Fatalf("set-up page:\n%s", page)
	}
	code := totpAt(t, secret, clock.Now())
	if recorder := do(router, http.MethodPost, "/ui/account/totp", form(url.Values{"code": {code}}), headers(session)); recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/ui/account?done=totp-on" {
		t.Fatalf("confirm: %d", recorder.Code)
	}

	// Signing out ends the session.
	recorder = do(router, http.MethodPost, "/ui/logout", "", headers(session))
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/ui/login" || cookieFrom(t, recorder, sessionCookie).MaxAge >= 0 {
		t.Fatalf("sign out: %d", recorder.Code)
	}
	if code := do(router, http.MethodGet, "/ui/feeds", "", headers(session)).Code; code != http.StatusSeeOther {
		t.Errorf("after signing out: %d, want the redirect to sign in", code)
	}

	// Signing in again asks for a code; somewhere outside the UI isn't
	// where it goes afterwards.
	recorder = do(router, http.MethodPost, "/ui/login?next=https%3A%2F%2Fevil.example%2F", form(url.Values{"username": {"alice"}, "password": {"alice's password"}}), headers())
	signIn = cookieFrom(t, recorder, signInCookie)
	if recorder.Code != http.StatusSeeOther || !strings.HasPrefix(recorder.Header().Get("Location"), "/ui/login/totp?next=%2Fui%2Ffeeds") {
		t.Fatalf("password with TOTP on: %d to %q", recorder.Code, recorder.Header().Get("Location"))
	}
	if recorder := do(router, http.MethodPost, "/ui/login/totp", form(url.Values{"code": {"000000"}}), headers(signIn)); recorder.Code != http.StatusUnauthorized {
		t.Errorf("wrong code: %d", recorder.Code)
	}
	// The code used to confirm can't be used again: the next one.
	if recorder := do(router, http.MethodPost, "/ui/login/totp", form(url.Values{"code": {code}}), headers(signIn)); recorder.Code != http.StatusUnauthorized {
		t.Errorf("a code used again: %d", recorder.Code)
	}
	clock.advance(30 * time.Second)
	next := totpAt(t, secret, clock.Now())
	recorder = do(router, http.MethodPost, "/ui/login/totp", form(url.Values{"code": {next}}), headers(signIn))
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/ui/feeds" || cookieFrom(t, recorder, sessionCookie) == nil {
		t.Fatalf("right code: %d to %q", recorder.Code, recorder.Header().Get("Location"))
	}
}

func TestUISessionCookieSecureOverHTTPS(t *testing.T) {
	router, _, service, _ := newSignInTestRouter(t, nil, nil)
	_, oneTime, _ := service.AddUser(context.Background(), "alice")
	request := httptest.NewRequest(http.MethodPost, "https://solstein.example/ui/login", strings.NewReader(url.Values{"username": {"alice"}, "password": {oneTime}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if cookie := cookieFrom(t, recorder, signInCookie); cookie == nil || !cookie.Secure {
		t.Errorf("over HTTPS the cookie isn't Secure: %+v", cookie)
	}
}

func TestSafeNext(t *testing.T) {
	for next, want := range map[string]string{
		"/ui/exits":            "/ui/exits",
		"/ui/feeds?saved=x":    "/ui/feeds?saved=x",
		"":                     "/ui/feeds",
		"https://evil.example": "/ui/feeds",
		"//evil.example/ui/":   "/ui/feeds",
		"/ui/login":            "/ui/feeds",
		"/ui/\\evil":           "/ui/feeds",
		"/api/v1/feeds":        "/ui/feeds",
	} {
		if got := safeNext(next); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", next, got, want)
		}
	}
}

func TestUIAccount(t *testing.T) {
	router, _, service, clock := newSignInTestRouter(t, nil, nil)
	ctx := context.Background()
	session := signInTester(t, service)
	cookie := "solstein_session=" + session
	post := func(target string, values url.Values, cookie string) *httptest.ResponseRecorder {
		return do(router, http.MethodPost, target, values.Encode(), map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Cookie": cookie})
	}

	page := do(router, http.MethodGet, "/ui/account", "", map[string]string{"Cookie": cookie}).Body.String()
	if !strings.Contains(page, "Signed in as <span class=\"mono\">tester</span>") || !strings.Contains(page, "Set up an authenticator") {
		t.Fatalf("account page:\n%s", page)
	}

	if recorder := post("/ui/account/password", url.Values{"current": {"wrong"}, "password": {"a brand new one"}, "repeat": {"a brand new one"}}, cookie); recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "current password isn") {
		t.Errorf("wrong current password: %d", recorder.Code)
	}
	recorder := post("/ui/account/password", url.Values{"current": {testerPassword}, "password": {"a brand new one"}, "repeat": {"a brand new one"}}, cookie)
	replacement := cookieFrom(t, recorder, sessionCookie)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/ui/account?done=password" || replacement == nil {
		t.Fatalf("change password: %d", recorder.Code)
	}
	if code := do(router, http.MethodGet, "/ui/account", "", map[string]string{"Cookie": cookie}).Code; code != http.StatusSeeOther {
		t.Error("the old session survives a password change")
	}
	cookie = "solstein_session=" + replacement.Value
	if body := do(router, http.MethodGet, "/ui/account?done=password", "", map[string]string{"Cookie": cookie}).Body.String(); !strings.Contains(body, "Changed your password.") {
		t.Error("no notice after changing the password")
	}

	// An authenticator, then removed again with the password.
	post("/ui/account/totp/begin", nil, cookie)
	users, _ := service.Users(ctx)
	secret, _, _, _ := service.PendingTOTP(ctx, users[0].ID)
	post("/ui/account/totp", url.Values{"code": {totpAt(t, secret, clock.Now())}}, cookie)
	if body := do(router, http.MethodGet, "/ui/account", "", map[string]string{"Cookie": cookie}).Body.String(); !strings.Contains(body, "Remove authenticator") {
		t.Fatal("the authenticator isn't on")
	}
	if recorder := post("/ui/account/totp/disable", url.Values{"password": {"wrong"}}, cookie); recorder.Code != http.StatusBadRequest {
		t.Errorf("remove with a wrong password: %d", recorder.Code)
	}
	if recorder := post("/ui/account/totp/disable", url.Values{"password": {"a brand new one"}}, cookie); recorder.Header().Get("Location") != "/ui/account?done=totp-off" {
		t.Errorf("remove: %d %q", recorder.Code, recorder.Header().Get("Location"))
	}
	// Without a pending set-up, its page goes back to the account.
	if recorder := do(router, http.MethodGet, "/ui/account/totp", "", map[string]string{"Cookie": cookie}); recorder.Header().Get("Location") != "/ui/account" {
		t.Errorf("set-up page with nothing pending: %d %q", recorder.Code, recorder.Header().Get("Location"))
	}
}
