package server

import (
	"bytes"
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"aunefyren/solstein/database"
	"aunefyren/solstein/feeds"
	"aunefyren/solstein/logger"
	"aunefyren/solstein/models"
	"aunefyren/solstein/outbound"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// The web UI (docs/web-ui.md), built to docs/style-guide.md: server-rendered
// pages under /ui. Every change is a plain HTML form POST answered with a
// redirect, so it works without JavaScript and a reload never repeats it.

const (
	uiPrefix = "/ui"
	// uiUserKey is where the authenticated user is kept in the Gin context.
	uiUserKey = "uiUser"
	// uiContentSecurityPolicy allows the UI's own stylesheet, icon and
	// script (live updates, which fetch Solstein's own pages) and nothing
	// else: no inline script or style, no framing, forms only back to
	// Solstein.
	uiContentSecurityPolicy = "default-src 'none'; style-src 'self'; img-src 'self'; script-src 'self'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"
)

//go:embed web
var webFiles embed.FS

// uiUser is who is using the web UI.
type uiUser struct {
	// Subject identifies the user to the authorization server (OAuth 2.0 /
	// OpenID Connect "sub"); empty while there is no sign-in.
	Subject string
	Name    string
}

// Anonymous reports whether nobody is signed in.
func (user uiUser) Anonymous() bool {
	return user.Subject == ""
}

// uiAuthenticator decides who is using the web UI. Every UI page goes
// through it, so sign-in is added in this one place: an OAuth 2.0
// authorization-code flow with PKCE run on the server would, finding no
// session, redirect to the authorization server here, and serve its
// callback under /ui/auth/ (docs/wip.md, Web UI).
type uiAuthenticator interface {
	// authenticate returns the request's user. When there is none, it has
	// answered the request itself (a redirect to sign in, or 401) and
	// returns false.
	authenticate(context *gin.Context) (uiUser, bool)
}

// ui serves the web UI's pages.
type ui struct {
	handlers    *handlers
	auth        uiAuthenticator
	version     string
	pages       map[string]*template.Template
	static      fs.FS
	crossOrigin *http.CrossOriginProtection
}

// uiNotice is the outcome of the last action, shown at the top of a page.
type uiNotice struct {
	Kind string // "ok" or "error", as in the style guide's notice--*
	Text string
}

// uiPage is what the layout template renders; Content is the page's own.
type uiPage struct {
	Nav     string
	Version string
	User    uiUser
	Notice  *uiNotice
	Content any
	// Refresh is where the page refreshes itself to without JavaScript
	// while its live region is active; empty for no refresh.
	Refresh string
}

// liveInterval is how often a live region updates (style guide: Live
// region); the <noscript> refresh uses it too, in seconds.
const liveInterval = 5 * time.Second

// uiLive is a page's live region: whether work is in progress, where to
// fetch it from, and what the status line says is going on.
type uiLive struct {
	Active   bool
	URL      string
	StopURL  string
	Note     string
	Interval int // milliseconds, for live.js
	Seconds  int
}

// newLive describes a live region at url (a path, with its query if any):
// active only while busy says there is something in progress, and the
// viewer hasn't stopped it (?live=off, the <noscript> Stop link).
func newLive(context *gin.Context, url string, busy bool, note string) uiLive {
	separator := "?"
	if strings.Contains(url, "?") {
		separator = "&"
	}
	return uiLive{
		Active:   busy && context.Query("live") != "off",
		URL:      url,
		StopURL:  url + separator + "live=off",
		Note:     note,
		Interval: int(liveInterval / time.Millisecond),
		Seconds:  int(liveInterval / time.Second),
	}
}

// registerUI adds the web UI's routes, every page behind auth. Only called
// when web_ui.enabled is on, so a disabled UI parses no templates and has no
// routes.
func (handlers *handlers) registerUI(router *gin.Engine, version string, auth uiAuthenticator) error {
	ui, err := newUI(handlers, auth, version)
	if err != nil {
		return err
	}
	router.GET("/", func(context *gin.Context) {
		context.Redirect(http.StatusFound, uiPrefix)
	})
	// The stylesheet and icon need no user: a sign-in page may need them.
	router.GET(uiPrefix+"/static/:file", ui.securityHeaders, ui.staticFile)

	// Signing in: the only pages without a user.
	signIn := router.Group(loginPath, ui.securityHeaders, ui.sameOrigin)
	signIn.GET("", ui.loginForm)
	signIn.POST("", ui.loginSubmit)
	signIn.GET("/password", ui.setPasswordForm)
	signIn.POST("/password", ui.setPasswordSubmit)
	signIn.GET("/totp", ui.totpForm)
	signIn.POST("/totp", ui.totpSubmit)

	pages := router.Group(uiPrefix, ui.securityHeaders, ui.sameOrigin, ui.requireUser)
	// "/ui" rather than "/ui/": Gin sends "/ui/" here anyway.
	pages.GET("", func(context *gin.Context) {
		context.Redirect(http.StatusFound, uiPrefix+"/feeds")
	})
	pages.GET("/feeds", ui.feedList)
	pages.POST("/feeds/:feedID", ui.feedSettings)
	pages.GET("/feeds/:feedID", ui.feedPage)
	pages.POST("/feeds/:feedID/retry", ui.retryFeed)
	pages.POST("/feeds/:feedID/prepare", ui.prepareFeed)
	pages.POST("/feeds/:feedID/rules", ui.feedRules)
	pages.POST("/feeds/:feedID/episodes/:episodeID/queue", ui.queueEpisode)
	pages.POST("/feeds/:feedID/episodes/:episodeID/serve", ui.serveEpisode)
	pages.POST("/feeds/:feedID/episodes/:episodeID/delete", ui.deleteEpisode)
	pages.GET("/instance", ui.instancePage)
	pages.GET("/exits", ui.exitsPage)
	pages.GET("/account", ui.accountPage)
	pages.POST("/account/password", ui.changePassword)
	pages.POST("/account/totp/begin", ui.beginTOTP)
	pages.GET("/account/totp", ui.totpSetupPage)
	pages.POST("/account/totp", ui.confirmTOTP)
	pages.POST("/account/totp/disable", ui.disableTOTP)
	pages.POST("/logout", ui.logout)
	return nil
}

func newUI(handlers *handlers, auth uiAuthenticator, version string) (*ui, error) {
	static, err := fs.Sub(webFiles, "web/static")
	if err != nil {
		return nil, err
	}
	funcs := template.FuncMap{"setting": newSettingField, "ruleActions": func() []uiRuleAction { return uiRuleActions }}
	pages := map[string]*template.Template{}
	for _, page := range []string{"feeds", "feed", "instance", "exits", "login", "account", "totp", "error"} {
		parsed, err := template.New(page).Funcs(funcs).ParseFS(webFiles, "web/templates/layout.html", "web/templates/facts.html", "web/templates/settings.html", "web/templates/live.html", "web/templates/"+page+".html")
		if err != nil {
			return nil, err
		}
		pages[page] = parsed
	}
	return &ui{
		handlers:    handlers,
		auth:        auth,
		version:     version,
		pages:       pages,
		static:      static,
		crossOrigin: http.NewCrossOriginProtection(),
	}, nil
}

func (ui *ui) securityHeaders(context *gin.Context) {
	header := context.Writer.Header()
	header.Set("Content-Security-Policy", uiContentSecurityPolicy)
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Frame-Options", "DENY")
	// same-origin, not no-referrer: under no-referrer a browser sends
	// "Origin: null" on a form post, and over plain HTTP (no Sec-Fetch-Site)
	// the cross-origin check then refuses every form of the UI. A page's
	// address still never goes to another site.
	header.Set("Referrer-Policy", "same-origin")
	context.Next()
}

func (ui *ui) requireUser(context *gin.Context) {
	user, ok := ui.auth.authenticate(context)
	if !ok {
		context.Abort()
		return
	}
	context.Set(uiUserKey, user)
	context.Next()
}

// sameOrigin refuses a form posted from another site: without sign-in, a
// page elsewhere could otherwise change settings through the browser of
// someone on an allowed network, and with a session cookie later it is what
// CSRF protection needs anyway.
func (ui *ui) sameOrigin(context *gin.Context) {
	if err := ui.crossOrigin.Check(context.Request); err != nil {
		logger.Log.Warn("Refused a cross-origin " + context.Request.Method + " to the web UI from " + context.ClientIP() + ".")
		ui.renderError(context, http.StatusForbidden, "Not allowed", "This change came from another site, so it wasn't made.")
		return
	}
	context.Next()
}

func (ui *ui) staticFile(context *gin.Context) {
	name := path.Base(context.Param("file"))
	data, err := fs.ReadFile(ui.static, name)
	if err != nil {
		context.Status(http.StatusNotFound)
		context.Abort()
		return
	}
	// Links carry ?v=<version>, so a new release is fetched fresh.
	context.Header("Cache-Control", "public, max-age=86400")
	http.ServeContent(context.Writer, context.Request, name, time.Time{}, bytes.NewReader(data))
}

func (ui *ui) render(context *gin.Context, status int, page, nav string, notice *uiNotice, content any) {
	user, _ := context.Get(uiUserKey)
	current, _ := user.(uiUser)
	var buffer bytes.Buffer
	data := uiPage{Nav: nav, Version: ui.version, User: current, Notice: notice, Content: content}
	if live, ok := content.(interface{ live() uiLive }); ok && live.live().Active {
		data.Refresh = live.live().URL
	}
	if err := ui.pages[page].ExecuteTemplate(&buffer, "layout", data); err != nil {
		logger.Log.Error("Failed to render the web UI's " + page + " page. Error: " + err.Error())
		context.String(http.StatusInternalServerError, "Failed to show the page.")
		context.Abort()
		return
	}
	context.Header("Cache-Control", "no-store")
	context.Data(status, "text/html; charset=utf-8", buffer.Bytes())
}

type uiError struct {
	Heading string
	Text    string
}

func (ui *ui) renderError(context *gin.Context, status int, heading, text string) {
	ui.render(context, status, "error", "", nil, uiError{Heading: heading, Text: text})
	context.Abort()
}

// uiToggle is a per-feed on/off override as the feed list shows it.
type uiToggle struct {
	// Value is the feed's own setting: "on", "off", or empty for the default.
	Value string
	// DefaultOn is what the global setting gives, for "Default (on)".
	DefaultOn bool
	// InUse is what applies to the feed, its setting and the global combined.
	InUse bool
}

type uiFeed struct {
	ID           string
	Title        string
	SourceHost   string
	Exit         string
	DeliveryMode string
	LastPolled   string
	Failing      bool
	RegionDiff   uiToggle
	PrepareAhead uiToggle
	// ServeDropped and DeleteDropped are what happens to the episodes the
	// source no longer lists.
	ServeDropped, DeleteDropped uiToggle
}

type uiFeedList struct {
	Feeds               []uiFeed
	RegionDiffAvailable bool
}

// uiSettingField is what the feed list's "setting" template renders: one
// labelled select for one feed's override.
type uiSettingField struct {
	Name, Label, FeedID string
	uiToggle
}

func newSettingField(name, label, feedID string, toggle uiToggle) uiSettingField {
	return uiSettingField{Name: name, Label: label, FeedID: feedID, uiToggle: toggle}
}

func (ui *ui) feedList(context *gin.Context) {
	ui.showFeeds(context, http.StatusOK, ui.savedNotice(context))
}

func (ui *ui) showFeeds(context *gin.Context, status int, notice *uiNotice) {
	list, err := ui.handlers.feeds.List(context.Request.Context())
	if err != nil {
		logger.Log.Error("Failed to list feeds for the web UI. Error: " + err.Error())
		ui.renderError(context, http.StatusInternalServerError, "Couldn't load the feeds", "Something went wrong; the log says what.")
		return
	}
	service := ui.handlers.feeds
	content := uiFeedList{RegionDiffAvailable: service.RegionDiffAvailable()}
	for _, feed := range list {
		content.Feeds = append(content.Feeds, uiFeedOf(service, feed))
	}
	ui.render(context, status, "feeds", "feeds", notice, content)
}

// savedNotice is the notice after a save redirected back (?saved=<feed ID>).
func (ui *ui) savedNotice(context *gin.Context) *uiNotice {
	id, err := uuid.Parse(context.Query("saved"))
	if err != nil {
		return nil
	}
	feed, err := ui.handlers.feeds.Feed(context.Request.Context(), id)
	if err != nil {
		return nil
	}
	return &uiNotice{Kind: "ok", Text: "Saved the settings of '" + feedTitle(feed) + "'."}
}

func uiFeedOf(service *feeds.Service, feed models.Feed) uiFeed {
	defaults := feed
	defaults.RegionDiff, defaults.PrepareAhead, defaults.ServeDropped, defaults.DeleteDropped = "", "", "", ""
	exit := feed.Exit
	if exit == "" {
		exit = "default"
	}
	return uiFeed{
		ID:            feed.ID.String(),
		Title:         feedTitle(feed),
		SourceHost:    sourceHost(feed.SourceURL),
		Exit:          exit,
		DeliveryMode:  service.DeliveryMode(feed),
		LastPolled:    formatTime(feed.LastPolledAt),
		Failing:       feed.LastError != "",
		RegionDiff:    uiToggle{Value: feed.RegionDiff, DefaultOn: service.Processed(defaults), InUse: service.Processed(feed)},
		PrepareAhead:  uiToggle{Value: feed.PrepareAhead, DefaultOn: service.PreparesAhead(defaults), InUse: service.PreparesAhead(feed)},
		ServeDropped:  uiToggle{Value: feed.ServeDropped, DefaultOn: service.FeedServesDropped(defaults), InUse: service.FeedServesDropped(feed)},
		DeleteDropped: uiToggle{Value: feed.DeleteDropped, DefaultOn: service.DeletesDropped(defaults), InUse: service.DeletesDropped(feed)},
	}
}

// settingsFailed shows the page the settings form was on again, with what
// went wrong.
func (ui *ui) settingsFailed(context *gin.Context, notice *uiNotice) {
	if context.PostForm("from") == "feed" {
		ui.showFeedPage(context, http.StatusBadRequest, notice)
		return
	}
	ui.showFeeds(context, http.StatusBadRequest, notice)
}

// uiFormField is one on/off override a form can set, and the feed field it
// goes into.
type uiFormField struct {
	name   string
	target *string
}

// feedSettings saves a feed's overrides from the feed list's form. Only the
// fields the form sent are changed; region diff only when it is available.
func (ui *ui) feedSettings(context *gin.Context) {
	id, err := uuid.Parse(context.Param("feedID"))
	if err != nil {
		ui.renderError(context, http.StatusNotFound, "No such feed", "This feed doesn't exist; it may have been removed.")
		return
	}
	feed, err := ui.handlers.feeds.Feed(context.Request.Context(), id)
	if errors.Is(err, database.ErrFeedNotFound) {
		ui.renderError(context, http.StatusNotFound, "No such feed", "This feed doesn't exist; it may have been removed.")
		return
	}
	if err != nil {
		logger.Log.Error("Failed to load feed " + id.String() + " for the web UI. Error: " + err.Error())
		ui.renderError(context, http.StatusInternalServerError, "Couldn't load the feed", "Something went wrong; the log says what.")
		return
	}

	fields := []uiFormField{{"prepare_ahead", &feed.PrepareAhead}, {"serve_dropped", &feed.ServeDropped}, {"delete_dropped", &feed.DeleteDropped}}
	if ui.handlers.feeds.RegionDiffAvailable() {
		fields = append(fields, uiFormField{"region_diff", &feed.RegionDiff})
	}
	for _, field := range fields {
		value, sent := context.GetPostForm(field.name)
		if !sent {
			continue
		}
		if value != "" && value != "on" && value != "off" {
			ui.settingsFailed(context, &uiNotice{Kind: "error", Text: "Couldn't save the settings of '" + feedTitle(feed) + "': a setting can only be Default, On or Off."})
			return
		}
		*field.target = value
	}

	err = ui.handlers.updateFeed(context.Request.Context(), &feed)
	if errors.Is(err, feeds.ErrInvalidSettings) {
		ui.settingsFailed(context, &uiNotice{Kind: "error", Text: "Couldn't save the settings of '" + feedTitle(feed) + "': " + err.Error() + "."})
		return
	}
	if err != nil {
		logger.Log.Error("Failed to update feed '" + feed.Title + "' from the web UI. Error: " + err.Error())
		ui.renderError(context, http.StatusInternalServerError, "Couldn't save the settings", "Something went wrong; the log says what.")
		return
	}
	logger.Log.Info("Changed the settings of feed '" + feed.Title + "' through the web UI (region diff: " + orDefault(feed.RegionDiff) + ", prepare ahead: " + orDefault(feed.PrepareAhead) + ", serve dropped: " + orDefault(feed.ServeDropped) + ", delete dropped: " + orDefault(feed.DeleteDropped) + ").")
	if context.PostForm("from") == "feed" {
		context.Redirect(http.StatusSeeOther, feedPagePath(feed)+"?done=saved")
		return
	}
	context.Redirect(http.StatusSeeOther, uiPrefix+"/feeds?saved="+feed.ID.String()+"#feed-"+feed.ID.String())
}

func feedTitle(feed models.Feed) string {
	if feed.Title != "" {
		return feed.Title
	}
	return sourceHost(feed.SourceURL)
}

// sourceHost is the feed's host only: the path and query of a private
// feed's URL can carry its access token.
func sourceHost(sourceURL string) string {
	parsed, err := url.Parse(sourceURL)
	if err != nil || parsed.Host == "" {
		return "unknown host"
	}
	return parsed.Host
}

func formatTime(at *time.Time) string {
	if at == nil || at.IsZero() {
		return "never"
	}
	return at.Local().Format("2 Jan 2006 15:04")
}

// orDefault names an on/off override for the log: "on", "off" or
// "default". The values come from a form, and were checked before saving,
// but the name is always a constant, so no text a request sent reaches the
// log (which code scanning can see, unlike the check).
func orDefault(value string) string {
	switch value {
	case "on":
		return "on"
	case "off":
		return "off"
	}
	return "default"
}

// uiFact is one row of a fact list (style guide: Fact list).
type uiFact struct {
	Label string
	Value string
	// Badge is the status badge's modifier ("on", "off", "warn"), empty for
	// none; BadgeText is its word.
	Badge, BadgeText string
	Mono             bool
	// Href links the value to the page that has it in full.
	Href string
}

type uiSection struct {
	Heading string
	Facts   []uiFact
}

type uiInstance struct {
	Sections []uiSection
}

func onFact(label string, on bool, onText, offText string) uiFact {
	if on {
		return uiFact{Label: label, Badge: "on", BadgeText: "On", Value: onText}
	}
	return uiFact{Label: label, Badge: "off", BadgeText: "Off", Value: offText}
}

func warnFact(label, value string) uiFact {
	return uiFact{Label: label, Badge: "warn", BadgeText: "Check", Value: value}
}

// listFact shows names from config.json, or empty in words.
func listFact(label string, values []string, empty string) uiFact {
	if len(values) == 0 {
		return uiFact{Label: label, Value: empty}
	}
	return uiFact{Label: label, Value: strings.Join(values, ", "), Mono: true}
}

// instancePage shows this Solstein: what it runs, its modules and exits, the
// defaults feeds follow, and who may reach it. Read-only, and nothing secret:
// no token, signing key or VPN key, nor the env:/file: references to them.
func (ui *ui) instancePage(context *gin.Context) {
	cfg, instance := ui.handlers.config, ui.handlers.instance

	running := []uiFact{{Label: "Version", Value: ui.version, Mono: true}}
	if !instance.StartedAt.IsZero() {
		running = append(running, uiFact{Label: "Running since", Value: formatTime(&instance.StartedAt)})
	}
	timezone := uiFact{Label: "Time zone", Value: cfg.Timezone, Mono: true}
	if cfg.Timezone == "" {
		timezone = uiFact{Label: "Time zone", Value: "The system's (" + time.Local.String() + ")"}
	}
	running = append(running, timezone)
	if cfg.ExternalURL == "" {
		running = append(running, warnFact("External URL", "Not set: rewritten feeds can't point clients back at Solstein. Set external_url."))
	} else {
		running = append(running, uiFact{Label: "External URL", Value: cfg.ExternalURL, Mono: true})
	}
	if list, err := ui.handlers.feeds.List(context.Request.Context()); err != nil {
		logger.Log.Error("Failed to count feeds for the web UI. Error: " + err.Error())
		running = append(running, uiFact{Label: "Feeds", Value: "Couldn't count them; the log says why."})
	} else {
		running = append(running, uiFact{Label: "Feeds", Value: strconv.Itoa(len(list))})
	}

	var modules []uiFact
	for _, module := range instance.Modules {
		fact := onFact(module.Name, module.On, module.Summary, module.Summary)
		if module.Key == ModuleExits && module.On {
			// The exits page has it in full.
			fact.Value = plural(len(instance.Exits), "exit") + ", default " + instance.DefaultExit + ": see Exits."
			fact.Href = uiPrefix + "/exits"
		}
		modules = append(modules, fact)
	}
	modules = append(modules, ui.webUIFact(context))

	defaults := []uiFact{
		{Label: "Delivery mode", Value: cfg.DeliveryMode, Mono: true},
		{Label: "Polling", Value: "Every " + strconv.Itoa(cfg.PollIntervalMinutes) + " minutes"},
		onFact("Prepare ahead", cfg.PrepareAhead, "Every episode is prepared before a client asks, and published once ready.", "Episodes are prepared when a feed is polled, and a backlog episode when a client asks for it."),
		onFact("Serve dropped episodes", cfg.ServeDroppedEpisodes, "Episodes a source no longer lists stay in the feed, and are kept for good, cached file and all.", "Episodes a source no longer lists leave the feed too."),
		onFact("Delete dropped episodes", cfg.DeleteDroppedEpisodes, "Episodes a source no longer lists, and that aren't served, are deleted a day later, cached file and all.", "Episodes a source no longer lists are kept, in case it lists them again."),
	}
	if ui.handlers.feeds.RegionDiffAvailable() {
		defaults = append(defaults, onFact("Region diff", cfg.RegionDiff.Enabled, "On for every feed that doesn't switch it off.", "Off, except for feeds that switch it on."))
		failure := "Published with its ads."
		if cfg.RegionDiff.OnFailure == "hide" {
			failure = "Kept out of the feed, and tried again slowly."
		}
		defaults = append(defaults, uiFact{Label: "An episode that can't be cleaned", Value: failure})
	}
	defaults = append(defaults,
		uiFact{Label: "Client wait", Value: "Up to " + strconv.Itoa(cfg.ProcessingWaitSeconds) + " seconds for an episode being prepared, then 503"},
		uiFact{Label: "Cache", Value: cacheDescription(cfg.CacheRetentionDays, cfg.CacheMaxSizeMB)},
		onFact("Skip tracking redirects", cfg.SkipTrackingRedirects, "Episodes come from the audio host directly.", "Tracking redirects are followed; one that fails is skipped."),
	)

	var access []uiFact
	if cfg.DisableAuth {
		access = append(access, warnFact("Token and signed URLs", "Off (disable_auth): anyone who can reach Solstein can subscribe through it."))
	} else {
		access = append(access, uiFact{Label: "Token and signed URLs", Badge: "on", BadgeText: "On", Value: "Required for subscribing, the API, and every feed and episode URL."})
	}
	if len(cfg.AllowedClientNetworks) == 0 {
		access = append(access, warnFact("Allowed client networks", "Any address."))
	} else {
		access = append(access, listFact("Allowed client networks", cfg.AllowedClientNetworks, ""))
	}
	access = append(access,
		listFact("Trusted proxies", cfg.TrustedProxies, "None"),
		listFact("Allowed source hosts", cfg.AllowedSourceHosts, "Any host"),
	)
	if cfg.AllowPrivateDestinations {
		access = append(access, warnFact("Private destinations", "Allowed: Solstein can fetch from loopback and internal addresses."))
	} else {
		access = append(access, uiFact{Label: "Private destinations", Value: "Refused"})
	}

	ui.render(context, http.StatusOK, "instance", "instance", nil, uiInstance{Sections: []uiSection{
		{Heading: "Solstein", Facts: running},
		{Heading: "Modules", Facts: modules},
		{Heading: "Defaults for feeds", Facts: defaults},
		{Heading: "Access", Facts: access},
	}})
}

func cacheDescription(retentionDays, maxSizeMB int) string {
	description := "Kept " + strconv.Itoa(retentionDays) + " days"
	if maxSizeMB > 0 {
		return description + ", at most " + strconv.Itoa(maxSizeMB) + " MB"
	}
	return description + ", no size cap"
}

// uiExit is one row of the exits page's table.
type uiExit struct {
	Name     string
	Provider string
	Roles    []string
	Where    string
	Server   string
	Tunnel   *uiTunnel
	Direct   bool
}

// uiTunnel is an open tunnel as the exits page shows it.
type uiTunnel struct {
	Use       string // "2 in use", "Idle 3 min"
	Handshake string // "Handshake 40 s ago"
	Key       string // "Key 1"
}

type uiExitsPage struct {
	AsOf     string
	Exits    []uiExit
	Sections []uiSection
	// Setup is the part that doesn't change while Solstein runs; Sections
	// (the providers) update live with the exits table while a tunnel is
	// open.
	Setup   uiSection
	Summary string
	Live    uiLive
}

func (page uiExitsPage) live() uiLive { return page.Live }

// exitsPage shows the exits and VPN tunnels as they are right now: where
// each exit comes out, its server and tunnel, what uses it, each provider's
// servers, keys, tunnels and benched servers, and whether there are enough
// tunnels for every exit in use at once. Read-only; keys are counted and
// numbered, never shown.
func (ui *ui) exitsPage(context *gin.Context) {
	cfg, instance := ui.handlers.config, ui.handlers.instance
	var status outbound.Status
	if instance.ExitStatus != nil {
		status = instance.ExitStatus()
	}
	// As of when the status was taken, so ages are never understated.
	now := time.Now()

	roles := map[string][]string{}
	if instance.DefaultExit != "" {
		roles[instance.DefaultExit] = append(roles[instance.DefaultExit], "Default exit")
	}
	if ui.handlers.feeds.RegionDiffAvailable() {
		for i, exit := range cfg.RegionDiff.Exits {
			role := "Region diff home"
			if i > 0 {
				role = "Region diff partner"
			}
			roles[exit] = append(roles[exit], role)
		}
		for _, exit := range cfg.RegionDiff.FallbackExits {
			roles[exit] = append(roles[exit], "Region diff fallback")
		}
	}
	if list, err := ui.handlers.feeds.List(context.Request.Context()); err != nil {
		logger.Log.Error("Failed to list feeds for the web UI. Error: " + err.Error())
	} else {
		named := map[string]int{}
		for _, feed := range list {
			if feed.Exit != "" {
				named[feed.Exit]++
			}
		}
		for exit, count := range named {
			roles[exit] = append(roles[exit], plural(count, "feed"))
		}
	}

	var rows []uiExit
	for _, exit := range status.Exits {
		row := uiExit{Name: exit.Name, Provider: exit.Provider, Roles: roles[exit.Name]}
		switch {
		case exit.Provider == "":
			row.Direct = true
			row.Provider = "This host's own connection"
			row.Where = "Where this host is"
			if exit.Country != "" {
				row.Where += " (" + exit.Country + ")"
			}
			row.Server = "No VPN"
		default:
			row.Where = whereDescription(exit)
			row.Server = "Not chosen yet: no connection through it since start-up"
			if exit.Server != "" {
				row.Server = exit.Server
				if exit.Country != "" {
					row.Server += " (" + exit.Country + ")"
				}
			}
			if exit.Tunnel != nil {
				row.Tunnel = tunnelDescription(*exit.Tunnel, now)
			}
		}
		rows = append(rows, row)
	}

	var sections []uiSection
	if !instance.VPN {
		sections = append(sections, uiSection{Heading: "VPN", Facts: []uiFact{{Label: "Providers", Value: "None set up: config.json has no vpn providers, or none could be loaded."}}})
	}
	for _, provider := range status.Providers {
		sections = append(sections, uiSection{Heading: "Provider " + provider.Name, Facts: providerFacts(provider, now)})
	}

	setup := []uiFact{{Label: "Default exit", Value: instance.DefaultExit, Mono: true}}
	if instance.DefaultExit == "" {
		setup[0] = uiFact{Label: "Default exit", Value: "Unknown"}
	}
	directSummary := cfg.DirectExitSummary()
	if directSummary == "" {
		directSummary = "Off"
	}
	setup = append(setup, onFact("Direct exit", !cfg.DirectExitOff(), "This host's own connection can be used (direct_exit: "+cfg.DirectExit+").", directSummary))
	if instance.VPN {
		if len(instance.TunnelBudget) == 0 {
			setup = append(setup, uiFact{Label: "Tunnels", Badge: "on", BadgeText: "Enough", Value: "Every exit that can be in use at once can have its own tunnel."})
		}
		for _, warning := range instance.TunnelBudget {
			// Written for the log, after "VPN: ": a sentence of its own here.
			setup = append(setup, warnFact("Tunnels", sentenceCase(warning)))
		}
	}
	open := 0
	for _, provider := range status.Providers {
		open += len(provider.Tunnels)
	}
	summary := "No tunnels open."
	if open > 0 {
		summary = plural(open, "tunnel") + " open."
	}
	verb := " are open"
	if open == 1 {
		verb = " is open"
	}
	ui.render(context, http.StatusOK, "exits", "exits", nil, uiExitsPage{
		AsOf:     now.Format("15:04:05"),
		Exits:    rows,
		Sections: sections,
		Setup:    uiSection{Heading: "Setup", Facts: setup},
		Summary:  summary,
		Live:     newLive(context, uiPrefix+"/exits", open > 0, plural(open, "tunnel")+verb),
	})
}

// whereDescription is an exit's locations in words: "NO only" when strict,
// otherwise the list it works down.
func whereDescription(exit outbound.ExitStatus) string {
	if len(exit.Locations) == 0 {
		return "Anywhere the provider allows"
	}
	if exit.Strict {
		return exit.Locations[0] + " only"
	}
	return strings.Join(exit.Locations, ", then ")
}

func tunnelDescription(tunnel outbound.TunnelStatus, now time.Time) *uiTunnel {
	described := &uiTunnel{Use: plural(tunnel.Users, "user")}
	if tunnel.Users == 0 {
		described.Use = "Idle " + formatDuration(tunnel.IdleFor)
	}
	described.Handshake = "No handshake yet"
	if !tunnel.LastHandshake.IsZero() {
		described.Handshake = "Handshake " + formatDuration(now.Sub(tunnel.LastHandshake)) + " ago"
	}
	if tunnel.Key > 0 {
		described.Key = "Key " + strconv.Itoa(tunnel.Key)
	}
	return described
}

func providerFacts(provider outbound.ProviderStatus, now time.Time) []uiFact {
	servers := strconv.Itoa(provider.Servers)
	if provider.ServerList != "" {
		servers += ", from the " + provider.ServerList
	}
	facts := []uiFact{
		{Label: "Type", Value: provider.Type, Mono: true},
		{Label: "Servers", Value: servers},
	}
	switch {
	case provider.Keys > 0:
		facts = append(facts, uiFact{Label: "Keys", Value: plural(provider.Keys, "key") + ": each tunnel open at the same time needs its own."})
	default:
		facts = append(facts, uiFact{Label: "Keys", Value: "Each server's own, from its .conf file."})
	}
	limit := "No limit"
	if provider.MaxTunnels > 0 {
		limit = strconv.Itoa(provider.MaxTunnels) + " at once"
	}
	facts = append(facts, uiFact{Label: "Tunnel limit", Value: limit})

	open := uiFact{Label: "Open tunnels", Value: "None: tunnels open on first use and close after 5 min idle."}
	if len(provider.Tunnels) > 0 {
		var names []string
		for _, tunnel := range provider.Tunnels {
			names = append(names, tunnel.Server)
		}
		open = uiFact{Label: "Open tunnels", Value: strconv.Itoa(len(provider.Tunnels)) + ": " + strings.Join(names, ", ")}
	}
	facts = append(facts, open)

	if len(provider.Benched) == 0 {
		facts = append(facts, uiFact{Label: "Benched servers", Value: "None"})
	} else {
		var benched []string
		for _, server := range provider.Benched {
			benched = append(benched, server.Server+" for "+formatDuration(server.Until.Sub(now))+" more")
		}
		facts = append(facts, warnFact("Benched servers", "Left out after failing: "+strings.Join(benched, ", ")+". The log says why."))
	}
	return facts
}

// formatDuration is a duration in its largest whole unit, as the style guide
// has it: "40 s", "3 min", "2 h", "5 days".
func formatDuration(duration time.Duration) string {
	switch {
	case duration < time.Minute:
		return strconv.Itoa(int(max(duration, 0)/time.Second)) + " s"
	case duration < time.Hour:
		return strconv.Itoa(int(duration/time.Minute)) + " min"
	case duration < 48*time.Hour:
		return strconv.Itoa(int(duration/time.Hour)) + " h"
	}
	return strconv.Itoa(int(duration/(24*time.Hour))) + " days"
}

func plural(count int, noun string) string {
	if count == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(count) + " " + noun + "s"
}

// sentenceCase starts a message written to follow a log prefix with a
// capital, so it reads as a sentence of its own.
func sentenceCase(message string) string {
	if message == "" {
		return message
	}
	return strings.ToUpper(message[:1]) + message[1:]
}

// webUIFact is the web UI's own line on the instance page: who can sign in.
func (ui *ui) webUIFact(context *gin.Context) uiFact {
	users, err := ui.handlers.auth.Users(context.Request.Context())
	if err != nil {
		logger.Log.Error("Failed to list web UI users. Error: " + err.Error())
		return uiFact{Label: "Web UI", Badge: "on", BadgeText: "On", Value: "With sign-in; the users couldn't be counted (the log says why)."}
	}
	withTOTP := 0
	for _, user := range users {
		if user.TOTPEnabled {
			withTOTP++
		}
	}
	return uiFact{Label: "Web UI", Badge: "on", BadgeText: "On", Value: "With sign-in: " + plural(len(users), "user") + ", " + strconv.Itoa(withTOTP) + " with an authenticator. Users are added and reset on the console (solstein user)."}
}
