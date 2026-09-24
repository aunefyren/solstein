package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"aunefyren/solstein/database"
	"aunefyren/solstein/episodes"
	"aunefyren/solstein/feeds"
	"aunefyren/solstein/logger"
	"aunefyren/solstein/outbound"
	"aunefyren/solstein/server"
	"aunefyren/solstein/settings"

	// Embeds the time zone database so -timezone works on hosts without one
	// (Windows, minimal containers).
	_ "time/tzdata"
)

// downloadWorkers is how many episodes are downloaded at once. Two keeps a
// backlog of new episodes moving without bursting requests at one host.
const downloadWorkers = 2

// version is set at build time with -ldflags "-X main.version=<tag>". It is
// never written to config.json, so a config file can't report a stale version.
var version = "dev"

func main() {
	os.Exit(run())
}

// run holds the whole start-up and returns the exit code, so deferred cleanup
// (closing the log file) runs before the process exits.
func run() int {
	cfg, startup, err := settings.Resolve(os.Args[1:], os.Getenv, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		// The logger isn't set up yet: its file lives in the config directory and
		// its level comes from the config that just failed.
		fmt.Fprintln(os.Stderr, "Failed to load configuration. Error: "+err.Error())
		return 1
	}
	if startup.ShowVersion {
		fmt.Println("solstein " + version)
		return 0
	}

	// Set before the logger starts so log timestamps use the configured zone.
	location, err := cfg.Location()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Failed to load time zone. Error: "+err.Error())
		return 1
	}
	time.Local = location

	logFile, err := logger.Init(startup.ConfigDir, cfg.LogLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Failed to initialise logger. Error: "+err.Error())
		return 1
	}
	defer logFile.Close()

	fmt.Println()
	fmt.Println("S O L S T E I N")
	fmt.Println()
	timezone := cfg.Timezone
	if timezone == "" {
		timezone = "system default"
	}
	logger.Log.Info("Running Solstein version " + version + " with log level " + cfg.LogLevel + " in time zone " + timezone + ".")
	if cfg.ExternalURL == "" {
		logger.Log.Warn("External URL is not set. Rewritten feeds need it to point Audiobookshelf back at Solstein; set it with -externalurl or SOLSTEIN_EXTERNAL_URL.")
	}

	store, err := database.Open(startup.ConfigDir)
	if err != nil {
		logger.Log.Error("Failed to open database. Error: " + err.Error())
		return 1
	}
	defer store.Close()
	logger.Log.Info("Database opened.")

	exits, err := outbound.New(outbound.Options{
		UserAgent:                "Solstein/" + version + " (+https://github.com/aunefyren/solstein)",
		AllowPrivateDestinations: cfg.AllowPrivateDestinations,
	})
	if err != nil {
		logger.Log.Error("Failed to set up exits. Error: " + err.Error())
		return 1
	}
	logger.Log.Info("Exits available: " + strings.Join(exits.Exits(), ", ") + ".")
	if cfg.AllowPrivateDestinations {
		logger.Log.Warn("Private destinations are allowed; Solstein can fetch from loopback and internal network addresses.")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	feedService := feeds.New(store, exits, feeds.Options{
		DefaultDeliveryMode: cfg.DeliveryMode,
		AllowedSourceHosts:  cfg.AllowedSourceHosts,
	})
	if cfg.DisableAuth {
		logger.Log.Warn("Auth is disabled: anyone who can reach Solstein can subscribe to feeds through it. Only use this on a private network.")
	} else {
		logger.Log.Info("Subscribe with <external URL>/api/rss/<auth_token>/<feed URL>; the token is auth_token in config.json.")
	}

	cache, err := episodes.NewCache(filepath.Join(startup.ConfigDir, "cache"))
	if err != nil {
		logger.Log.Error("Failed to set up the episode cache. Error: " + err.Error())
		return 1
	}
	pipeline := episodes.NewPipeline(store, exits, cache, episodes.Options{
		DefaultDeliveryMode: cfg.DeliveryMode,
		Workers:             downloadWorkers,
	})
	if err := pipeline.Recover(ctx); err != nil {
		logger.Log.Error("Failed to recover interrupted downloads. Error: " + err.Error())
		return 1
	}
	poller := feeds.NewPoller(feedService, time.Duration(cfg.PollIntervalMinutes)*time.Minute, pipeline.Wake)

	episodeServer := episodes.NewServer(store, exits, cache, feedService, episodes.Options{})

	srv, err := server.New(server.Options{Config: cfg, Version: version, Feeds: feedService, Episodes: episodeServer})
	if err != nil {
		logger.Log.Error("Failed to set up HTTP server. Error: " + err.Error())
		return 1
	}

	// The poller and pipeline stop with ctx; they are waited for before the
	// database closes.
	var background sync.WaitGroup
	background.Go(func() { pipeline.Run(ctx) })
	background.Go(func() { poller.Run(ctx) })

	exitCode := 0
	logger.Log.Info("Starting HTTP server on " + srv.Addr + ".")
	if err := server.Run(ctx, srv); err != nil {
		logger.Log.Error("HTTP server stopped. Error: " + err.Error())
		exitCode = 1
	}
	stop()
	background.Wait()

	logger.Log.Info("Solstein stopped.")
	return exitCode
}
