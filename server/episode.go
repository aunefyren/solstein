package server

import (
	"errors"
	"net/http"
	"regexp"
	"strings"

	"aunefyren/solstein/database"
	"aunefyren/solstein/episodes"
	"aunefyren/solstein/feeds"
	"aunefyren/solstein/logger"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

var extensionPattern = regexp.MustCompile(`^[a-z0-9]{1,5}$`)

// episode serves /api/episodes/{feedID}/{episodeID}.{ext}, the signed
// enclosure URL.
func (handlers *handlers) episode(context *gin.Context) {
	feedID, feedErr := uuid.Parse(context.Param("feedID"))
	name, extension, _ := strings.Cut(context.Param("file"), ".")
	episodeID, episodeErr := uuid.Parse(name)
	if feedErr != nil || episodeErr != nil || !extensionPattern.MatchString(extension) {
		context.JSON(http.StatusNotFound, gin.H{"error": "Episode not found."})
		context.Abort()
		return
	}
	if !handlers.signatureValid(context, feeds.EpisodePath(feedID, episodeID, extension)) {
		logger.Log.Warn("Refused episode request from " + context.ClientIP() + " with a missing or wrong signature.")
		context.JSON(http.StatusForbidden, gin.H{"error": "Forbidden."})
		context.Abort()
		return
	}

	err := handlers.episodes.Serve(context.Writer, context.Request, feedID, episodeID)
	switch {
	case err == nil:
	case errors.Is(err, database.ErrEpisodeNotFound), errors.Is(err, database.ErrFeedNotFound):
		context.JSON(http.StatusNotFound, gin.H{"error": "Episode not found."})
		context.Abort()
	case errors.Is(err, episodes.ErrSourceFailed):
		logger.Log.Warn("Failed to fetch episode from its source. Error: " + err.Error())
		context.JSON(http.StatusBadGateway, gin.H{"error": "Could not fetch the episode from its source."})
		context.Abort()
	default:
		logger.Log.Error("Failed to serve episode. Error: " + err.Error())
		context.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to serve episode."})
		context.Abort()
	}
}
