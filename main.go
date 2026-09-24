package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"aunefyren/solstein/database"
	"aunefyren/solstein/logger"
	"aunefyren/solstein/outbound"
	"aunefyren/solstein/server"
	"aunefyren/solstein/settings"

	// Embeds the time zone database so -timezone works on hosts without one
	// (Windows, minimal containers).
	_ "time/tzdata"
)

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

	srv := server.New(cfg, version)
	logger.Log.Info("Starting HTTP server on " + srv.Addr + ".")
	if err := server.Run(ctx, srv); err != nil {
		logger.Log.Error("HTTP server stopped. Error: " + err.Error())
		return 1
	}

	logger.Log.Info("Solstein stopped.")
	return 0
}
