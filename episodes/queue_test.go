package episodes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"aunefyren/solstein/database"
	"aunefyren/solstein/models"
)

func TestWithheldEpisodeIsRetriedSlowly(t *testing.T) {
	setup := newTestSetup(t)
	processor := &fakeProcessor{err: fmt.Errorf("%w: not MP3", ErrPermanent), hide: true}
	server := setup.processedServer(t, processor, 5*time.Second)
	episode := setup.addEpisode(t, "/ok.mp3")
	setup.processOne(t)
	stored := setup.reload(t, episode)
	if stored.State != models.EpisodeFailed || !stored.Withheld || stored.NextAttemptAt == nil || !stored.NextAttemptAt.Equal(setup.clock.Now().Add(withheldRetryDelays[0])) {
		t.Fatalf("withheld episode = %+v, want its first slow retry scheduled", stored)
	}
	if setup.processOne(t) {
		t.Fatal("retried before it was due")
	}

	// Fails again: still withheld, and the next retry is further off.
	setup.clock.advance(withheldRetryDelays[0])
	if !setup.processOne(t) {
		t.Fatal("the due retry wasn't claimed")
	}
	stored = setup.reload(t, episode)
	if stored.State != models.EpisodeFailed || !stored.Withheld || stored.LateRetries != 1 || !stored.NextAttemptAt.Equal(setup.clock.Now().Add(withheldRetryDelays[1])) {
		t.Fatalf("after a failed retry: %+v", stored)
	}
	if _, err := serve(t, server, stored, http.MethodGet, ""); !errors.Is(err, database.ErrEpisodeNotFound) {
		t.Errorf("served while withheld: %v", err)
	}

	// The problem has passed: the next retry publishes it.
	processor.setErr(nil)
	setup.clock.advance(withheldRetryDelays[1])
	setup.processOne(t)
	stored = setup.reload(t, episode)
	if stored.State != models.EpisodeReady || stored.Withheld || stored.CacheFile == "" || stored.NextAttemptAt != nil || stored.LateRetries != 0 || stored.FailedAttempts != 0 {
		t.Fatalf("after a good retry: %+v", stored)
	}
	if recorder, err := serve(t, server, stored, http.MethodGet, ""); err != nil || recorder.Body.String() != strings.ToUpper(audio) {
		t.Errorf("got %d %q, %v", recorder.Code, recorder.Body.String(), err)
	}
}

func TestWithheldRetriesEnd(t *testing.T) {
	setup := newTestSetup(t)
	processor := &fakeProcessor{err: errors.New("the diff result is implausible"), hide: true}
	server := setup.processedServer(t, processor, 5*time.Second)
	backlog := setup.addBacklog(t, "/ok.mp3")
	for range maxRequestFailures {
		serve(t, server, backlog, http.MethodGet, "")
	}
	if stored := setup.reload(t, backlog); !stored.Withheld || stored.NextAttemptAt == nil {
		t.Fatalf("backlog episode = %+v, want withheld with a retry", stored)
	}

	for retry := range withheldRetryDelays {
		setup.clock.advance(withheldRetryDelays[retry])
		if !setup.processOne(t) {
			t.Fatalf("retry %d wasn't claimed", retry+1)
		}
	}
	stored := setup.reload(t, backlog)
	if stored.State != models.EpisodeFailed || !stored.Withheld || stored.NextAttemptAt != nil || stored.LateRetries != len(withheldRetryDelays) {
		t.Fatalf("after the last retry: %+v", stored)
	}
	setup.clock.advance(30 * 24 * time.Hour)
	if setup.processOne(t) {
		t.Error("retried after the last slow retry")
	}
	// Every retry asked for fresh copies.
	processor.mutex.Lock()
	defer processor.mutex.Unlock()
	for i, job := range processor.jobs {
		if job.Fresh != (i > 0) {
			t.Errorf("job %d: fresh = %v", i, job.Fresh)
		}
	}
}

func TestRetryFailed(t *testing.T) {
	setup := newTestSetup(t)
	processor := &fakeProcessor{err: fmt.Errorf("%w: not MP3", ErrPermanent)}
	setup.withProcessor(t, processor)
	published := setup.addEpisode(t, "/ok.mp3")
	setup.processOne(t)
	processor.mutex.Lock()
	processor.hide = true
	processor.mutex.Unlock()
	withheld := setup.addEpisode(t, "/ok.mp3?2")
	setup.processOne(t)
	// Both failed; the published one has no retry of its own, and the
	// withheld one has used its slow retries up.
	stored := setup.reload(t, withheld)
	stored.NextAttemptAt, stored.LateRetries = nil, len(withheldRetryDelays)
	setup.store.UpdateEpisode(context.Background(), &stored)
	if stored := setup.reload(t, published); stored.State != models.EpisodeFailed || stored.Withheld || stored.NextAttemptAt != nil {
		t.Fatalf("published unprocessed: %+v", stored)
	}

	queued, err := setup.pipeline.RetryFailed(context.Background(), setup.feed.ID)
	if err != nil || queued != 2 {
		t.Fatalf("queued %d, %v", queued, err)
	}

	// Still failing: the published one stays as it is, the withheld one gets
	// its slow retries afresh.
	setup.processOne(t)
	setup.processOne(t)
	if stored := setup.reload(t, published); stored.State != models.EpisodeFailed || stored.NextAttemptAt != nil || stored.LateRetries != 1 {
		t.Errorf("published unprocessed after retry: %+v", stored)
	}
	if stored := setup.reload(t, withheld); stored.State != models.EpisodeFailed || !stored.Withheld || stored.LateRetries != 1 || stored.NextAttemptAt == nil {
		t.Errorf("withheld after retry: %+v", stored)
	}

	// Fixed: another retry processes both.
	processor.setErr(nil)
	if queued, err := setup.pipeline.RetryFailed(context.Background(), setup.feed.ID); err != nil || queued != 2 {
		t.Fatalf("queued %d, %v", queued, err)
	}
	setup.processOne(t)
	setup.processOne(t)
	for _, episode := range []models.Episode{published, withheld} {
		if stored := setup.reload(t, episode); stored.State != models.EpisodeReady || stored.Withheld || stored.CacheFile == "" {
			t.Errorf("%s: %+v", episode.GUID, stored)
		}
	}
}

func TestQueuePreparesAhead(t *testing.T) {
	setup := newTestSetup(t)
	processor := &fakeProcessor{}
	setup.withProcessor(t, processor)
	var backlog []models.Episode
	for day := 1; day <= 3; day++ {
		episode := setup.addBacklog(t, fmt.Sprintf("/ok.mp3?%d", day))
		published := time.Date(2026, 9, day, 12, 0, 0, 0, time.UTC)
		episode.PublishedAt = &published
		setup.store.UpdateEpisode(context.Background(), &episode)
		backlog = append(backlog, episode)
	}

	queued, err := setup.pipeline.Queue(context.Background(), setup.feed.ID, 2)
	if err != nil || queued != 2 {
		t.Fatalf("queued %d, %v", queued, err)
	}
	for setup.processOne(t) {
	}
	processor.mutex.Lock()
	if len(processor.jobs) != 2 || processor.jobs[0].Episode.ID != backlog[2].ID || processor.jobs[1].Episode.ID != backlog[1].ID {
		t.Errorf("jobs %+v, want the two newest, newest first", processor.jobs)
	}
	processor.mutex.Unlock()
	for i, episode := range backlog {
		stored := setup.reload(t, episode)
		if (stored.CacheFile != "") != (i > 0) || stored.State != models.EpisodeReady || stored.NextAttemptAt != nil {
			t.Errorf("episode %d: %+v", i, stored)
		}
	}

	// Only episodes without their file are queued.
	if queued, err := setup.pipeline.Queue(context.Background(), setup.feed.ID, 0); err != nil || queued != 1 {
		t.Errorf("all: queued %d, %v; want the one left", queued, err)
	}

	// A feed whose episodes aren't prepared has nothing to queue.
	setup.feed.Title = "Streamed"
	setup.store.UpdateFeed(context.Background(), &setup.feed)
	if _, err := setup.pipeline.Queue(context.Background(), setup.feed.ID, 0); !errors.Is(err, ErrNotPrepared) {
		t.Errorf("stream-mode feed: err = %v", err)
	}
}

func TestQueuedEpisodeFailingWaitsForRequest(t *testing.T) {
	setup := newTestSetup(t)
	processor := &fakeProcessor{err: errors.New("tunnel down")}
	setup.withProcessor(t, processor)
	backlog := setup.addBacklog(t, "/ok.mp3")
	setup.pipeline.Queue(context.Background(), setup.feed.ID, 0)
	setup.processOne(t)
	if stored := setup.reload(t, backlog); stored.State != models.EpisodeReady || stored.NextAttemptAt != nil || stored.FailedAttempts != 1 {
		t.Errorf("episode = %+v, want published, no longer queued, one failure on record", stored)
	}
	if setup.processOne(t) {
		t.Error("claimed again")
	}
}

func TestReconcileSchedulesEarlierWithheldEpisodes(t *testing.T) {
	setup := newTestSetup(t)
	setup.withProcessor(t, &fakeProcessor{hide: true})
	episode := setup.addEpisode(t, "/ok.mp3")
	// Withheld before slow retries existed.
	episode.State, episode.Withheld, episode.PreparedWith = models.EpisodeFailed, true, setup.pipeline.failedRecipe(setup.feed)
	setup.store.UpdateEpisode(context.Background(), &episode)

	setup.reconcile(t)
	if stored := setup.reload(t, episode); stored.NextAttemptAt == nil || stored.State != models.EpisodeFailed {
		t.Fatalf("episode = %+v, want a retry scheduled", stored)
	}
	setup.processOne(t)
	if stored := setup.reload(t, episode); stored.State != models.EpisodeReady || stored.Withheld {
		t.Errorf("episode = %+v after the retry", stored)
	}
}
