package server

import (
	stdcontext "context"
	"errors"
	"net/http"

	"aunefyren/solstein/database"
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
	FeedURL           string `json:"feed_url"`
}

func (handlers *handlers) feedResponse(context *gin.Context, feed models.Feed) feedResponse {
	return feedResponse{
		Feed:              feed,
		DeliveryModeInUse: handlers.feeds.DeliveryMode(feed),
		FeedURL:           handlers.urls(context).Feed(feed.ID),
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
	Exit                *string `json:"exit"`
	DeliveryMode        *string `json:"delivery_mode"`
	PollIntervalMinutes *int    `json:"poll_interval_minutes"`
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

	err := handlers.feeds.Update(context.Request.Context(), &feed)
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
