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

	"aunefyren/solstein/episodes"
	"aunefyren/solstein/feeds"
	"aunefyren/solstein/logger"
	"aunefyren/solstein/settings"
	"aunefyren/solstein/signing"

	"github.com/gin-gonic/gin"
)

const (
	shutdownTimeout = 30 * time.Second
	healthPath      = "/api/health"
)

// Options are the server's dependencies.
type Options struct {
	Config   settings.Config
	Version  string
	Feeds    *feeds.Service
	Episodes *episodes.Server
}

// handlers carries what the route handlers need.
type handlers struct {
	feeds       *feeds.Service
	episodes    *episodes.Server
	access      access
	signer      signing.Signer
	externalURL string
}

// New builds the HTTP server.
func New(options Options) (*http.Server, error) {
	router, err := newRouter(options)
	if err != nil {
		return nil, err
	}
	return &http.Server{
		Addr:    ":" + strconv.Itoa(options.Config.Port),
		Handler: router,
		// Guards against clients that open a connection and never finish the
		// headers. There is deliberately no WriteTimeout: episode downloads are
		// large and Audiobookshelf may read them slowly.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}, nil
}

func newRouter(options Options) (*gin.Engine, error) {
	gin.SetMode(gin.ReleaseMode)
	cfg := options.Config

	clientNetworks, err := parsePrefixes(cfg.AllowedClientNetworks)
	if err != nil {
		return nil, fmt.Errorf("allowed client networks: %w", err)
	}
	trustedProxies, err := parsePrefixes(cfg.TrustedProxies)
	if err != nil {
		return nil, fmt.Errorf("trusted proxies: %w", err)
	}
	handlers := &handlers{
		feeds:    options.Feeds,
		episodes: options.Episodes,
		access: access{
			disableAuth:    cfg.DisableAuth,
			token:          cfg.AuthToken,
			clientNetworks: clientNetworks,
			trustedProxies: trustedProxies,
		},
		signer:      signing.New(cfg.URLSigningKey),
		externalURL: cfg.ExternalURL,
	}

	// gin.New rather than gin.Default: Default's request logger writes to
	// stdout in its own format, bypassing logrus and the log file.
	router := gin.New()
	router.Use(requestLogger(), gin.Recovery(), handlers.access.clientNetworkCheck())

	// Only the configured proxies' X-Forwarded-For is believed; with none,
	// the client address is the connection's, so it can't be spoofed.
	if err := router.SetTrustedProxies(cfg.TrustedProxies); err != nil {
		return nil, fmt.Errorf("trusted proxies: %w", err)
	}

	router.GET(healthPath, healthHandler(options.Version))
	router.GET(rssPrefix+":token/*source", handlers.subscribeByPrefix)
	router.GET("/api/feeds/:file", handlers.feedByID)
	router.GET("/api/episodes/:feedID/:file", handlers.episode)
	router.HEAD("/api/episodes/:feedID/:file", handlers.episode)

	api := router.Group("/api/v1", handlers.access.requireToken())
	{
		api.GET("/feeds", handlers.apiListFeeds)
		api.POST("/feeds", handlers.apiCreateFeed)
		api.GET("/feeds/:feedID", handlers.apiGetFeed)
		api.PATCH("/feeds/:feedID", handlers.apiUpdateFeed)
		api.DELETE("/feeds/:feedID", handlers.apiDeleteFeed)
	}

	return router, nil
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
