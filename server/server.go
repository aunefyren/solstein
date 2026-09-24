// Package server owns the HTTP side of Solstein: the Gin router, its
// middleware and the handlers that serve feeds and episodes to Audiobookshelf.
package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"aunefyren/solstein/logger"
	"aunefyren/solstein/settings"

	"github.com/gin-gonic/gin"
)

const shutdownTimeout = 30 * time.Second

// New builds the HTTP server for cfg. version is reported by /api/health.
func New(cfg settings.Config, version string) *http.Server {
	return &http.Server{
		Addr:    ":" + strconv.Itoa(cfg.Port),
		Handler: newRouter(version),
		// Guards against clients that open a connection and never finish the
		// headers. There is deliberately no WriteTimeout: episode downloads are
		// large and Audiobookshelf may read them slowly.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
}

func newRouter(version string) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)

	// gin.New rather than gin.Default: Default's request logger writes to
	// stdout in its own format, bypassing logrus and the log file.
	router := gin.New()
	router.Use(requestLogger(), gin.Recovery())

	// Solstein is reached directly or through a reverse proxy the operator
	// controls; trusting every proxy by default would let any client spoof
	// its IP with X-Forwarded-For.
	if err := router.SetTrustedProxies(nil); err != nil {
		logger.Log.Warn("Failed to reset trusted proxies. Error: " + err.Error())
	}

	api := router.Group("/api")
	{
		api.GET("/health", healthHandler(version))
	}

	return router
}

// Run serves until ctx is cancelled, then shuts down gracefully so in-flight
// downloads can finish.
func Run(ctx context.Context, srv *http.Server) error {
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		return fmt.Errorf("listen on %s: %w", srv.Addr, err)
	case <-ctx.Done():
	}

	logger.Log.Info("Shutting down HTTP server...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shut down HTTP server: %w", err)
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("listen on %s: %w", srv.Addr, err)
	}
	return nil
}
