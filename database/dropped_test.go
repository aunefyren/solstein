package database

import (
	"context"
	"testing"
	"time"

	"aunefyren/solstein/models"
)

func TestSyncEpisodesMarksDropped(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	feed := createTestFeed(t, store, "https://example.com/feed")
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	a := models.Episode{GUID: "a", SourceURL: "https://example.com/a.mp3", State: models.EpisodeDiscovered, SourceItem: []byte("<item>a</item>")}
	b := models.Episode{GUID: "b", SourceURL: "https://example.com/b.mp3", State: models.EpisodeReady}
	if _, err := store.SyncEpisodes(ctx, feed.ID, []models.Episode{a, b}, Hiding{Now: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SyncEpisodes(ctx, feed.ID, []models.Episode{b}, Hiding{Now: now}); err != nil {
		t.Fatal(err)
	}
	episodes, _ := store.ListEpisodes(ctx, feed.ID)
	byGUID := map[string]models.Episode{}
	for _, episode := range episodes {
		byGUID[episode.GUID] = episode
	}
	if dropped := byGUID["a"].DroppedAt; dropped == nil || !dropped.Equal(now) || byGUID["b"].DroppedAt != nil {
		t.Fatalf("a should be dropped at %v, b not: %+v", now, episodes)
	}
	if string(byGUID["a"].SourceItem) != "<item>a</item>" {
		t.Errorf("a's kept item = %q", byGUID["a"].SourceItem)
	}

	// Never prepared in the background once dropped.
	if _, err := store.ClaimNextEpisode(ctx, now, "cache", nil); err != ErrNoWork {
		t.Errorf("ClaimNextEpisode = %v, want ErrNoWork with only a dropped episode waiting", err)
	}
	listed, _ := store.ListDropped(ctx)
	if len(listed) != 1 || listed[0].GUID != "a" {
		t.Errorf("ListDropped = %+v", listed)
	}
	if err := store.DeleteEpisode(ctx, feed.ID, byGUID["a"].ID); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteEpisode(ctx, feed.ID, byGUID["a"].ID); err != ErrEpisodeNotFound {
		t.Errorf("deleting twice = %v, want ErrEpisodeNotFound", err)
	}
}

// TestMigrationAsksEverySourceInFull: a database from before source items
// were kept has every feed's next poll made in full, so its episodes' items
// are kept and the dropped ones noticed even where the source hasn't
// changed since.
func TestMigrationAsksEverySourceInFull(t *testing.T) {
	directory := t.TempDir()
	store, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	feed := createTestFeed(t, store, "https://example.com/feed")
	feed.ETag, feed.LastModified = `"v1"`, "Thu, 01 Oct 2026 05:58:41 GMT"
	if err := store.UpdateFeed(context.Background(), &feed); err != nil {
		t.Fatal(err)
	}
	reopen := func() models.Feed {
		t.Helper()
		store.Close()
		if store, err = Open(directory); err != nil {
			t.Fatal(err)
		}
		reloaded, err := store.GetFeed(context.Background(), feed.ID)
		if err != nil {
			t.Fatal(err)
		}
		return reloaded
	}
	t.Cleanup(func() { store.Close() })

	if reloaded := reopen(); reloaded.ETag != `"v1"` {
		t.Fatal("reopening an up-to-date database cleared the ETag")
	}
	if err := store.db.Migrator().DropColumn(&models.Episode{}, "SourceItem"); err != nil {
		t.Fatal(err)
	}
	if reloaded := reopen(); reloaded.ETag != "" || reloaded.LastModified != "" {
		t.Errorf("after the upgrade: ETag %q, Last-Modified %q; want both cleared", reloaded.ETag, reloaded.LastModified)
	}
}
