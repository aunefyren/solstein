package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"aunefyren/solstein/database"
	"aunefyren/solstein/episodes"
	"aunefyren/solstein/feeds"
	"aunefyren/solstein/logger"
	"aunefyren/solstein/models"
	"aunefyren/solstein/modules/exits"
	"aunefyren/solstein/modules/regiondiff"
	"aunefyren/solstein/outbound"
	"aunefyren/solstein/server"
	"aunefyren/solstein/settings"

	"github.com/google/uuid"

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

	// The VPN module is optional: with no usable providers it stays off and
	// only "direct" exists.
	vpnModule, vpnWarnings := exits.Setup(cfg.VPN, startup.ConfigDir, os.Getenv)
	for _, warning := range vpnWarnings {
		logger.Log.Warn("VPN: " + warning)
	}
	var exitProviders []outbound.Provider
	if vpnModule != nil {
		exitProviders = append(exitProviders, vpnModule)
		logger.Log.Info("VPN module on: " + vpnModule.Summary() + ".")
	}

	exitManager, err := outbound.New(outbound.Options{
		UserAgent:                "Solstein/" + version + " (+https://github.com/aunefyren/solstein)",
		AllowPrivateDestinations: cfg.AllowPrivateDestinations,
		Providers:                exitProviders,
		DefaultExit:              cfg.DefaultExit,
		DisableDirect:            cfg.DisableDirect,
	})
	if err != nil {
		// default_exit and disable_direct problems end up here: refusing to
		// start is better than sending traffic the operator said mustn't
		// leave directly.
		logger.Log.Error("Failed to set up exits. Error: " + err.Error())
		return 1
	}
	logger.Log.Info("Exits available: " + strings.Join(exitManager.Exits(), ", ") + "; default: " + exitManager.DefaultExit() + ".")
	if cfg.DisableDirect {
		logger.Log.Info("The direct exit is disabled: nothing goes out on this host's own connection except the VPN tunnels themselves.")
	}
	if vpnModule != nil {
		// Server-list refreshes take the default route like everything else,
		// with the core's safeguards; the built-in list covers the start.
		defaultClient, err := exitManager.Client("")
		if err != nil {
			logger.Log.Error("Failed to get the default exit's client. Error: " + err.Error())
			return 1
		}
		vpnModule.SetFetchClient(defaultClient)
	}
	if cfg.AllowPrivateDestinations {
		logger.Log.Warn("Private destinations are allowed; Solstein can fetch from loopback and internal network addresses.")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Region diff is optional too: off unless region_diff names two exits
	// that exist. It needs nothing started; it runs inside the pipeline.
	regionDiff, regionDiffWarnings := regiondiff.Setup(cfg.RegionDiff, exitManager.Exits(), exitManager, startup.ConfigDir)
	for _, warning := range regionDiffWarnings {
		logger.Log.Warn("Region diff: " + warning)
	}
	feedOptions := feeds.Options{
		DefaultDeliveryMode: cfg.DeliveryMode,
		AllowedSourceHosts:  cfg.AllowedSourceHosts,
	}
	var processor episodes.Processor
	if regionDiff != nil {
		processor = regionDiff
		feedOptions.RegionDiffAvailable = true
		feedOptions.Processed = regionDiff.Handles
		feedOptions.ProcessBacklog = cfg.RegionDiff.Backlog
		logger.Log.Info("Region diff on: " + regionDiff.Summary() + ".")
	}

	feedService := feeds.New(store, exitManager, feedOptions)
	warnAboutFeedSettings(ctx, feedService, exitManager.Exits(), regionDiff != nil)
	if regionDiff != nil {
		warnAboutWithholding(ctx, feedService, cfg.RegionDiff.OnFailure == "hide", regionDiff.HideOnFailure)
	}
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
	pipeline := episodes.NewPipeline(store, exitManager, cache, episodes.Options{
		DefaultDeliveryMode: cfg.DeliveryMode,
		Workers:             downloadWorkers,
		Processor:           processor,
	})
	if err := pipeline.Recover(ctx); err != nil {
		logger.Log.Error("Failed to recover interrupted downloads. Error: " + err.Error())
		return 1
	}
	// config.json may have changed since the last run.
	if err := pipeline.Reconcile(ctx, uuid.Nil); err != nil {
		logger.Log.Error("Failed to bring episodes in line with the settings. Error: " + err.Error())
		return 1
	}
	poller := feeds.NewPoller(feedService, time.Duration(cfg.PollIntervalMinutes)*time.Minute, pipeline.Wake)

	episodeServer := episodes.NewServer(store, exitManager, cache, feedService, pipeline, episodes.Options{})

	srv, err := server.New(server.Options{Config: cfg, Version: version, Feeds: feedService, Episodes: episodeServer})
	if err != nil {
		logger.Log.Error("Failed to set up HTTP server. Error: " + err.Error())
		return 1
	}

	// The poller, pipeline and housekeeper stop with ctx; they are waited for before the
	// database closes.
	housekeeper := episodes.NewHousekeeper(store, cache, time.Duration(cfg.CacheRetentionDays)*24*time.Hour, nil)

	var background sync.WaitGroup
	background.Go(func() { pipeline.Run(ctx) })
	background.Go(func() { poller.Run(ctx) })
	background.Go(func() { housekeeper.Run(ctx) })
	if vpnModule != nil {
		background.Go(func() { vpnModule.Run(ctx) })
	}

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

// warnAboutFeedSettings flags feeds whose settings can't take effect: an
// exit that no longer exists (removed from config.json, or its provider
// disabled), whose polls and downloads fail until it is back or the feed is
// changed; or region diff switched on while the module is off.
func warnAboutFeedSettings(ctx context.Context, feedService *feeds.Service, available []string, regionDiffRunning bool) {
	list, err := feedService.List(ctx)
	if err != nil {
		logger.Log.Error("Failed to check feeds' settings. Error: " + err.Error())
		return
	}
	for _, feed := range list {
		if feed.Exit != "" && !slices.Contains(available, feed.Exit) {
			logger.Log.Warn("Feed '" + feed.Title + "' uses exit '" + feed.Exit + "', which isn't available; it can't be polled or downloaded until the exit is back or the feed's exit is changed.")
		}
		if feed.RegionDiff == "on" && !regionDiffRunning {
			logger.Log.Warn("Feed '" + feed.Title + "' has region diff switched on, but region diff is off; its episodes are served with their ads.")
		}
		for _, exit := range feed.RegionDiffExits {
			if regionDiffRunning && !slices.Contains(available, exit) {
				logger.Log.Warn("Feed '" + feed.Title + "' compares through exit '" + exit + "', which isn't available; its episodes can't be processed until the exit is back or the feed's region_diff_exits is changed.")
			}
		}
	}
}

// warnAboutWithholding says, when any feed's region-diff failure policy is
// hide, that episodes which can't be cleaned are kept out of the feed, and
// when and how they are tried again. Without it, an episode missing from a
// feed looks like a bug.
func warnAboutWithholding(ctx context.Context, feedService *feeds.Service, globalHide bool, hideOnFailure func(models.Feed) bool) {
	list, err := feedService.List(ctx)
	if err != nil {
		logger.Log.Error("Failed to check feeds' failure policies. Error: " + err.Error())
		return
	}
	var hiding []string
	for _, feed := range list {
		if feedService.Processed(feed) && hideOnFailure(feed) {
			hiding = append(hiding, "'"+feed.Title+"'")
		}
	}
	scope := ""
	switch {
	case globalHide && len(hiding) == len(list):
		scope = "every feed"
	case len(hiding) > 0:
		scope = "feeds " + strings.Join(hiding, ", ")
	case globalHide:
		scope = "every feed region diff handles" // none does at the moment
	default:
		return
	}
	logger.Log.Warn("Region diff: for " + scope + ", episodes that can't be cleaned are kept out of the feed (on_failure: hide). " +
		"They are tried again " + episodes.WithheldRetrySchedule + ", then stay out until POST /api/v1/feeds/<feed ID>/retry or a change of settings.")
}
