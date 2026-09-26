package database

import (
	"context"
	"testing"
	"time"

	"aunefyren/solstein/models"

	"github.com/google/uuid"
)

// TestClosedStoreReturnsErrors checks that every query reports a database
// that has gone away, rather than an empty result that looks like data.
func TestClosedStoreReturnsErrors(t *testing.T) {
	store := openTestStore(t)
	store.Close()
	ctx := context.Background()
	now := time.Now()
	ids := []uuid.UUID{uuid.New()}
	feedID := uuid.New()
	withID := func(feed models.Feed) *models.Feed {
		feed.ID = feedID
		return &feed
	}

	calls := map[string]func() error{
		"CreateFeed": func() error { return store.CreateFeed(ctx, &models.Feed{SourceURL: "https://a.example/feed"}) },
		"GetFeed":    func() error { _, err := store.GetFeed(ctx, feedID); return err },
		"GetFeedBySourceURL": func() error {
			_, err := store.GetFeedBySourceURL(ctx, "https://a.example/feed")
			return err
		},
		"ListFeeds":        func() error { _, err := store.ListFeeds(ctx); return err },
		"UpdateFeed":       func() error { return store.UpdateFeed(ctx, withID(models.Feed{Title: "x"})) },
		"DeleteFeed":       func() error { return store.DeleteFeed(ctx, feedID) },
		"SaveFeedDocument": func() error { return store.SaveFeedDocument(ctx, feedID, []byte("<rss/>"), now) },
		"GetFeedDocument":  func() error { _, err := store.GetFeedDocument(ctx, feedID); return err },
		"CreateEpisode": func() error {
			return store.CreateEpisode(ctx, &models.Episode{FeedID: feedID, GUID: "a", SourceURL: "https://a.example/a.mp3"})
		},
		"GetEpisode":       func() error { _, err := store.GetEpisode(ctx, feedID, ids[0]); return err },
		"ListEpisodes":     func() error { _, err := store.ListEpisodes(ctx, feedID); return err },
		"MarkReleased":     func() error { return store.MarkReleased(ctx, ids, now) },
		"UpdateEpisode":    func() error { e := models.Episode{FeedID: feedID}; e.ID = ids[0]; return store.UpdateEpisode(ctx, &e) },
		"ListCachedBefore": func() error { _, err := store.ListCachedBefore(ctx, now); return err },
		"CacheFiles":       func() error { _, err := store.CacheFiles(ctx); return err },
		"ClaimNextEpisode": func() error {
			_, err := store.ClaimNextEpisode(ctx, now, "cache", ids)
			return err
		},
		"ClaimQueuedEpisode": func() error {
			_, err := store.ClaimQueuedEpisode(ctx, now, now.Add(time.Minute), "cache", ids)
			return err
		},
		"ClaimEpisode":             func() error { _, err := store.ClaimEpisode(ctx, ids[0], now); return err },
		"ResetInterruptedEpisodes": func() error { _, err := store.ResetInterruptedEpisodes(ctx); return err },
		"QueueFailedEpisodes":      func() error { _, err := store.QueueFailedEpisodes(ctx, ids, now); return err },
		"QueueUncachedEpisodes":    func() error { _, err := store.QueueUncachedEpisodes(ctx, ids, now); return err },
		"CreateSubscription": func() error {
			return store.CreateSubscription(ctx, &models.Feed{SourceURL: "https://b.example/feed"}, []byte("<rss/>"), now,
				[]models.Episode{{GUID: "a", SourceURL: "https://b.example/a.mp3"}})
		},
		"AddNewEpisodes": func() error {
			_, err := store.AddNewEpisodes(ctx, feedID, []models.Episode{{GUID: "a", SourceURL: "https://a.example/a.mp3"}})
			return err
		},
	}
	for name, call := range calls {
		if err := call(); err == nil {
			t.Errorf("%s on a closed store: no error", name)
		}
	}
}
