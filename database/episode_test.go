package database

import (
	"context"
	"errors"
	"testing"
	"time"

	"aunefyren/solstein/models"

	"github.com/google/uuid"
)

func createTestEpisode(t *testing.T, store *Store, feedID uuid.UUID, guid string, publishedAt *time.Time) models.Episode {
	t.Helper()
	episode := models.Episode{
		FeedID:      feedID,
		GUID:        guid,
		SourceURL:   "https://example.com/" + guid + ".mp3",
		PublishedAt: publishedAt,
		State:       models.EpisodeDiscovered,
	}
	if err := store.CreateEpisode(context.Background(), &episode); err != nil {
		t.Fatalf("CreateEpisode: %v", err)
	}
	return episode
}

func TestCreateEpisode(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	feed := createTestFeed(t, store, "https://example.com/feed")
	episode := createTestEpisode(t, store, feed.ID, "guid-1", nil)

	if episode.ID == uuid.Nil {
		t.Fatal("ID not assigned")
	}

	duplicate := models.Episode{FeedID: feed.ID, GUID: "guid-1", SourceURL: "https://example.com/x.mp3", State: models.EpisodeDiscovered}
	if err := store.CreateEpisode(ctx, &duplicate); !errors.Is(err, ErrEpisodeExists) {
		t.Errorf("duplicate GUID: err = %v, want ErrEpisodeExists", err)
	}

	// The same GUID in another feed is a different episode.
	other := createTestFeed(t, store, "https://example.com/other")
	createTestEpisode(t, store, other.ID, "guid-1", nil)

	orphan := models.Episode{FeedID: uuid.New(), GUID: "guid-2", SourceURL: "https://example.com/y.mp3", State: models.EpisodeDiscovered}
	if err := store.CreateEpisode(ctx, &orphan); !errors.Is(err, ErrFeedNotFound) {
		t.Errorf("unknown feed: err = %v, want ErrFeedNotFound", err)
	}
}

func TestGetEpisodeScopedToFeed(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	feed := createTestFeed(t, store, "https://example.com/feed")
	other := createTestFeed(t, store, "https://example.com/other")
	episode := createTestEpisode(t, store, feed.ID, "guid-1", nil)

	got, err := store.GetEpisode(ctx, feed.ID, episode.ID)
	if err != nil || got.GUID != "guid-1" {
		t.Errorf("GetEpisode = %+v, %v", got, err)
	}
	if _, err := store.GetEpisode(ctx, other.ID, episode.ID); !errors.Is(err, ErrEpisodeNotFound) {
		t.Errorf("wrong feed: err = %v, want ErrEpisodeNotFound", err)
	}
	if _, err := store.GetEpisode(ctx, feed.ID, uuid.New()); !errors.Is(err, ErrEpisodeNotFound) {
		t.Errorf("unknown ID: err = %v, want ErrEpisodeNotFound", err)
	}
}

func TestListEpisodesPublishOrder(t *testing.T) {
	store := openTestStore(t)
	feed := createTestFeed(t, store, "https://example.com/feed")

	monday := time.Date(2026, 9, 21, 6, 0, 0, 0, time.UTC)
	tuesday := monday.Add(24 * time.Hour)
	// Created out of order, one without a publish date.
	createTestEpisode(t, store, feed.ID, "tuesday", &tuesday)
	createTestEpisode(t, store, feed.ID, "undated", nil)
	createTestEpisode(t, store, feed.ID, "monday", &monday)

	episodes, err := store.ListEpisodes(context.Background(), feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, episode := range episodes {
		order = append(order, episode.GUID)
	}
	want := []string{"monday", "tuesday", "undated"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestUpdateEpisode(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	feed := createTestFeed(t, store, "https://example.com/feed")
	episode := createTestEpisode(t, store, feed.ID, "guid-1", nil)

	cachedAt := time.Now().UTC().Truncate(time.Second)
	episode.State = models.EpisodeReady
	episode.CacheFile = "abc.mp3"
	episode.CacheSize = 1234
	episode.CachedAt = &cachedAt
	if err := store.UpdateEpisode(ctx, &episode); err != nil {
		t.Fatalf("UpdateEpisode: %v", err)
	}

	stored, err := store.GetEpisode(ctx, feed.ID, episode.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != models.EpisodeReady || stored.CacheFile != "abc.mp3" || stored.CacheSize != 1234 || !stored.CachedAt.Equal(cachedAt) {
		t.Errorf("stored = %+v", stored)
	}

	missing := models.Episode{Base: models.Base{ID: uuid.New()}, FeedID: feed.ID}
	if err := store.UpdateEpisode(ctx, &missing); !errors.Is(err, ErrEpisodeNotFound) {
		t.Errorf("unknown ID: err = %v, want ErrEpisodeNotFound", err)
	}
}
