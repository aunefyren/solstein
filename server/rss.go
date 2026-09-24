package server

import (
	stdcontext "context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"aunefyren/solstein/database"
	"aunefyren/solstein/feeds"
	"aunefyren/solstein/logger"
	"aunefyren/solstein/models"
	"aunefyren/solstein/signing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	rssPrefix = "/api/rss/"
	// subscribeTimeout bounds the first fetch of a new feed. ABS gives up on
	// a feed request after 30 seconds, so this stays well under that.
	subscribeTimeout = 20 * time.Second
)

// subscribeByPrefix serves /api/rss/{token}/{source URL}: subscribes to the
// source feed on first use and returns the rewritten feed.
func (handlers *handlers) subscribeByPrefix(context *gin.Context) {
	// The raw path, so the source URL's own escaping survives; gin's params
	// are unescaped.
	rest := strings.TrimPrefix(context.Request.URL.EscapedPath(), rssPrefix)
	token, source, _ := strings.Cut(rest, "/")
	if handlers.access.disableAuth && token != handlers.access.token {
		// Without auth the token segment is optional.
		source = rest
	} else if !handlers.access.tokenValid(token) {
		logger.Log.Warn("Refused subscribe request from " + context.ClientIP() + " with a wrong token.")
		context.JSON(http.StatusForbidden, gin.H{"error": "Forbidden."})
		context.Abort()
		return
	}

	sourceURL, err := feeds.SourceFromPrefixPath(source, context.Request.URL.RawQuery)
	if err != nil {
		logger.Log.Warn("Refused subscribe request with an invalid source URL. Error: " + err.Error())
		context.JSON(http.StatusBadRequest, gin.H{"error": "Invalid source feed URL."})
		context.Abort()
		return
	}

	ctx, cancel := stdcontext.WithTimeout(context.Request.Context(), subscribeTimeout)
	defer cancel()
	feed, created, err := handlers.feeds.Subscribe(ctx, sourceURL, feeds.Settings{})
	if err != nil {
		handlers.subscribeFailed(context, sourceURL, err)
		return
	}
	if created {
		logger.Log.Info("Subscribed to feed '" + feed.Title + "' from " + withoutQuery(sourceURL) + ".")
	}
	handlers.writeFeed(context, feed)
}

// feedByID serves /api/feeds/{feedID}.xml, the signed feed URL.
func (handlers *handlers) feedByID(context *gin.Context) {
	file := context.Param("file")
	feedID, err := uuid.Parse(strings.TrimSuffix(file, ".xml"))
	if err != nil || !strings.HasSuffix(file, ".xml") {
		context.JSON(http.StatusNotFound, gin.H{"error": "Feed not found."})
		context.Abort()
		return
	}
	if !handlers.signatureValid(context, feeds.FeedPath(feedID)) {
		logger.Log.Warn("Refused feed request from " + context.ClientIP() + " with a missing or wrong signature.")
		context.JSON(http.StatusForbidden, gin.H{"error": "Forbidden."})
		context.Abort()
		return
	}

	feed, err := handlers.feeds.Feed(context.Request.Context(), feedID)
	if errors.Is(err, database.ErrFeedNotFound) {
		context.JSON(http.StatusNotFound, gin.H{"error": "Feed not found."})
		context.Abort()
		return
	}
	if err != nil {
		logger.Log.Error("Failed to load feed. Error: " + err.Error())
		context.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load feed."})
		context.Abort()
		return
	}
	handlers.writeFeed(context, feed)
}

// signatureValid checks the sig parameter against the canonical path. The
// canonical path, not the request's, is signed, so a request can't pass by
// spelling the path differently.
func (handlers *handlers) signatureValid(context *gin.Context, canonicalPath string) bool {
	if handlers.access.disableAuth {
		return true
	}
	return context.Request.URL.Path == canonicalPath &&
		handlers.signer.Verify(canonicalPath, context.Query(signing.QueryParameter))
}

func (handlers *handlers) urls(context *gin.Context) feeds.URLs {
	return feeds.URLs{
		Base:     handlers.access.baseURL(context, handlers.externalURL),
		Signer:   handlers.signer,
		Unsigned: handlers.access.disableAuth,
	}
}

func (handlers *handlers) writeFeed(context *gin.Context, feed models.Feed) {
	output, err := handlers.feeds.Render(context.Request.Context(), feed, handlers.urls(context))
	if err != nil {
		logger.Log.Error("Failed to render feed '" + feed.Title + "'. Error: " + err.Error())
		context.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to render feed."})
		context.Abort()
		return
	}
	// Clients should always revalidate: the feed changes as episodes become
	// ready.
	context.Header("Cache-Control", "no-cache")
	context.Data(http.StatusOK, "application/rss+xml; charset=utf-8", output)
}

// subscribeFailed maps a subscribe error to a response.
func (handlers *handlers) subscribeFailed(context *gin.Context, sourceURL string, err error) {
	status, message := http.StatusInternalServerError, "Failed to subscribe to the feed."
	switch {
	case errors.Is(err, feeds.ErrInvalidSourceURL), errors.Is(err, feeds.ErrInvalidSettings):
		status, message = http.StatusBadRequest, "Invalid request: "+err.Error()
	case errors.Is(err, feeds.ErrSourceNotAllowed):
		status, message = http.StatusForbidden, "That source host is not allowed."
	case errors.Is(err, feeds.ErrFetchFailed):
		status, message = http.StatusBadGateway, "Could not fetch a valid RSS feed from the source."
	}
	logger.Log.Warn("Failed to subscribe to " + withoutQuery(sourceURL) + ". Error: " + err.Error())
	context.JSON(status, gin.H{"error": message})
	context.Abort()
}

// withoutQuery drops the query string before a URL is logged: private feeds
// often carry their access token there.
func withoutQuery(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "(unparseable URL)"
	}
	parsed.RawQuery = ""
	return parsed.String()
}
