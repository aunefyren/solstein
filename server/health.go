package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// healthHandler reports that the process is up, for container health checks
// and monitoring. It deliberately says nothing about module state, so a
// tunnel being down doesn't get the whole container restarted.
func healthHandler(version string) gin.HandlerFunc {
	return func(context *gin.Context) {
		context.JSON(http.StatusOK, gin.H{"status": "ok", "version": version})
	}
}
