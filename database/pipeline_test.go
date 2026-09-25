package database

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"aunefyren/solstein/models"

	"github.com/google/uuid"
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
	// Backlog is fetched on demand (ready, uncached), unless queued for
	// processing up front (discovered).
	backlog := models.Episode{FeedID: feed.ID, GUID: "backlog", SourceURL: "x", State: models.EpisodeReady, Backlog: true}
	queued := models.Episode{FeedID: feed.ID, GUID: "queued", SourceURL: "x", State: models.EpisodeDiscovered, Backlog: true}
	for _, episode := range []*models.Episode{&backlog, &queued} {
		if err := store.CreateEpisode(ctx, episode); err != nil {
			t.Fatal(err)
		}
	}
	later := now.Add(time.Hour)
	retrying := models.Episode{FeedID: feed.ID, GUID: "retrying", SourceURL: "x", State: models.EpisodeDiscovered, NextAttemptAt: &later}
	if err := store.CreateEpisode(ctx, &retrying); err != nil {
		t.Fatal(err)
	}

	first, err := store.ClaimNextEpisode(ctx, now, "cache", nil)
	if err != nil || first.GUID != "monday" || first.State != models.EpisodeAcquiring {
		t.Fatalf("first claim = %+v, %v; want monday", first, err)
	}
	second, err := store.ClaimNextEpisode(ctx, now, "cache", nil)
	if err != nil || second.GUID != "tuesday" {
		t.Fatalf("second claim = %+v, %v; want tuesday", second, err)
	}
	if third, err := store.ClaimNextEpisode(ctx, now, "cache", nil); err != nil || third.GUID != "queued" {
		t.Fatalf("third claim = %+v, %v; want the queued backlog episode", third, err)
	}
	// The ready backlog episode waits for a request, and the retry isn't due yet.
	if _, err := store.ClaimNextEpisode(ctx, now, "cache", nil); !errors.Is(err, ErrNoWork) {
		t.Errorf("fourth claim: err = %v, want ErrNoWork", err)
	}
	if claimed, err := store.ClaimNextEpisode(ctx, later, "cache", nil); err != nil || claimed.GUID != "retrying" {
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
	claimed, err := store.ClaimNextEpisode(ctx, now, "stream", nil)
	if err != nil || claimed.GUID != "c" {
		t.Fatalf("claim = %+v, %v; want c", claimed, err)
	}
	if _, err := store.ClaimNextEpisode(ctx, now, "stream", nil); !errors.Is(err, ErrNoWork) {
		t.Errorf("err = %v, want ErrNoWork", err)
	}
	// Global default cache: the inheriting feed too, never the stream one.
	if claimed, err := store.ClaimNextEpisode(ctx, now, "cache", nil); err != nil || claimed.GUID != "a" {
		t.Errorf("claim = %+v, %v; want a", claimed, err)
	}
	if _, err := store.ClaimNextEpisode(ctx, now, "cache", nil); !errors.Is(err, ErrNoWork) {
		t.Errorf("err = %v, want ErrNoWork", err)
	}
	// A processed feed's episodes are claimed whatever its mode.
	if claimed, err := store.ClaimNextEpisode(ctx, now, "cache", []uuid.UUID{streams.ID}); err != nil || claimed.GUID != "b" {
		t.Errorf("claim = %+v, %v; want b from the processed stream feed", claimed, err)
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
				episode, err := store.ClaimNextEpisode(ctx, time.Now().UTC(), "cache", nil)
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

func TestClaimEpisode(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	feed := createTestFeed(t, store, "https://example.com/feed")
	later := now.Add(time.Hour)
	retrying := models.Episode{FeedID: feed.ID, GUID: "retrying", SourceURL: "x", State: models.EpisodeDiscovered, NextAttemptAt: &later}
	ready := models.Episode{FeedID: feed.ID, GUID: "ready", SourceURL: "x", State: models.EpisodeReady}
	for _, episode := range []*models.Episode{&retrying, &ready} {
		if err := store.CreateEpisode(ctx, episode); err != nil {
			t.Fatal(err)
		}
	}
	// A client asked: claimed even though its retry isn't due.
	claimed, err := store.ClaimEpisode(ctx, retrying.ID, now)
	if err != nil || claimed.State != models.EpisodeAcquiring || claimed.GUID != "retrying" {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}
	if _, err := store.ClaimEpisode(ctx, retrying.ID, now); !errors.Is(err, ErrNoWork) {
		t.Errorf("second claim: err = %v, want ErrNoWork", err)
	}
	if _, err := store.ClaimEpisode(ctx, ready.ID, now); !errors.Is(err, ErrNoWork) {
		t.Errorf("ready episode: err = %v, want ErrNoWork", err)
	}
}

func TestClaimQueuedEpisode(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	feed := createTestFeed(t, store, "https://example.com/feed")

	monday, tuesday, later := now.Add(-48*time.Hour), now.Add(-24*time.Hour), now.Add(time.Hour)
	add := func(guid string, state models.EpisodeState, published, next *time.Time, cacheFile string) models.Episode {
		episode := models.Episode{FeedID: feed.ID, GUID: guid, SourceURL: "x", State: state, PublishedAt: published, NextAttemptAt: next, CacheFile: cacheFile}
		if err := store.CreateEpisode(ctx, &episode); err != nil {
			t.Fatal(err)
		}
		return episode
	}
	add("waiting", models.EpisodeDiscovered, &monday, &now, "") // ClaimNextEpisode's
	add("not queued", models.EpisodeFailed, &monday, nil, "")
	add("cached", models.EpisodeReady, &monday, &now, "feed/cached.mp3")
	add("not due", models.EpisodeFailed, &monday, &later, "")
	add("old", models.EpisodeReady, &monday, &now, "")
	add("new", models.EpisodeReady, &tuesday, &now, "")
	add("failed", models.EpisodeFailed, &monday, &now, "")

	// Queued at the same time: newest first.
	lease := now.Add(3 * time.Hour)
	var order []string
	for {
		claimed, err := store.ClaimQueuedEpisode(ctx, now, lease, "cache", nil)
		if errors.Is(err, ErrNoWork) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if claimed.NextAttemptAt == nil || !claimed.NextAttemptAt.Equal(lease) {
			t.Errorf("%s: next attempt %v, want the lease", claimed.GUID, claimed.NextAttemptAt)
		}
		order = append(order, claimed.GUID)
	}
	// Same date: the one added last first.
	if !slices.Equal(order, []string{"new", "failed", "old"}) {
		t.Fatalf("claimed %v", order)
	}

	// Claimed episodes keep their state, and come due again after the lease.
	if claimed, err := store.ClaimQueuedEpisode(ctx, lease, lease.Add(time.Hour), "cache", nil); err != nil || claimed.State == models.EpisodeAcquiring {
		t.Errorf("after the lease: %+v, %v", claimed, err)
	}
}

func TestQueueEpisodes(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	feed := createTestFeed(t, store, "https://example.com/feed")
	var ids []uuid.UUID
	for _, episode := range []models.Episode{
		{GUID: "failed", State: models.EpisodeFailed, LateRetries: 8},
		{GUID: "uncached", State: models.EpisodeReady},
		{GUID: "cached", State: models.EpisodeReady, CacheFile: "feed/cached.mp3"},
		{GUID: "waiting", State: models.EpisodeDiscovered},
	} {
		episode.FeedID, episode.SourceURL = feed.ID, "x"
		if err := store.CreateEpisode(ctx, &episode); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, episode.ID)
	}

	if queued, err := store.QueueFailedEpisodes(ctx, ids, now); err != nil || queued != 1 {
		t.Errorf("failed: queued %d, %v", queued, err)
	}
	if queued, err := store.QueueUncachedEpisodes(ctx, ids, now); err != nil || queued != 1 {
		t.Errorf("uncached: queued %d, %v", queued, err)
	}
	for i, guid := range []string{"failed", "uncached", "cached", "waiting"} {
		stored, _ := store.GetEpisode(ctx, feed.ID, ids[i])
		queued := stored.NextAttemptAt != nil
		if queued != (i < 2) || stored.LateRetries != 0 {
			t.Errorf("%s: next attempt %v, late retries %d", guid, stored.NextAttemptAt, stored.LateRetries)
		}
	}
}
