package database

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"aunefyren/solstein/models"
)

func TestClaimNextEpisode(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	feed := createTestFeed(t, store, "https://example.com/feed")

	monday := now.Add(-48 * time.Hour)
	tuesday := now.Add(-24 * time.Hour)
	createTestEpisode(t, store, feed.ID, "tuesday", &tuesday)
	createTestEpisode(t, store, feed.ID, "monday", &monday)
	backlog := models.Episode{FeedID: feed.ID, GUID: "backlog", SourceURL: "x", State: models.EpisodeDiscovered, Backlog: true}
	if err := store.CreateEpisode(ctx, &backlog); err != nil {
		t.Fatal(err)
	}
	later := now.Add(time.Hour)
	retrying := models.Episode{FeedID: feed.ID, GUID: "retrying", SourceURL: "x", State: models.EpisodeDiscovered, NextAttemptAt: &later}
	if err := store.CreateEpisode(ctx, &retrying); err != nil {
		t.Fatal(err)
	}

	first, err := store.ClaimNextEpisode(ctx, now, "cache")
	if err != nil || first.GUID != "monday" || first.State != models.EpisodeAcquiring {
		t.Fatalf("first claim = %+v, %v; want monday", first, err)
	}
	second, err := store.ClaimNextEpisode(ctx, now, "cache")
	if err != nil || second.GUID != "tuesday" {
		t.Fatalf("second claim = %+v, %v; want tuesday", second, err)
	}
	// Backlog is fetched on demand, and the retry isn't due yet.
	if _, err := store.ClaimNextEpisode(ctx, now, "cache"); !errors.Is(err, ErrNoWork) {
		t.Errorf("third claim: err = %v, want ErrNoWork", err)
	}
	if claimed, err := store.ClaimNextEpisode(ctx, later, "cache"); err != nil || claimed.GUID != "retrying" {
		t.Errorf("claim after retry time = %+v, %v", claimed, err)
	}

	stored, _ := store.GetEpisode(ctx, feed.ID, first.ID)
	if stored.State != models.EpisodeAcquiring {
		t.Errorf("claimed episode state in database = %s", stored.State)
	}
}

func TestClaimNextEpisodeRespectsDeliveryMode(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	inherits := createTestFeed(t, store, "https://example.com/inherits")
	createTestEpisode(t, store, inherits.ID, "a", nil)
	streams := models.Feed{SourceURL: "https://example.com/streams", DeliveryMode: "stream"}
	if err := store.CreateFeed(ctx, &streams); err != nil {
		t.Fatal(err)
	}
	createTestEpisode(t, store, streams.ID, "b", nil)
	caches := models.Feed{SourceURL: "https://example.com/caches", DeliveryMode: "cache"}
	if err := store.CreateFeed(ctx, &caches); err != nil {
		t.Fatal(err)
	}
	createTestEpisode(t, store, caches.ID, "c", nil)

	// Global default stream: only the feed that sets cache itself.
	claimed, err := store.ClaimNextEpisode(ctx, now, "stream")
	if err != nil || claimed.GUID != "c" {
		t.Fatalf("claim = %+v, %v; want c", claimed, err)
	}
	if _, err := store.ClaimNextEpisode(ctx, now, "stream"); !errors.Is(err, ErrNoWork) {
		t.Errorf("err = %v, want ErrNoWork", err)
	}
	// Global default cache: the inheriting feed too, never the stream one.
	if claimed, err := store.ClaimNextEpisode(ctx, now, "cache"); err != nil || claimed.GUID != "a" {
		t.Errorf("claim = %+v, %v; want a", claimed, err)
	}
	if _, err := store.ClaimNextEpisode(ctx, now, "cache"); !errors.Is(err, ErrNoWork) {
		t.Errorf("err = %v, want ErrNoWork", err)
	}
}

func TestClaimNextEpisodeConcurrent(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	feed := createTestFeed(t, store, "https://example.com/feed")
	for _, guid := range []string{"1", "2", "3", "4", "5", "6"} {
		createTestEpisode(t, store, feed.ID, guid, nil)
	}

	var (
		mutex   sync.Mutex
		claimed = map[string]int{}
		wait    sync.WaitGroup
	)
	for range 4 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for {
				episode, err := store.ClaimNextEpisode(ctx, time.Now().UTC(), "cache")
				if errors.Is(err, ErrNoWork) {
					return
				}
				if err != nil {
					t.Error(err)
					return
				}
				mutex.Lock()
				claimed[episode.GUID]++
				mutex.Unlock()
			}
		}()
	}
	wait.Wait()

	if len(claimed) != 6 {
		t.Errorf("claimed %d distinct episodes, want 6: %v", len(claimed), claimed)
	}
	for guid, count := range claimed {
		if count != 1 {
			t.Errorf("episode %s claimed %d times", guid, count)
		}
	}
}

func TestResetInterruptedEpisodes(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	feed := createTestFeed(t, store, "https://example.com/feed")
	episode := createTestEpisode(t, store, feed.ID, "a", nil)
	episode.State = models.EpisodeAcquiring
	if err := store.UpdateEpisode(ctx, &episode); err != nil {
		t.Fatal(err)
	}

	count, err := store.ResetInterruptedEpisodes(ctx)
	if err != nil || count != 1 {
		t.Fatalf("ResetInterruptedEpisodes = %d, %v", count, err)
	}
	stored, _ := store.GetEpisode(ctx, feed.ID, episode.ID)
	if stored.State != models.EpisodeDiscovered {
		t.Errorf("state = %s, want discovered", stored.State)
	}
}
