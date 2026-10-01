package database

import (
	"context"
	"errors"
	"testing"
	"time"

	"aunefyren/solstein/models"
)

func TestCreateSubscription(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	fetchedAt := time.Now().UTC().Truncate(time.Second)

	feed := models.Feed{SourceURL: "https://example.com/feed", Title: "Show"}
	episodes := []models.Episode{
		{GUID: "a", SourceURL: "https://example.com/a.mp3", State: models.EpisodeReady, Backlog: true},
		{GUID: "b", SourceURL: "https://example.com/b.mp3", State: models.EpisodeReady, Backlog: true},
	}
	if err := store.CreateSubscription(ctx, &feed, []byte("<rss/>"), fetchedAt, episodes); err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}

	document, err := store.GetFeedDocument(ctx, feed.ID)
	if err != nil || string(document.Data) != "<rss/>" || !document.FetchedAt.Equal(fetchedAt) {
		t.Errorf("document = %+v, %v", document, err)
	}
	stored, err := store.ListEpisodes(ctx, feed.ID)
	if err != nil || len(stored) != 2 || !stored[0].Backlog {
		t.Errorf("episodes = %+v, %v", stored, err)
	}

	again := models.Feed{SourceURL: "https://example.com/feed"}
	if err := store.CreateSubscription(ctx, &again, []byte("<rss/>"), fetchedAt, nil); !errors.Is(err, ErrFeedExists) {
		t.Errorf("duplicate: err = %v, want ErrFeedExists", err)
	}
}

func TestCreateSubscriptionIsAtomic(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	// Two episodes with the same GUID violate the unique index, so the whole
	// subscription must roll back.
	feed := models.Feed{SourceURL: "https://example.com/feed"}
	episodes := []models.Episode{
		{GUID: "same", SourceURL: "https://example.com/1.mp3", State: models.EpisodeReady},
		{GUID: "same", SourceURL: "https://example.com/2.mp3", State: models.EpisodeReady},
	}
	if err := store.CreateSubscription(ctx, &feed, []byte("<rss/>"), time.Now(), episodes); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := store.GetFeedBySourceURL(ctx, "https://example.com/feed"); !errors.Is(err, ErrFeedNotFound) {
		t.Errorf("feed left behind after failed subscription: err = %v", err)
	}
}

func TestSyncEpisodes(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	feed := createTestFeed(t, store, "https://example.com/feed")
	existing := createTestEpisode(t, store, feed.ID, "old", nil)
	existing.State = models.EpisodeReady
	if err := store.UpdateEpisode(ctx, &existing); err != nil {
		t.Fatal(err)
	}

	added, err := store.SyncEpisodes(ctx, feed.ID, []models.Episode{
		{GUID: "old", SourceURL: "https://example.com/changed.mp3", State: models.EpisodeDiscovered},
		{GUID: "new", SourceURL: "https://example.com/new.mp3", State: models.EpisodeDiscovered},
		{GUID: "new", SourceURL: "https://example.com/dup.mp3", State: models.EpisodeDiscovered},
	}, Hiding{})
	if err != nil {
		t.Fatalf("SyncEpisodes: %v", err)
	}
	if len(added) != 1 || added[0].GUID != "new" || added[0].FeedID != feed.ID {
		t.Errorf("added = %+v", added)
	}

	kept, err := store.GetEpisode(ctx, feed.ID, existing.ID)
	if err != nil || kept.State != models.EpisodeReady || kept.SourceURL != existing.SourceURL {
		t.Errorf("existing episode changed: %+v, %v", kept, err)
	}

	added, err = store.SyncEpisodes(ctx, feed.ID, nil, Hiding{})
	if err != nil || len(added) != 0 {
		t.Errorf("empty add = %+v, %v", added, err)
	}
}

func TestFeedDocument(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	feed := createTestFeed(t, store, "https://example.com/feed")

	if _, err := store.GetFeedDocument(ctx, feed.ID); !errors.Is(err, ErrFeedDocumentNotFound) {
		t.Errorf("before save: err = %v, want ErrFeedDocumentNotFound", err)
	}

	first := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	if err := store.SaveFeedDocument(ctx, feed.ID, []byte("one"), first); err != nil {
		t.Fatal(err)
	}
	second := first.Add(time.Hour)
	if err := store.SaveFeedDocument(ctx, feed.ID, []byte("two"), second); err != nil {
		t.Fatal(err)
	}
	document, err := store.GetFeedDocument(ctx, feed.ID)
	if err != nil || string(document.Data) != "two" || !document.FetchedAt.Equal(second) {
		t.Errorf("document = %+v, %v", document, err)
	}

	if err := store.DeleteFeed(ctx, feed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetFeedDocument(ctx, feed.ID); !errors.Is(err, ErrFeedDocumentNotFound) {
		t.Errorf("document survived feed deletion: err = %v", err)
	}
}

// TestSyncEpisodesReadsRulesInItsTransaction: the hider is built from the
// rules as stored when the episodes are, not from a copy the caller loaded
// earlier.
func TestSyncEpisodesReadsRulesInItsTransaction(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	feed := createTestFeed(t, store, "https://example.com/feed")
	var seen []models.FeedRule
	hiding := Hiding{HiderFor: func(rules []models.FeedRule) func(models.Episode) bool {
		seen = rules
		return func(models.Episode) bool { return len(rules) > 0 }
	}}
	if _, err := store.ReplaceFeedRules(ctx, feed.ID, []models.FeedRule{{Action: "hide"}}, Hiding{}); err != nil {
		t.Fatal(err)
	}
	added, err := store.SyncEpisodes(ctx, feed.ID, []models.Episode{{GUID: "a", SourceURL: "https://example.com/a.mp3", State: models.EpisodeDiscovered}}, hiding)
	if err != nil || len(added) != 1 || !added[0].Hidden || len(seen) != 1 || seen[0].Action != "hide" {
		t.Fatalf("added %+v, %v; rules seen %+v", added, err, seen)
	}
	stored, _ := store.GetEpisode(ctx, feed.ID, added[0].ID)
	if !stored.Hidden {
		t.Error("stored without its hidden flag")
	}
}
