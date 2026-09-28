package episodes

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"aunefyren/solstein/logger"
	"aunefyren/solstein/models"

	"github.com/google/uuid"
)

// Status is where an episode stands, in the terms of the web UI's feed page
// (docs/web-ui.md): derived from several of its fields, since the pipeline's
// own state (ready, failed …) doesn't say whether it was cleaned, is waiting
// for a retry, or was given up on.
type Status string

const (
	// StatusCleaned has its file, and a processor's note on what it did.
	StatusCleaned Status = "cleaned"
	// StatusCached has its file, downloaded as it is.
	StatusCached Status = "cached"
	// StatusNotCached is published without a file: backlog not fetched yet,
	// or a copy expired from the cache. It is prepared when a client asks.
	StatusNotCached Status = "not-cached"
	// StatusPassedThrough is served from the source (stream or original
	// mode): nothing to prepare.
	StatusPassedThrough Status = "passed-through"
	// StatusWorking is being downloaded or processed right now.
	StatusWorking Status = "working"
	// StatusQueued waits for a worker: new, queued in the background, or a
	// retry that is due.
	StatusQueued Status = "queued"
	// StatusRetrying failed and is tried again at NextAttempt.
	StatusRetrying Status = "retrying"
	// StatusWithheld failed and is kept out of the feed, retried slowly.
	StatusWithheld Status = "withheld"
	// StatusGivenUp is withheld, and its slow retries are over: only a
	// retry (or a change of settings) brings it back.
	StatusGivenUp Status = "given-up"
	// StatusPublishedWithAds failed, and the failure policy published it
	// unprocessed.
	StatusPublishedWithAds Status = "published-with-ads"
)

// EpisodeView is an episode as the web UI shows it.
type EpisodeView struct {
	models.Episode
	Status Status
	// NextAttempt is when it is tried again, for StatusRetrying and
	// StatusWithheld; zero otherwise.
	NextAttempt time.Time
	// Error is LastError with any URL cut down to its host: an error often
	// quotes the source URL, whose query can carry a private feed's token.
	Error string
	// CanRetry and CanPrepare say which action applies now: a failed
	// episode can be tried again, one without its file prepared.
	CanRetry, CanPrepare bool
}

// ErrEpisodeBusy means an episode is being prepared right now, so there is
// nothing to queue.
var ErrEpisodeBusy = errors.New("the episode is being prepared right now")

// Episodes returns a feed's episodes, newest first, with their status.
func (pipeline *Pipeline) Episodes(ctx context.Context, feedID uuid.UUID) ([]EpisodeView, error) {
	feed, err := pipeline.store.GetFeed(ctx, feedID)
	if err != nil {
		return nil, err
	}
	list, err := pipeline.store.ListEpisodes(ctx, feedID)
	if err != nil {
		return nil, err
	}
	newestFirst(list)
	prepares, now := pipeline.prepares(feed), pipeline.options.Now()
	views := make([]EpisodeView, 0, len(list))
	for _, episode := range list {
		views = append(views, viewOf(episode, prepares, pipeline.preparing(episode.ID), now))
	}
	return views, nil
}

// viewOf works out an episode's status. prepares is whether the feed's
// episodes are prepared at all; working, whether a job for it is running.
func viewOf(episode models.Episode, prepares, working bool, now time.Time) EpisodeView {
	view := EpisodeView{Episode: episode, Error: scrubURLs(episode.LastError)}
	queued := episode.NextAttemptAt != nil && !episode.NextAttemptAt.After(now)
	later := episode.NextAttemptAt != nil && episode.NextAttemptAt.After(now)
	switch {
	case working || episode.State == models.EpisodeAcquiring || episode.State == models.EpisodeProcessing:
		view.Status = StatusWorking
	case episode.State == models.EpisodeDiscovered && later && episode.FailedAttempts > 0:
		view.Status, view.NextAttempt = StatusRetrying, *episode.NextAttemptAt
	case episode.State == models.EpisodeDiscovered:
		view.Status = StatusQueued
	case episode.State == models.EpisodeFailed && queued:
		view.Status = StatusQueued
	case episode.State == models.EpisodeFailed && episode.Withheld && later:
		view.Status, view.NextAttempt = StatusWithheld, *episode.NextAttemptAt
	case episode.State == models.EpisodeFailed && episode.Withheld && episode.LateRetries >= len(withheldRetryDelays):
		view.Status = StatusGivenUp
	case episode.State == models.EpisodeFailed && episode.Withheld:
		// Not scheduled yet (from before slow retries existed): start-up
		// gives it its first retry.
		view.Status = StatusWithheld
	case episode.State == models.EpisodeFailed:
		view.Status = StatusPublishedWithAds
	case episode.CacheFile != "" && episode.ProcessNote != "":
		view.Status = StatusCleaned
	case episode.CacheFile != "":
		view.Status = StatusCached
	case !prepares:
		view.Status = StatusPassedThrough
	case queued:
		view.Status = StatusQueued
	default:
		view.Status = StatusNotCached
	}
	idle := view.Status != StatusWorking && view.Status != StatusQueued
	view.CanRetry = idle && prepares && episode.State == models.EpisodeFailed
	view.CanPrepare = idle && prepares && view.Status == StatusNotCached
	return view
}

// QueueEpisode queues one episode for the background, as RetryFailed and
// Queue do for a whole feed: a failed one to be tried again, one published
// without its file to be prepared. It reports whether it was queued: false
// when it's neither (it has its file, or is waiting already).
func (pipeline *Pipeline) QueueEpisode(ctx context.Context, feedID, episodeID uuid.UUID) (bool, error) {
	feed, err := pipeline.store.GetFeed(ctx, feedID)
	if err != nil {
		return false, err
	}
	if !pipeline.prepares(feed) {
		return false, ErrNotPrepared
	}
	episode, err := pipeline.store.GetEpisode(ctx, feedID, episodeID)
	if err != nil {
		return false, err
	}
	if pipeline.preparing(episode.ID) {
		return false, ErrEpisodeBusy
	}
	queue, purpose := pipeline.store.QueueUncachedEpisodes, "prepared"
	if episode.State == models.EpisodeFailed {
		queue, purpose = pipeline.store.QueueFailedEpisodes, "tried again"
	}
	queued, err := queue(ctx, []uuid.UUID{episode.ID}, pipeline.options.Now().UTC())
	if err != nil || queued == 0 {
		return false, err
	}
	logger.Log.Info(fmt.Sprintf("Queued episode '%s' of '%s' to be %s in the background.", episode.Title, feed.Title, purpose))
	pipeline.Wake()
	return true, nil
}

// urlPattern finds URLs in error text; scheme and host are kept.
var urlPattern = regexp.MustCompile(`\b([a-zA-Z][a-zA-Z0-9+.-]*://[^/\s"'<>?#]+)[^\s"'<>]*`)

// scrubURLs cuts every URL in a text down to its scheme and host
// ("https://podkast.nrk.no/…"), so a path or query carrying a token isn't
// shown.
func scrubURLs(text string) string {
	return urlPattern.ReplaceAllStringFunc(text, func(match string) string {
		host := urlPattern.FindStringSubmatch(match)[1]
		if host == match {
			return match
		}
		return host + "/…"
	})
}
