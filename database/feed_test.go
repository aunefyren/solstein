package database

import (
	"context"
	"errors"
	"testing"
	"time"

	"aunefyren/solstein/models"

	"github.com/google/uuid"
)

func TestCreateFeedAssignsID(t *testing.T) {
	store := openTestStore(t)
	feed := createTestFeed(t, store, "https://example.com/feed")

	if feed.ID == uuid.Nil {
		t.Fatal("ID not assigned")
	}
	if feed.CreatedAt.IsZero() {
		t.Error("CreatedAt not set")
	}
}

func TestCreateFeedKeepsGivenID(t *testing.T) {
	store := openTestStore(t)
	id := uuid.New()
	feed := models.Feed{Base: models.Base{ID: id}, SourceURL: "https://example.com/feed"}
	if err := store.CreateFeed(context.Background(), &feed); err != nil {
		t.Fatal(err)
	}
	if feed.ID != id {
		t.Errorf("ID = %s, want the given %s", feed.ID, id)
	}
}

func TestCreateFeedDuplicate(t *testing.T) {
	store := openTestStore(t)
	createTestFeed(t, store, "https://example.com/feed")

	duplicate := models.Feed{SourceURL: "https://example.com/feed"}
	err := store.CreateFeed(context.Background(), &duplicate)
	if !errors.Is(err, ErrFeedExists) {
		t.Errorf("err = %v, want ErrFeedExists", err)
	}
}

func TestGetFeed(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	feed := createTestFeed(t, store, "https://example.com/feed")

	byID, err := store.GetFeed(ctx, feed.ID)
	if err != nil || byID.SourceURL != feed.SourceURL {
		t.Errorf("GetFeed = %+v, %v", byID, err)
	}
	byURL, err := store.GetFeedBySourceURL(ctx, feed.SourceURL)
	if err != nil || byURL.ID != feed.ID {
		t.Errorf("GetFeedBySourceURL = %+v, %v", byURL, err)
	}

	if _, err := store.GetFeed(ctx, uuid.New()); !errors.Is(err, ErrFeedNotFound) {
		t.Errorf("unknown ID: err = %v, want ErrFeedNotFound", err)
	}
	if _, err := store.GetFeedBySourceURL(ctx, "https://example.com/other"); !errors.Is(err, ErrFeedNotFound) {
		t.Errorf("unknown URL: err = %v, want ErrFeedNotFound", err)
	}
}

func TestListFeedsOldestFirst(t *testing.T) {
	store := openTestStore(t)
	first := createTestFeed(t, store, "https://example.com/1")
	second := createTestFeed(t, store, "https://example.com/2")

	feeds, err := store.ListFeeds(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(feeds) != 2 || feeds[0].ID != first.ID || feeds[1].ID != second.ID {
		t.Errorf("ListFeeds = %+v", feeds)
	}
}

func TestUpdateFeed(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	feed := createTestFeed(t, store, "https://example.com/feed")

	polledAt := time.Now().UTC().Truncate(time.Second)
	feed.Title = "Renamed"
	feed.LastPolledAt = &polledAt
	feed.Exit = "sweden"
	if err := store.UpdateFeed(ctx, &feed); err != nil {
		t.Fatalf("UpdateFeed: %v", err)
	}

	// Zero values must be saved too, so a per-feed override can be cleared.
	feed.Exit = ""
	if err := store.UpdateFeed(ctx, &feed); err != nil {
		t.Fatal(err)
	}

	stored, err := store.GetFeed(ctx, feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Title != "Renamed" || stored.Exit != "" || stored.LastPolledAt == nil || !stored.LastPolledAt.Equal(polledAt) {
		t.Errorf("stored = %+v", stored)
	}
	if !stored.CreatedAt.Equal(feed.CreatedAt) {
		t.Errorf("CreatedAt changed from %v to %v", feed.CreatedAt, stored.CreatedAt)
	}
}

func TestUpdateFeedNotFound(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	missing := models.Feed{Base: models.Base{ID: uuid.New()}, SourceURL: "https://example.com/feed"}
	if err := store.UpdateFeed(ctx, &missing); !errors.Is(err, ErrFeedNotFound) {
		t.Errorf("unknown ID: err = %v, want ErrFeedNotFound", err)
	}
	if err := store.UpdateFeed(ctx, &models.Feed{}); !errors.Is(err, ErrFeedNotFound) {
		t.Errorf("no ID: err = %v, want ErrFeedNotFound", err)
	}
	if feeds, _ := store.ListFeeds(ctx); len(feeds) != 0 {
		t.Errorf("UpdateFeed created a feed: %+v", feeds)
	}
}

func TestDeleteFeedCascadesToEpisodes(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	feed := createTestFeed(t, store, "https://example.com/feed")
	episode := createTestEpisode(t, store, feed.ID, "guid-1", nil)

	if err := store.DeleteFeed(ctx, feed.ID); err != nil {
		t.Fatalf("DeleteFeed: %v", err)
	}
	if _, err := store.GetEpisode(ctx, feed.ID, episode.ID); !errors.Is(err, ErrEpisodeNotFound) {
		t.Errorf("episode survived feed deletion: err = %v", err)
	}
	if err := store.DeleteFeed(ctx, feed.ID); !errors.Is(err, ErrFeedNotFound) {
		t.Errorf("second delete: err = %v, want ErrFeedNotFound", err)
	}

	// No soft delete: the same source URL can be added again.
	createTestFeed(t, store, "https://example.com/feed")
}
