package database

import (
	"context"
	"testing"
	"time"
)

func TestHousekeepingQueries(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	feed := createTestFeed(t, store, "https://example.com/feed")
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

	createTestEpisode(t, store, feed.ID, "uncached", nil)
	old, recent := now.Add(-20*24*time.Hour), now.Add(-time.Hour)
	for guid, cachedAt := range map[string]time.Time{"old": old, "recent": recent} {
		episode := createTestEpisode(t, store, feed.ID, guid, nil)
		episode.CacheFile, episode.CachedAt = feed.ID.String()+"/"+guid+".mp3", &cachedAt
		if err := store.UpdateEpisode(ctx, &episode); err != nil {
			t.Fatal(err)
		}
	}

	expired, err := store.ListCachedBefore(ctx, now.Add(-14*24*time.Hour))
	if err != nil || len(expired) != 1 || expired[0].GUID != "old" {
		t.Errorf("ListCachedBefore = %+v, %v", expired, err)
	}

	files, err := store.CacheFiles(ctx)
	if err != nil || len(files) != 2 || !files[feed.ID.String()+"/old.mp3"] || !files[feed.ID.String()+"/recent.mp3"] {
		t.Errorf("CacheFiles = %v, %v", files, err)
	}
}
