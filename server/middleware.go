package server

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"aunefyren/solstein/logger"

	"github.com/gin-gonic/gin"
)

// requestLogger logs each request through logrus. Successful requests log at
// debug, since Audiobookshelf polls feeds often and would drown the info log;
// client and server errors log at warn and error.
func requestLogger() gin.HandlerFunc {
	return func(context *gin.Context) {
		start := time.Now()
		context.Next()

		status := context.Writer.Status()
		// Only the path is logged, never the query string, which carries
		// signatures and API tokens.
		message := fmt.Sprintf("%d %s %s from %s in %s",
			status, context.Request.Method, redactPath(context.Request.URL.Path), context.ClientIP(), time.Since(start).Round(time.Microsecond))

		switch {
		case status >= http.StatusInternalServerError:
			logger.Log.Error(message)
		case status >= http.StatusBadRequest:
			logger.Log.Warn(message)
		default:
			logger.Log.Debug(message)
		}
	}
}

// redactPath hides the subscribe token in /api/rss/{token}/... paths.
func redactPath(path string) string {
	rest, ok := strings.CutPrefix(path, rssPrefix)
	if !ok {
		return path
	}
	if _, source, found := strings.Cut(rest, "/"); found {
		return rssPrefix + "***/" + source
	}
	return rssPrefix + "***"
}
