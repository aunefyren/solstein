package server

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"aunefyren/solstein/database"
	"aunefyren/solstein/episodes"
	"aunefyren/solstein/feeds"
	"aunefyren/solstein/logger"
	"aunefyren/solstein/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// The feed page (docs/web-ui.md): one feed's episodes, what became of each,
// and retrying or preparing them.

// feedPageSize is how many episodes the page shows before "Show all".
const feedPageSize = 50

// uiEpisode is one row of the feed page's episode table.
type uiEpisode struct {
	ID        string
	Title     string
	Published string
	Length    string
	Backlog   bool
	// Badge and BadgeText are the status badge; Detail says what it means.
	Badge, BadgeText, Detail string
	// Result is what processing did (the note), Error what went wrong.
	Result, Error string
	Served        string // "Downloaded by a client 28 Sep 2026 10:01", or empty
	// Rule says which of the feed's rules decides for the episode
	// ("tagged bonus by rule 1"), or is empty.
	Rule       string
	CanRetry   bool
	CanPrepare bool
	// ClientHasIt warns that a retry won't reach a client that already
	// downloaded the episode.
	ClientHasIt bool
}

type uiFeedPage struct {
	Feed                uiFeed
	RegionDiffAvailable bool
	Summary             string
	Episodes            []uiEpisode
	// Shown, Total and Problems count the episodes; Filter is "", "all" or
	// "problems".
	Shown, Total, Problems int
	Filter                 string
	Failed, NotCached      int
	Prepares               bool
	// Rules is the rule editor.
	Rules uiRules
	// Live updates the episodes while some are queued or being prepared.
	Live uiLive
}

func (page uiFeedPage) live() uiLive { return page.Live }

// statusBadges gives each status its badge and word (style guide: Status
// badge; the word carries the meaning).
var statusBadges = map[episodes.Status][2]string{
	episodes.StatusCleaned:          {"on", "Cleaned"},
	episodes.StatusCached:           {"on", "Cached"},
	episodes.StatusNotCached:        {"off", "Not cached"},
	episodes.StatusPassedThrough:    {"off", "Passed through"},
	episodes.StatusWorking:          {"off", "Working"},
	episodes.StatusQueued:           {"off", "Queued"},
	episodes.StatusRetrying:         {"warn", "Retrying"},
	episodes.StatusWithheld:         {"warn", "Withheld"},
	episodes.StatusGivenUp:          {"error", "Given up"},
	episodes.StatusPublishedWithAds: {"warn", "Published with ads"},
	episodes.StatusHidden:           {"off", "Hidden"},
}

// problem is whether an episode needs a look: it failed, is waiting on a
// retry, or failed on a client's request.
func problem(view episodes.EpisodeView) bool {
	switch view.Status {
	case episodes.StatusRetrying, episodes.StatusWithheld, episodes.StatusGivenUp, episodes.StatusPublishedWithAds:
		return true
	case episodes.StatusNotCached:
		return view.FailedAttempts > 0
	}
	return false
}

func (ui *ui) feedPage(context *gin.Context) {
	ui.showFeedPage(context, http.StatusOK, nil)
}

func (ui *ui) showFeedPage(context *gin.Context, status int, notice *uiNotice) {
	ui.showFeedPageWith(context, status, notice, nil)
}

// showFeedPageWith shows the feed page with the rule editor rules builds
// from the feed's episodes (the rows a refused save sent, so nothing typed
// is lost; or rules checked without saving), or nil for the feed's rules
// as stored.
func (ui *ui) showFeedPageWith(context *gin.Context, status int, notice *uiNotice, rules func([]models.Episode) uiRules) {
	feed, ok := ui.loadUIFeed(context)
	if !ok {
		return
	}
	var views []episodes.EpisodeView
	if ui.handlers.episodes != nil {
		var err error
		if views, err = ui.handlers.episodes.Episodes(context.Request.Context(), feed.ID); err != nil {
			logger.Log.Error("Failed to list the episodes of feed '" + feed.Title + "' for the web UI. Error: " + err.Error())
			ui.renderError(context, http.StatusInternalServerError, "Couldn't load the episodes", "Something went wrong; the log says what.")
			return
		}
	}

	content := uiFeedPage{
		Feed:                uiFeedOf(ui.handlers.feeds, feed),
		RegionDiffAvailable: ui.handlers.feeds.RegionDiffAvailable(),
		Total:               len(views),
		Filter:              context.Query("show"),
		Prepares:            ui.handlers.feeds.Processed(feed) || ui.handlers.feeds.DeliveryMode(feed) == "cache",
	}
	// The stored rules, even when the editor shows rows a refused save
	// sent: the episodes say which rule decides for them as things are.
	stored, err := ui.handlers.feeds.Rules(context.Request.Context(), feed.ID)
	if err != nil {
		logger.Log.Error("Failed to load the rules of feed '" + feed.Title + "' for the web UI. Error: " + err.Error())
		ui.renderError(context, http.StatusInternalServerError, "Couldn't load the rules", "Something went wrong; the log says what.")
		return
	}
	list := make([]models.Episode, len(views))
	for i, view := range views {
		list[i] = view.Episode
	}
	deciding := feeds.DecidingRules(stored, list)
	if rules != nil {
		content.Rules = rules(list)
	} else {
		content.Rules = rulesEditor(stored, list)
	}
	if notice == nil {
		notice = feedPageNotice(context, views)
	}
	counts := map[episodes.Status]int{}
	for _, view := range views {
		counts[view.Status]++
		if problem(view) {
			content.Problems++
		}
		if view.CanRetry {
			content.Failed++
		}
		if view.CanPrepare {
			content.NotCached++
		}
	}
	content.Summary = summarise(len(views), counts)
	busy := counts[episodes.StatusQueued] + counts[episodes.StatusWorking]
	liveURL := feedPagePath(feed)
	if content.Filter == "all" || content.Filter == "problems" {
		liveURL += "?show=" + content.Filter
	}
	verb := " are queued or working"
	if busy == 1 {
		verb = " is queued or working"
	}
	content.Live = newLive(context, liveURL, busy > 0, plural(busy, "episode")+verb)

	for i, view := range views {
		if content.Filter == "problems" && !problem(view) {
			continue
		}
		if content.Filter != "all" && content.Filter != "problems" && len(content.Episodes) == feedPageSize {
			break
		}
		row := uiEpisodeOf(view)
		if i < len(deciding) && deciding[i] >= 0 {
			withRule(&row, view, deciding[i], stored[deciding[i]])
		}
		content.Episodes = append(content.Episodes, row)
	}
	content.Shown = len(content.Episodes)
	ui.render(context, status, "feed", "feeds", notice, content)
}

// summarise is the count line: "86 episodes: 12 cleaned, 70 not cached, 3
// withheld".
func summarise(total int, counts map[episodes.Status]int) string {
	summary := plural(total, "episode")
	var parts []string
	for _, status := range []episodes.Status{
		episodes.StatusCleaned, episodes.StatusCached, episodes.StatusNotCached, episodes.StatusPassedThrough,
		episodes.StatusWorking, episodes.StatusQueued, episodes.StatusRetrying, episodes.StatusWithheld,
		episodes.StatusGivenUp, episodes.StatusPublishedWithAds, episodes.StatusHidden,
	} {
		if counts[status] > 0 {
			parts = append(parts, strconv.Itoa(counts[status])+" "+strings.ToLower(statusBadges[status][1]))
		}
	}
	if len(parts) > 0 {
		summary += ": " + strings.Join(parts, ", ")
	}
	return summary + "."
}

func uiEpisodeOf(view episodes.EpisodeView) uiEpisode {
	badge := statusBadges[view.Status]
	row := uiEpisode{
		ID:         view.ID.String(),
		Title:      view.Title,
		Published:  formatTime(view.PublishedAt),
		Length:     formatLength(view.SourceSeconds),
		Backlog:    view.Backlog,
		Badge:      badge[0],
		BadgeText:  badge[1],
		Result:     sentenceCase(view.ProcessNote),
		Error:      sentenceCase(view.Error),
		CanRetry:   view.CanRetry,
		CanPrepare: view.CanPrepare,
	}
	if row.Title == "" {
		row.Title = "Untitled episode"
	}
	if view.FullyServedAt != nil {
		row.Served = "Downloaded by a client " + formatTime(view.FullyServedAt)
		row.ClientHasIt = view.CanRetry
	}
	next := ""
	if !view.NextAttempt.IsZero() {
		next = view.NextAttempt.Local().Format("2 Jan 15:04")
	}
	switch view.Status {
	case episodes.StatusCleaned, episodes.StatusCached:
		if view.Status == episodes.StatusCached {
			row.Result = "Downloaded as it is."
		}
		row.Detail = "Cached, " + formatSize(view.CacheSize)
		if view.CacheSeconds > 0 && view.CacheSeconds != view.SourceSeconds {
			row.Detail += ", now " + formatLength(view.CacheSeconds)
		}
		row.Detail += "."
	case episodes.StatusNotCached:
		row.Detail = "Its cached copy expired; fetched again when a client asks."
		if view.Backlog {
			row.Detail = "Fetched when a client asks for it, or with Prepare."
		}
		if view.FailedAttempts > 0 {
			row.Detail += " Failed " + plural(view.FailedAttempts, "time") + " on a client's request; after 3 the failure policy applies."
		}
	case episodes.StatusPassedThrough:
		row.Detail = "Served from the source (stream or original mode); nothing to prepare."
	case episodes.StatusWorking:
		row.Detail = "Being downloaded or cleaned right now."
	case episodes.StatusQueued:
		row.Detail = "Waiting for a worker."
	case episodes.StatusRetrying:
		row.Detail = "Attempt " + strconv.Itoa(view.FailedAttempts) + " failed; tried again at " + next + "."
	case episodes.StatusWithheld:
		row.Detail = "Kept out of the feed; tried again slowly, next at " + next + "."
		if next == "" {
			row.Detail = "Kept out of the feed; tried again slowly, from the next start-up."
		}
	case episodes.StatusGivenUp:
		row.Detail = "Kept out of the feed; its retries are over. Retry to try once more."
	case episodes.StatusPublishedWithAds:
		row.Detail = "Couldn't be cleaned, so it was published with its ads."
	case episodes.StatusHidden:
		row.Detail = "Left out of the feed by one of its rules; not prepared."
	}
	return row
}

// withRule says on an episode's row which rule decides for it (index, from
// 0): what it tags the episode as, or that it hides it.
func withRule(row *uiEpisode, view episodes.EpisodeView, index int, rule feeds.Rule) {
	name := "rule " + strconv.Itoa(index+1)
	switch {
	case rule.Action == feeds.RuleTag:
		row.Rule = "tagged " + rule.EpisodeType + " by " + name
	case rule.Action == feeds.RuleHide && view.Status == episodes.StatusHidden:
		row.Rule = "hidden by " + name
		row.Detail = "Left out of the feed by " + name + "; not prepared."
	}
}

// feedPageNotice is the notice after an action redirected back. Only IDs
// and counts come in the query, never text, so a link can't make the page
// say something it didn't do.
func feedPageNotice(context *gin.Context, views []episodes.EpisodeView) *uiNotice {
	switch context.Query("done") {
	case "saved":
		return &uiNotice{Kind: "ok", Text: "Saved the settings."}
	case "rules":
		return &uiNotice{Kind: "ok", Text: "Saved the rules."}
	case "queued":
		for _, view := range views {
			if view.ID.String() == context.Query("episode") {
				return &uiNotice{Kind: "ok", Text: "Queued '" + view.Title + "'."}
			}
		}
	case "retry":
		return &uiNotice{Kind: "ok", Text: "Queued " + plural(atoi(context.Query("count")), "failed episode") + " to be tried again."}
	case "prepare":
		return &uiNotice{Kind: "ok", Text: "Queued " + plural(atoi(context.Query("count")), "episode") + " to be prepared, newest first."}
	}
	return nil
}

func atoi(text string) int {
	number, _ := strconv.Atoi(text)
	return number
}

// loadUIFeed finds the feed a feed page's path names, or answers 404.
func (ui *ui) loadUIFeed(context *gin.Context) (models.Feed, bool) {
	id, err := uuid.Parse(context.Param("feedID"))
	if err != nil {
		ui.renderError(context, http.StatusNotFound, "No such feed", "This feed doesn't exist; it may have been removed.")
		return models.Feed{}, false
	}
	feed, err := ui.handlers.feeds.Feed(context.Request.Context(), id)
	if errors.Is(err, database.ErrFeedNotFound) {
		ui.renderError(context, http.StatusNotFound, "No such feed", "This feed doesn't exist; it may have been removed.")
		return models.Feed{}, false
	}
	if err != nil {
		logger.Log.Error("Failed to load feed " + id.String() + " for the web UI. Error: " + err.Error())
		ui.renderError(context, http.StatusInternalServerError, "Couldn't load the feed", "Something went wrong; the log says what.")
		return models.Feed{}, false
	}
	return feed, true
}

func feedPagePath(feed models.Feed) string {
	return uiPrefix + "/feeds/" + feed.ID.String()
}

// feedPageURL is the feed page in a view ("", "all" or "problems"; anything
// else is the default), with a query for the notice: an action comes back
// to the view it was taken in, not the newest 50, where the episode may not
// even be.
func feedPageURL(feed models.Feed, show string, query url.Values) string {
	if show == "all" || show == "problems" {
		query.Set("show", show)
	}
	return feedPagePath(feed) + "?" + query.Encode()
}

// queueEpisode retries or prepares one episode.
func (ui *ui) queueEpisode(context *gin.Context) {
	feed, ok := ui.loadUIFeed(context)
	if !ok {
		return
	}
	episodeID, err := uuid.Parse(context.Param("episodeID"))
	if err != nil || ui.handlers.episodes == nil {
		ui.renderError(context, http.StatusNotFound, "No such episode", "This episode doesn't exist in this feed.")
		return
	}
	queued, err := ui.handlers.episodes.QueueEpisode(context.Request.Context(), feed.ID, episodeID)
	switch {
	case errors.Is(err, database.ErrEpisodeNotFound):
		ui.renderError(context, http.StatusNotFound, "No such episode", "This episode doesn't exist in this feed.")
	case errors.Is(err, episodes.ErrEpisodeBusy):
		ui.showFeedPage(context, http.StatusConflict, &uiNotice{Kind: "error", Text: "That episode is being prepared right now."})
	case errors.Is(err, episodes.ErrNotPrepared):
		ui.showFeedPage(context, http.StatusBadRequest, &uiNotice{Kind: "error", Text: "This feed's episodes are served from the source, so there's nothing to prepare."})
	case err != nil:
		logger.Log.Error("Failed to queue an episode of feed '" + feed.Title + "' from the web UI. Error: " + err.Error())
		ui.renderError(context, http.StatusInternalServerError, "Couldn't queue the episode", "Something went wrong; the log says what.")
	case !queued:
		ui.showFeedPage(context, http.StatusOK, &uiNotice{Kind: "ok", Text: "Nothing to queue: that episode has its file, or is waiting already."})
	default:
		query := url.Values{"done": {"queued"}, "episode": {episodeID.String()}}
		context.Redirect(http.StatusSeeOther, feedPageURL(feed, context.PostForm("show"), query)+"#episode-"+episodeID.String())
	}
}

// retryFeed queues every failed episode of the feed, as the API's retry.
func (ui *ui) retryFeed(context *gin.Context) {
	ui.queueFeed(context, "retry", func(feed models.Feed) (int, error) {
		return ui.handlers.episodes.RetryFailed(context.Request.Context(), feed.ID)
	})
}

// prepareFeed queues every episode without its file, as the API's prepare.
func (ui *ui) prepareFeed(context *gin.Context) {
	ui.queueFeed(context, "prepare", func(feed models.Feed) (int, error) {
		return ui.handlers.episodes.Queue(context.Request.Context(), feed.ID, 0)
	})
}

func (ui *ui) queueFeed(context *gin.Context, done string, queue func(models.Feed) (int, error)) {
	feed, ok := ui.loadUIFeed(context)
	if !ok {
		return
	}
	if ui.handlers.episodes == nil {
		ui.showFeedPage(context, http.StatusBadRequest, &uiNotice{Kind: "error", Text: "This feed's episodes are served from the source, so there's nothing to queue."})
		return
	}
	queued, err := queue(feed)
	switch {
	case errors.Is(err, episodes.ErrNotPrepared):
		ui.showFeedPage(context, http.StatusBadRequest, &uiNotice{Kind: "error", Text: "This feed's episodes are served from the source, so there's nothing to queue."})
	case err != nil:
		logger.Log.Error("Failed to queue the episodes of feed '" + feed.Title + "' from the web UI. Error: " + err.Error())
		ui.renderError(context, http.StatusInternalServerError, "Couldn't queue the episodes", "Something went wrong; the log says what.")
	default:
		query := url.Values{"done": {done}, "count": {strconv.Itoa(queued)}}
		context.Redirect(http.StatusSeeOther, feedPageURL(feed, context.PostForm("show"), query))
	}
}

// formatLength is a duration in seconds as an episode length: "1:02:53",
// "39:00"; empty when unknown.
func formatLength(seconds int) string {
	if seconds <= 0 {
		return ""
	}
	duration := time.Duration(seconds) * time.Second
	hours, minutes, rest := int(duration.Hours()), int(duration.Minutes())%60, seconds%60
	if hours > 0 {
		return strconv.Itoa(hours) + ":" + twoDigits(minutes) + ":" + twoDigits(rest)
	}
	return strconv.Itoa(minutes) + ":" + twoDigits(rest)
}

func twoDigits(number int) string {
	if number < 10 {
		return "0" + strconv.Itoa(number)
	}
	return strconv.Itoa(number)
}

// formatSize is a byte count in MB, as the log writes it: "86.4 MB".
func formatSize(bytes int64) string {
	return strconv.FormatFloat(float64(bytes)/(1<<20), 'f', 1, 64) + " MB"
}
