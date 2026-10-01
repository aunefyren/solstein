package episodes

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"aunefyren/solstein/models"
)

func TestViewOf(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	earlier, later := now.Add(-time.Minute), now.Add(time.Hour)
	cases := []struct {
		name     string
		episode  models.Episode
		prepares bool
		working  bool
		want     Status
		retry    bool
		prepare  bool
	}{
		{"cleaned", models.Episode{State: models.EpisodeReady, CacheFile: "a.mp3", ProcessNote: "removed 1m of ads"}, true, false, StatusCleaned, false, false},
		{"cached", models.Episode{State: models.EpisodeReady, CacheFile: "a.mp3"}, true, false, StatusCached, false, false},
		{"backlog", models.Episode{State: models.EpisodeReady, Backlog: true}, true, false, StatusNotCached, false, true},
		{"backlog queued", models.Episode{State: models.EpisodeReady, NextAttemptAt: &earlier}, true, false, StatusQueued, false, false},
		{"streamed", models.Episode{State: models.EpisodeReady}, false, false, StatusPassedThrough, false, false},
		{"job running", models.Episode{State: models.EpisodeReady}, true, true, StatusWorking, false, false},
		{"acquiring", models.Episode{State: models.EpisodeAcquiring}, true, false, StatusWorking, false, false},
		{"new", models.Episode{State: models.EpisodeDiscovered}, true, false, StatusQueued, false, false},
		{"retry later", models.Episode{State: models.EpisodeDiscovered, FailedAttempts: 2, NextAttemptAt: &later}, true, false, StatusRetrying, false, false},
		{"withheld", models.Episode{State: models.EpisodeFailed, Withheld: true, LateRetries: 1, NextAttemptAt: &later}, true, false, StatusWithheld, true, false},
		{"withheld, retry due", models.Episode{State: models.EpisodeFailed, Withheld: true, NextAttemptAt: &earlier}, true, false, StatusQueued, false, false},
		{"given up", models.Episode{State: models.EpisodeFailed, Withheld: true, LateRetries: len(withheldRetryDelays)}, true, false, StatusGivenUp, true, false},
		{"published with ads", models.Episode{State: models.EpisodeFailed}, true, false, StatusPublishedWithAds, true, false},
		{"hidden", models.Episode{State: models.EpisodeDiscovered, Hidden: true}, true, false, StatusHidden, false, false},
		{"hidden backlog", models.Episode{State: models.EpisodeReady, Backlog: true, Hidden: true}, true, false, StatusHidden, false, false},
		{"published with ads, retrying now", models.Episode{State: models.EpisodeFailed}, true, true, StatusWorking, false, false},
	}
	for _, c := range cases {
		view := viewOf(c.episode, c.prepares, c.working, now)
		if view.Status != c.want || view.CanRetry != c.retry || view.CanPrepare != c.prepare {
			t.Errorf("%s: status %s, retry %v, prepare %v; want %s, %v, %v", c.name, view.Status, view.CanRetry, view.CanPrepare, c.want, c.retry, c.prepare)
		}
	}
	if view := viewOf(models.Episode{State: models.EpisodeDiscovered, FailedAttempts: 1, NextAttemptAt: &later}, true, false, now); !view.NextAttempt.Equal(later) {
		t.Errorf("next attempt %v, want %v", view.NextAttempt, later)
	}
}

func TestScrubURLs(t *testing.T) {
	for in, want := range map[string]string{
		`Get "https://feeds.example.com/private/feed?token=s3cret": EOF`: `Get "https://feeds.example.com/…": EOF`,
		"source answered 404 Not Found":                                  "source answered 404 Not Found",
		"download from http://cdn.example:8080/a.mp3 broke":              "download from http://cdn.example:8080/… broke",
		"see https://example.com":                                        "see https://example.com",
	} {
		if got := scrubURLs(in); got != want {
			t.Errorf("scrubURLs(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestQueueEpisode(t *testing.T) {
	setup := newTestSetup(t)
	processor := &fakeProcessor{err: fmt.Errorf("%w: not MP3", ErrPermanent)}
	setup.withProcessor(t, processor)
	failed := setup.addEpisode(t, "/ok.mp3")
	setup.processOne(t)
	backlog := setup.addBacklog(t, "/ok.mp3?backlog")
	ctx := context.Background()

	views, err := setup.pipeline.Episodes(ctx, setup.feed.ID)
	if err != nil || len(views) != 2 {
		t.Fatalf("views %+v, %v", views, err)
	}
	statuses := map[string]EpisodeView{}
	for _, view := range views {
		statuses[view.ID.String()] = view
	}
	if view := statuses[failed.ID.String()]; view.Status != StatusPublishedWithAds || !view.CanRetry || view.Error == "" {
		t.Errorf("failed episode: %+v", view)
	}
	if view := statuses[backlog.ID.String()]; view.Status != StatusNotCached || !view.CanPrepare {
		t.Errorf("backlog episode: %+v", view)
	}

	// A retry and a prepare, each queued once.
	for _, episode := range []models.Episode{failed, backlog} {
		if queued, err := setup.pipeline.QueueEpisode(ctx, setup.feed.ID, episode.ID); err != nil || !queued {
			t.Errorf("queue %s: %v, %v", episode.Title, queued, err)
		}
		if view := setup.viewOfEpisode(t, episode); view.Status != StatusQueued || view.CanRetry || view.CanPrepare {
			t.Errorf("after queueing: %+v", view)
		}
	}
	// Fixed, the retry goes through.
	processor.setErr(nil)
	setup.processOne(t)
	setup.processOne(t)
	if view := setup.viewOfEpisode(t, failed); view.Status != StatusCleaned {
		t.Errorf("retried episode: %s", view.Status)
	}
	// One with its file has nothing to queue.
	if queued, err := setup.pipeline.QueueEpisode(ctx, setup.feed.ID, failed.ID); err != nil || queued {
		t.Errorf("queue a cleaned episode: %v, %v", queued, err)
	}

	setup.feed.DeliveryMode = "original"
	setup.store.UpdateFeed(ctx, &setup.feed)
	setup.withProcessor(t, nil)
	if _, err := setup.pipeline.QueueEpisode(ctx, setup.feed.ID, backlog.ID); !errors.Is(err, ErrNotPrepared) {
		t.Errorf("queue in original mode: %v", err)
	}
}

func (setup *testSetup) viewOfEpisode(t *testing.T, episode models.Episode) EpisodeView {
	t.Helper()
	views, err := setup.pipeline.Episodes(context.Background(), setup.feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range views {
		if view.ID == episode.ID {
			return view
		}
	}
	t.Fatalf("episode %s not listed", episode.ID)
	return EpisodeView{}
}
