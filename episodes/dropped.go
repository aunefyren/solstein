package episodes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"aunefyren/solstein/database"
	"aunefyren/solstein/feeds"
	"aunefyren/solstein/logger"
	"aunefyren/solstein/models"

	"github.com/google/uuid"
)

// Episodes the source feed no longer lists (docs/episodes.md, Dropped
// episodes): checking that a served one's audio is still there, keeping a
// served one's file, deleting the rest when the feed says so, and the feed
// page's per-episode serve switch and delete button.

// ErrNotDropped means the source still lists the episode, so it can't be
// deleted: the next poll would only find it again, as a new episode.
var ErrNotDropped = errors.New("the source still lists the episode")

// linkCheckTimeout bounds one check that a dropped episode's audio is still
// at its source.
const linkCheckTimeout = time.Minute

// sourceGone is the error for a source answering that the audio isn't
// there (ErrSourceGone), with the status it answered.
type sourceGone struct{ status string }

func (gone sourceGone) Error() string        { return "source answered " + gone.status }
func (gone sourceGone) Is(target error) bool { return target == ErrSourceGone }

// noteGone stops serving a dropped episode whose source answered that its
// audio is gone, with a warning saying so: kept in the feed, it would only
// fail every client that asks. An episode still in the source is left
// alone; its failure is the pipeline's to handle.
func noteGone(ctx context.Context, store *database.Store, now time.Time, feed models.Feed, episode models.Episode, err error) {
	var gone sourceGone
	if episode.DroppedAt == nil || !errors.As(err, &gone) {
		return
	}
	warning := "Its source answered " + gone.status + " on " + now.Local().Format("2 Jan 2006 15:04") + ": the audio is gone, so it is no longer served."
	if storeErr := store.SetEpisodeServe(context.WithoutCancel(ctx), episode.FeedID, episode.ID, "off", warning); storeErr != nil {
		logger.Log.Error("Failed to stop serving episode '" + episode.Title + "', whose audio is gone. Error: " + storeErr.Error())
		return
	}
	logger.Log.Warn("Stopped serving episode '" + episode.Title + "' of '" + feed.Title + "', which its source no longer lists: the audio is gone too. Error: " + withoutURLs(err))
}

// checkLink asks a dropped episode's source for its first byte, and stops
// serving the episode if the source answers that the audio is gone. Any
// other failure (a timeout, an exit without a tunnel) says nothing about the
// audio, so it is only logged at debug level and the episode stays served.
func (pipeline *Pipeline) checkLink(ctx context.Context, feed models.Feed, episode models.Episode) {
	client, err := pipeline.exits.Client(feed.Exit)
	if err != nil {
		logger.Log.Debug("Couldn't check that the audio of dropped episode '" + episode.Title + "' is still there. Error: " + err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(ctx, linkCheckTimeout)
	defer cancel()
	response, err := requestSource(ctx, client, http.MethodGet, episode.SourceURL, pipeline.options.SkipTrackers, func(request *http.Request) {
		request.Header.Set("Accept", "*/*")
		request.Header.Set("Range", "bytes=0-0")
	}, checkResponse)
	if err != nil {
		if errors.Is(err, ErrSourceGone) {
			noteGone(ctx, pipeline.store, pipeline.options.Now(), feed, episode, err)
			return
		}
		logger.Log.Debug("Couldn't check that the audio of dropped episode '" + episode.Title + "' is still there. Error: " + withoutURLs(err))
		return
	}
	response.Body.Close()
}

// WithDropped has the housekeeper look after episodes their source no
// longer lists, as feedService's settings say: keeping a served one's cached
// file, checking (once per run) that a served one without a file can still
// be fetched through pipeline, and deleting the rest once DroppedGrace has
// passed where delete_dropped is on. Without it, they are treated like any
// other episode.
func (housekeeper *Housekeeper) WithDropped(feedService *feeds.Service, pipeline *Pipeline) *Housekeeper {
	housekeeper.feeds, housekeeper.pipeline = feedService, pipeline
	housekeeper.checked = map[uuid.UUID]bool{}
	return housekeeper
}

// keptFor returns what tells whether an episode's cached file is kept
// whatever the cache settings say: a dropped episode that is still served,
// whose source may well not have it any more. Nil keeps nothing.
func (housekeeper *Housekeeper) keptFor(ctx context.Context) (func(models.Episode) bool, error) {
	if housekeeper.feeds == nil {
		return nil, nil
	}
	feedsByID, err := housekeeper.feedsByID(ctx)
	if err != nil {
		return nil, err
	}
	return func(episode models.Episode) bool {
		feed, ok := feedsByID[episode.FeedID]
		return ok && episode.DroppedAt != nil && housekeeper.feeds.ServesDropped(feed, episode)
	}, nil
}

func (housekeeper *Housekeeper) feedsByID(ctx context.Context) (map[uuid.UUID]models.Feed, error) {
	list, err := housekeeper.store.ListFeeds(ctx)
	if err != nil {
		return nil, err
	}
	feedsByID := make(map[uuid.UUID]models.Feed, len(list))
	for _, feed := range list {
		feedsByID[feed.ID] = feed
	}
	return feedsByID, nil
}

// sweepDropped checks the served dropped episodes without a file, and
// deletes the unserved ones of feeds that delete them, once they have been
// dropped for DroppedGrace. It returns how many it deleted.
func (housekeeper *Housekeeper) sweepDropped(ctx context.Context) (int, error) {
	if housekeeper.feeds == nil {
		return 0, nil
	}
	dropped, err := housekeeper.store.ListDropped(ctx)
	if err != nil || len(dropped) == 0 {
		return 0, err
	}
	feedsByID, err := housekeeper.feedsByID(ctx)
	if err != nil {
		return 0, err
	}
	cutoff := housekeeper.now().UTC().Add(-feeds.DroppedGrace)
	deleted := 0
	for _, episode := range dropped {
		if ctx.Err() != nil {
			return deleted, nil
		}
		feed, ok := feedsByID[episode.FeedID]
		if !ok {
			continue
		}
		served := housekeeper.feeds.ServesDropped(feed, episode)
		switch {
		case served && episode.CacheFile == "" && housekeeper.pipeline != nil && !housekeeper.checked[episode.ID]:
			housekeeper.checked[episode.ID] = true
			housekeeper.pipeline.checkLink(ctx, feed, episode)
		case !served && housekeeper.feeds.DeletesDropped(feed) && episode.DroppedAt.Before(cutoff):
			if housekeeper.pipeline != nil && housekeeper.pipeline.preparing(episode.ID) {
				continue // a client is being served it; next sweep
			}
			ok, err := housekeeper.deleteEpisode(ctx, episode)
			if err != nil {
				return deleted, err
			}
			if ok {
				logger.Log.Info("Deleted episode '" + episode.Title + "' of '" + feed.Title + "', which its source no longer lists (delete_dropped).")
				deleted++
			}
		}
	}
	return deleted, nil
}

// deleteEpisode removes an episode and its cached file. It reports false,
// without error, when the file couldn't be deleted (being served, on
// Windows), leaving both for the next sweep.
func (housekeeper *Housekeeper) deleteEpisode(ctx context.Context, episode models.Episode) (bool, error) {
	if !removeCacheFile(housekeeper.cache, episode) {
		return false, nil
	}
	if err := housekeeper.store.DeleteEpisode(ctx, episode.FeedID, episode.ID); err != nil && !errors.Is(err, database.ErrEpisodeNotFound) {
		return false, err
	}
	return true, nil
}

// removeCacheFile deletes an episode's cached file, if it has one. It
// reports false when the file is there but couldn't be deleted.
func removeCacheFile(cache Cache, episode models.Episode) bool {
	if episode.CacheFile == "" {
		return true
	}
	fullPath, err := cache.Path(episode.CacheFile)
	if err != nil {
		logger.Log.Warn("Not deleting the cache file of episode '" + episode.Title + "': " + err.Error())
		return false
	}
	if err := os.Remove(fullPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		logger.Log.Warn("Failed to delete the cache file of episode '" + episode.Title + "'; will retry. Error: " + err.Error())
		return false
	}
	return true
}

// DeleteEpisode deletes one episode the source no longer lists, with its
// cached file, from the feed page. It returns ErrNotDropped for one the
// source still lists, ErrEpisodeBusy while it is being prepared or served
// into the cache, and database.ErrEpisodeNotFound for an unknown one.
func (server *Server) DeleteEpisode(ctx context.Context, feedID, episodeID uuid.UUID) (models.Episode, error) {
	episode, err := server.store.GetEpisode(ctx, feedID, episodeID)
	if err != nil {
		return models.Episode{}, err
	}
	if episode.DroppedAt == nil {
		return models.Episode{}, ErrNotDropped
	}
	if (server.pipeline != nil && server.pipeline.preparing(episode.ID)) || server.teeingNow(episode.ID) {
		return models.Episode{}, ErrEpisodeBusy
	}
	if !removeCacheFile(server.cache, episode) {
		return models.Episode{}, fmt.Errorf("the cache file of episode '%s' couldn't be deleted", episode.Title)
	}
	if err := server.store.DeleteEpisode(ctx, feedID, episodeID); err != nil {
		return models.Episode{}, err
	}
	return episode, nil
}

// SetServe sets whether one episode is served once its source no longer
// lists it: "on", "off", or empty to follow the feed. Setting it by hand
// clears any warning Solstein gave when it switched serving off itself.
func (server *Server) SetServe(ctx context.Context, feedID, episodeID uuid.UUID, serve string) (models.Episode, error) {
	if serve != "" && serve != "on" && serve != "off" {
		return models.Episode{}, fmt.Errorf("%w: serve must be \"on\", \"off\" or empty", feeds.ErrInvalidSettings)
	}
	episode, err := server.store.GetEpisode(ctx, feedID, episodeID)
	if err != nil {
		return models.Episode{}, err
	}
	if err := server.store.SetEpisodeServe(ctx, feedID, episodeID, serve, ""); err != nil {
		return models.Episode{}, err
	}
	episode.Serve, episode.ServeWarning = serve, ""
	return episode, nil
}

func (server *Server) teeingNow(episodeID uuid.UUID) bool {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	return server.teeing[episodeID]
}
