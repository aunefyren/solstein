package server

import (
	stdcontext "context"
	"errors"
	"fmt"
	"net/http"

	"aunefyren/solstein/database"
	"aunefyren/solstein/episodes"
	"aunefyren/solstein/feeds"
	"aunefyren/solstein/logger"
	"aunefyren/solstein/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// feedResponse is a feed as the API returns it, with the signed URL to
// subscribe to in a podcast client.
type feedResponse struct {
	models.Feed
	DeliveryModeInUse string `json:"delivery_mode_in_use"`
	// RegionDiffInUse says whether the feed's episodes are region-diffed,
	// after the global setting and the feed's own are combined.
	RegionDiffInUse bool `json:"region_diff_in_use"`
	// PrepareAheadInUse says whether the feed's episodes are all prepared
	// before a client asks, the global and the feed's settings combined.
	PrepareAheadInUse bool `json:"prepare_ahead_in_use"`
	// ServeDroppedInUse and DeleteDroppedInUse say what happens to the
	// episodes the source no longer lists, the global and the feed's
	// settings combined (an episode can override serving for itself).
	ServeDroppedInUse  bool   `json:"serve_dropped_in_use"`
	DeleteDroppedInUse bool   `json:"delete_dropped_in_use"`
	FeedURL            string `json:"feed_url"`
}

func (handlers *handlers) feedResponse(context *gin.Context, feed models.Feed) feedResponse {
	return feedResponse{
		Feed:               feed,
		DeliveryModeInUse:  handlers.feeds.DeliveryMode(feed),
		RegionDiffInUse:    handlers.feeds.Processed(feed),
		PrepareAheadInUse:  handlers.feeds.PreparesAhead(feed),
		ServeDroppedInUse:  handlers.feeds.FeedServesDropped(feed),
		DeleteDroppedInUse: handlers.feeds.DeletesDropped(feed),
		FeedURL:            handlers.urls(context).Feed(feed.ID),
	}
}

func (handlers *handlers) apiListFeeds(context *gin.Context) {
	list, err := handlers.feeds.List(context.Request.Context())
	if err != nil {
		logger.Log.Error("Failed to list feeds. Error: " + err.Error())
		context.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list feeds."})
		context.Abort()
		return
	}
	response := make([]feedResponse, 0, len(list))
	for _, feed := range list {
		response = append(response, handlers.feedResponse(context, feed))
	}
	context.JSON(http.StatusOK, response)
}

type createFeedRequest struct {
	SourceURL string `json:"source_url" binding:"required"`
	feeds.Settings
}

// apiCreateFeed subscribes to a feed. Posting a source that is already
// subscribed returns the existing feed (200) without changing its settings.
func (handlers *handlers) apiCreateFeed(context *gin.Context) {
	var request createFeedRequest
	if err := context.ShouldBindJSON(&request); err != nil {
		context.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body: " + err.Error()})
		context.Abort()
		return
	}

	ctx, cancel := stdcontext.WithTimeout(context.Request.Context(), subscribeTimeout)
	defer cancel()
	feed, created, err := handlers.feeds.Subscribe(ctx, request.SourceURL, request.Settings)
	if err != nil {
		handlers.subscribeFailed(context, request.SourceURL, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		logger.Log.Info("Subscribed to feed '" + feed.Title + "' from " + withoutQuery(feed.SourceURL) + " through the API.")
	}
	context.JSON(status, handlers.feedResponse(context, feed))
}

func (handlers *handlers) apiGetFeed(context *gin.Context) {
	feed, ok := handlers.loadFeed(context)
	if !ok {
		return
	}
	context.JSON(http.StatusOK, handlers.feedResponse(context, feed))
}

// updateFeedRequest has pointer fields so an omitted field is left alone and
// an empty one clears the override.
type updateFeedRequest struct {
	Exit                       *string   `json:"exit"`
	DeliveryMode               *string   `json:"delivery_mode"`
	PollIntervalMinutes        *int      `json:"poll_interval_minutes"`
	RegionDiff                 *string   `json:"region_diff"`
	RegionDiffExits            *[]string `json:"region_diff_exits"`
	RegionDiffOnFailure        *string   `json:"region_diff_on_failure"`
	RegionDiffTrimBreakMarkers *string   `json:"region_diff_trim_break_markers"`
	RegionDiffCompareByAudio   *string   `json:"region_diff_compare_by_audio"`
	PrepareAhead               *string   `json:"prepare_ahead"`
	ServeDropped               *string   `json:"serve_dropped"`
	DeleteDropped              *string   `json:"delete_dropped"`
}

func (handlers *handlers) apiUpdateFeed(context *gin.Context) {
	var request updateFeedRequest
	if err := context.ShouldBindJSON(&request); err != nil {
		context.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body: " + err.Error()})
		context.Abort()
		return
	}
	feed, ok := handlers.loadFeed(context)
	if !ok {
		return
	}
	if request.Exit != nil {
		feed.Exit = *request.Exit
	}
	if request.DeliveryMode != nil {
		feed.DeliveryMode = *request.DeliveryMode
	}
	if request.PollIntervalMinutes != nil {
		feed.PollIntervalMinutes = *request.PollIntervalMinutes
	}
	if request.RegionDiff != nil {
		feed.RegionDiff = *request.RegionDiff
	}
	if request.RegionDiffExits != nil {
		feed.RegionDiffExits = *request.RegionDiffExits
	}
	if request.RegionDiffOnFailure != nil {
		feed.RegionDiffOnFailure = *request.RegionDiffOnFailure
	}
	if request.RegionDiffTrimBreakMarkers != nil {
		feed.RegionDiffTrimBreakMarkers = *request.RegionDiffTrimBreakMarkers
	}
	if request.RegionDiffCompareByAudio != nil {
		feed.RegionDiffCompareByAudio = *request.RegionDiffCompareByAudio
	}
	if request.PrepareAhead != nil {
		feed.PrepareAhead = *request.PrepareAhead
	}
	if request.ServeDropped != nil {
		feed.ServeDropped = *request.ServeDropped
	}
	if request.DeleteDropped != nil {
		feed.DeleteDropped = *request.DeleteDropped
	}

	err := handlers.updateFeed(context.Request.Context(), &feed)
	if errors.Is(err, feeds.ErrInvalidSettings) {
		context.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		context.Abort()
		return
	}
	if err != nil {
		logger.Log.Error("Failed to update feed '" + feed.Title + "'. Error: " + err.Error())
		context.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update feed."})
		context.Abort()
		return
	}
	context.JSON(http.StatusOK, handlers.feedResponse(context, feed))
}

// updateFeed saves a feed's changed settings and brings its episodes in line
// with them. The API and the web UI both change feeds through it, so a
// setting means the same whichever way it was changed. An invalid setting
// returns feeds.ErrInvalidSettings, and nothing is saved.
func (handlers *handlers) updateFeed(ctx stdcontext.Context, feed *models.Feed) error {
	if err := handlers.feeds.Update(ctx, feed); err != nil {
		return err
	}
	if handlers.episodes == nil {
		return nil
	}
	// Cached files made with the old settings go; the settings are saved
	// either way, and serving checks each episode again.
	if err := handlers.episodes.FeedChanged(ctx, feed.ID); err != nil {
		logger.Log.Error("Failed to update the episodes of feed '" + feed.Title + "' to its new settings. Error: " + err.Error())
	}
	// Preparing ahead keeps episodes without a file out of the feed, so
	// nothing would ask for them: they are queued instead.
	if handlers.feeds.PreparesAhead(*feed) {
		if _, err := handlers.episodes.Queue(ctx, feed.ID, 0); err != nil && !errors.Is(err, episodes.ErrNotPrepared) {
			logger.Log.Error("Failed to queue the episodes of feed '" + feed.Title + "' to be prepared ahead. Error: " + err.Error())
		}
	}
	return nil
}

func (handlers *handlers) apiDeleteFeed(context *gin.Context) {
	feed, ok := handlers.loadFeed(context)
	if !ok {
		return
	}
	if err := handlers.feeds.Delete(context.Request.Context(), feed.ID); err != nil {
		logger.Log.Error("Failed to delete feed '" + feed.Title + "'. Error: " + err.Error())
		context.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete feed."})
		context.Abort()
		return
	}
	if handlers.episodes != nil {
		if err := handlers.episodes.RemoveFeed(feed.ID); err != nil {
			// The hourly clean-up will get it; the feed itself is gone.
			logger.Log.Warn("Failed to delete cached audio of feed '" + feed.Title + "'. Error: " + err.Error())
		}
	}
	logger.Log.Info("Deleted feed '" + feed.Title + "'.")
	context.Status(http.StatusNoContent)
}

// apiRetryFailed queues the feed's failed episodes, withheld or published
// unprocessed, for another attempt in the background.
func (handlers *handlers) apiRetryFailed(context *gin.Context) {
	feed, ok := handlers.loadFeed(context)
	if !ok {
		return
	}
	queued, err := 0, episodes.ErrNotPrepared
	if handlers.episodes != nil {
		queued, err = handlers.episodes.RetryFailed(context.Request.Context(), feed.ID)
	}
	handlers.queued(context, feed, queued, err)
}

// apiRetryAllFailed queues the failed episodes of every feed whose episodes
// are prepared, as apiRetryFailed does for one.
func (handlers *handlers) apiRetryAllFailed(context *gin.Context) {
	list, err := handlers.feeds.List(context.Request.Context())
	if err != nil {
		logger.Log.Error("Failed to list feeds. Error: " + err.Error())
		context.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list feeds."})
		context.Abort()
		return
	}
	total := 0
	for _, feed := range list {
		if handlers.episodes == nil {
			break
		}
		queued, err := handlers.episodes.RetryFailed(context.Request.Context(), feed.ID)
		if errors.Is(err, episodes.ErrNotPrepared) || errors.Is(err, database.ErrFeedNotFound) {
			continue // nothing to retry, or deleted meanwhile
		}
		if err != nil {
			logger.Log.Error("Failed to queue episodes of feed '" + feed.Title + "'. Error: " + err.Error())
			context.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to queue episodes."})
			context.Abort()
			return
		}
		total += queued
	}
	context.JSON(http.StatusOK, gin.H{"queued": total})
}

type prepareRequest struct {
	// Newest limits it to the feed's newest episodes; zero means all.
	Newest int `json:"newest"`
}

// apiPrepare queues the feed's newest episodes that are published without
// their file (backlog, or expired from the cache) to be prepared in the
// background, before any client asks for them.
func (handlers *handlers) apiPrepare(context *gin.Context) {
	var request prepareRequest
	// The body is optional: none prepares every episode.
	if context.Request.ContentLength != 0 {
		if err := context.ShouldBindJSON(&request); err != nil {
			context.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body: " + err.Error()})
			context.Abort()
			return
		}
	}
	if request.Newest < 0 {
		context.JSON(http.StatusBadRequest, gin.H{"error": "newest can't be negative."})
		context.Abort()
		return
	}
	feed, ok := handlers.loadFeed(context)
	if !ok {
		return
	}
	queued, err := 0, episodes.ErrNotPrepared
	if handlers.episodes != nil {
		queued, err = handlers.episodes.Queue(context.Request.Context(), feed.ID, request.Newest)
	}
	handlers.queued(context, feed, queued, err)
}

// queued answers a request that queued episodes.
func (handlers *handlers) queued(context *gin.Context, feed models.Feed, queued int, err error) {
	if errors.Is(err, episodes.ErrNotPrepared) {
		context.JSON(http.StatusBadRequest, gin.H{"error": "The feed's episodes aren't prepared by Solstein (stream or original mode, without region diff), so there is nothing to queue."})
		context.Abort()
		return
	}
	if err != nil {
		logger.Log.Error("Failed to queue episodes of feed '" + feed.Title + "'. Error: " + err.Error())
		context.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to queue episodes."})
		context.Abort()
		return
	}
	context.JSON(http.StatusOK, gin.H{"queued": queued})
}

// loadFeed reads the :feedID parameter and loads the feed, responding with
// 404 or 500 itself when it can't.
func (handlers *handlers) loadFeed(context *gin.Context) (models.Feed, bool) {
	feedID, err := uuid.Parse(context.Param("feedID"))
	if err != nil {
		context.JSON(http.StatusNotFound, gin.H{"error": "Feed not found."})
		context.Abort()
		return models.Feed{}, false
	}
	feed, err := handlers.feeds.Feed(context.Request.Context(), feedID)
	if errors.Is(err, database.ErrFeedNotFound) {
		context.JSON(http.StatusNotFound, gin.H{"error": "Feed not found."})
		context.Abort()
		return models.Feed{}, false
	}
	if err != nil {
		logger.Log.Error("Failed to load feed. Error: " + err.Error())
		context.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load feed."})
		context.Abort()
		return models.Feed{}, false
	}
	return feed, true
}

// rulesBody is a feed's rules, in the order they are tried, as the API
// takes and returns them.
type rulesBody struct {
	Rules []feeds.Rule `json:"rules"`
}

func (handlers *handlers) apiGetRules(context *gin.Context) {
	feed, ok := handlers.loadFeed(context)
	if !ok {
		return
	}
	rules, err := handlers.feeds.Rules(context.Request.Context(), feed.ID)
	if err != nil {
		logger.Log.Error("Failed to load the rules of feed '" + feed.Title + "'. Error: " + err.Error())
		context.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load rules."})
		context.Abort()
		return
	}
	context.JSON(http.StatusOK, rulesBody{Rules: nonNil(rules)})
}

// apiSetRules replaces a feed's rules. Episodes already stored are hidden or
// shown to match at once; one no longer hidden is prepared as it would have
// been.
func (handlers *handlers) apiSetRules(context *gin.Context) {
	var request rulesBody
	if err := context.ShouldBindJSON(&request); err != nil {
		context.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body: " + err.Error()})
		context.Abort()
		return
	}
	feed, ok := handlers.loadFeed(context)
	if !ok {
		return
	}
	err := handlers.setRules(context.Request.Context(), feed, request.Rules)
	if errors.Is(err, feeds.ErrInvalidSettings) {
		context.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		context.Abort()
		return
	}
	if errors.Is(err, database.ErrFeedNotFound) {
		context.JSON(http.StatusNotFound, gin.H{"error": "Feed not found."}) // deleted meanwhile
		context.Abort()
		return
	}
	if err != nil {
		logger.Log.Error("Failed to save the rules of feed '" + feed.Title + "'. Error: " + err.Error())
		context.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save rules."})
		context.Abort()
		return
	}
	context.JSON(http.StatusOK, rulesBody{Rules: nonNil(request.Rules)})
}

// setRules saves a feed's rules and has the episodes they stop hiding
// prepared. The API and the web UI both save rules through it. Invalid
// rules return feeds.ErrInvalidSettings, and nothing is saved.
func (handlers *handlers) setRules(ctx stdcontext.Context, feed models.Feed, rules []feeds.Rule) error {
	shown, err := handlers.feeds.SetRules(ctx, feed.ID, rules)
	if err != nil {
		return err
	}
	logger.Log.Info(fmt.Sprintf("Saved %s for feed '%s'.", plural(len(rules), "rule"), feed.Title))
	if shown > 0 && handlers.episodes != nil {
		// Shown again, they are queued where the feed prepares ahead
		// (feeds.Service.SetRules); the workers look now.
		handlers.episodes.Wake()
	}
	return nil
}

// nonNil makes an empty list encode as [] rather than null.
func nonNil[T any](list []T) []T {
	if list == nil {
		return []T{}
	}
	return list
}
