package episodes

import (
	"context"
	"fmt"
	"os"

	"aunefyren/solstein/logger"
	"aunefyren/solstein/models"

	"github.com/google/uuid"
)

// recipe describes how a feed's episodes are prepared with the current
// settings: which processor with which settings, or which exit a plain
// download goes through. Empty means nothing is kept (stream and original
// mode). It is recorded on each episode as it is prepared (PreparedWith), so
// Reconcile can tell when the settings behind a file have changed.
func (pipeline *Pipeline) recipe(feed models.Feed) string {
	if processor := pipeline.options.Processor; processor != nil && processor.Handles(feed) {
		return processor.Name() + ": " + processor.Recipe(feed)
	}
	mode := feed.DeliveryMode
	if mode == "" {
		mode = pipeline.options.DefaultDeliveryMode
	}
	if mode != "cache" {
		return ""
	}
	exit := feed.Exit
	if exit == "" {
		exit = pipeline.exits.DefaultExit()
	}
	return "download through " + exit
}

// failedRecipe is recorded on an episode given up on: the recipe it failed
// under, and what the failure policy did with it. A change to either gives
// the episode another chance.
func (pipeline *Pipeline) failedRecipe(feed models.Feed) string {
	outcome := "published unprocessed"
	if processor := pipeline.options.Processor; processor != nil && processor.Handles(feed) && processor.HideOnFailure(feed) {
		outcome = "withheld"
	}
	return pipeline.recipe(feed) + "; failed, " + outcome
}

// prepares reports whether a feed's episodes are prepared by the pipeline
// (cache mode, or processed) rather than published as they are.
func (pipeline *Pipeline) prepares(feed models.Feed) bool {
	return pipeline.recipe(feed) != ""
}

// Reconcile brings episodes in line with their feed's current settings, for
// one feed or, with uuid.Nil, all of them. Run it at start-up (config.json
// may have changed) and after a feed's settings change. A cached file made
// with other settings — another exit, region diff switched on or off, other
// diff settings — is deleted, so the next request prepares the episode
// again as the settings now say. A failed episode whose settings or failure
// policy changed gets a fresh start: it is retried from the first attempt.
// Episodes being prepared are left alone; they are checked when served.
func (pipeline *Pipeline) Reconcile(ctx context.Context, feedID uuid.UUID) error {
	var list []models.Feed
	if feedID == uuid.Nil {
		var err error
		if list, err = pipeline.store.ListFeeds(ctx); err != nil {
			return err
		}
	} else {
		feed, err := pipeline.store.GetFeed(ctx, feedID)
		if err != nil {
			return err
		}
		list = []models.Feed{feed}
	}

	retried := false
	for _, feed := range list {
		episodes, err := pipeline.store.ListEpisodes(ctx, feed.ID)
		if err != nil {
			return err
		}
		cleared, restarted := 0, 0
		for _, episode := range episodes {
			if pipeline.preparing(episode.ID) {
				continue // checked when served; its outcome would overwrite this
			}
			changed, outcome := pipeline.reconcileEpisode(feed, &episode)
			if !changed {
				continue
			}
			if err := pipeline.store.UpdateEpisode(ctx, &episode); err != nil {
				return err
			}
			switch outcome {
			case cacheCleared:
				cleared++
			case failureCleared:
				restarted++
				retried = retried || episode.State == models.EpisodeDiscovered
			}
		}
		if cleared > 0 || restarted > 0 {
			logger.Log.Info(fmt.Sprintf("Settings of feed '%s' changed: cleared %d cached episodes made with the old settings, and %d failed episodes will be tried again.", feed.Title, cleared, restarted))
		}
	}
	if retried {
		pipeline.Wake()
	}
	return nil
}

// CheckEpisode reconciles one episode as it is about to be served, and
// saves it if that changed anything. It catches files a preparation that
// was already running during a settings change recorded with the old ones.
func (pipeline *Pipeline) CheckEpisode(ctx context.Context, feed models.Feed, episode models.Episode) (models.Episode, error) {
	if pipeline.preparing(episode.ID) {
		return episode, nil // checked again once its job is done
	}
	if changed, _ := pipeline.reconcileEpisode(feed, &episode); changed {
		if err := pipeline.store.UpdateEpisode(ctx, &episode); err != nil {
			return episode, err
		}
	}
	return episode, nil
}

type reconcileOutcome int

const (
	unchanged reconcileOutcome = iota
	recorded                   // an old episode's settings were recorded
	cacheCleared
	failureCleared
)

// reconcileEpisode applies Reconcile's rules to one episode, in memory. It
// reports whether the episode changed.
func (pipeline *Pipeline) reconcileEpisode(feed models.Feed, episode *models.Episode) (bool, reconcileOutcome) {
	switch {
	case episode.State == models.EpisodeFailed:
		want := pipeline.failedRecipe(feed)
		if episode.PreparedWith == want || episode.PreparedWith == "" {
			changed := false
			if episode.PreparedWith == "" {
				// From before settings were recorded: assume the current ones
				// rather than retry every old failure at once.
				episode.PreparedWith, changed = want, true
			}
			if episode.Withheld && episode.NextAttemptAt == nil && episode.LateRetries == 0 {
				// Withheld before withheld episodes were retried: start its
				// slow retries now.
				now := pipeline.options.Now().UTC()
				episode.NextAttemptAt, changed = &now, true
			}
			if !changed {
				return false, unchanged
			}
			return true, recorded
		}
		pipeline.removeCacheFile(episode)
		episode.ForgetCache()
		episode.Attempts, episode.FailedAttempts, episode.NextAttemptAt, episode.LastError = 0, 0, nil, ""
		episode.LateRetries = 0
		episode.Withheld, episode.ProcessNote, episode.PreparedWith = false, "", ""
		episode.State = models.EpisodeDiscovered
		if episode.Backlog || !pipeline.prepares(feed) {
			// Published as it is, and fetched or processed on request.
			episode.State = models.EpisodeReady
		}
		return true, failureCleared

	case episode.CacheFile != "" && episode.State == models.EpisodeReady:
		want := pipeline.recipe(feed)
		if episode.PreparedWith == "" {
			// From before settings were recorded. A processed feed's file
			// without a processor's note wasn't processed; anything else is
			// taken to match the current settings.
			processed := pipeline.options.Processor != nil && pipeline.options.Processor.Handles(feed)
			if !processed || episode.ProcessNote != "" {
				episode.PreparedWith = want
				return true, recorded
			}
		} else if episode.PreparedWith == want {
			return false, unchanged
		}
		pipeline.removeCacheFile(episode)
		episode.ForgetCache()
		episode.ProcessNote, episode.PreparedWith = "", ""
		return true, cacheCleared
	}
	return false, unchanged
}

// removeCacheFile deletes an episode's cached file. A file that can't be
// deleted now (being served, on Windows) is left for housekeeping, which
// removes files no episode refers to.
func (pipeline *Pipeline) removeCacheFile(episode *models.Episode) {
	if episode.CacheFile == "" {
		return
	}
	if path, err := pipeline.cache.Path(episode.CacheFile); err == nil {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			logger.Log.Warn("Failed to delete outdated cached file of episode '" + episode.Title + "'; housekeeping will. Error: " + err.Error())
		}
	}
}
