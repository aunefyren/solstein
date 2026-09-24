package server

import (
	"fmt"
	"net/http"
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
		message := fmt.Sprintf("%d %s %s from %s in %s",
			status, context.Request.Method, context.Request.URL.Path, context.ClientIP(), time.Since(start).Round(time.Microsecond))

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
