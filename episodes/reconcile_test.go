package episodes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"aunefyren/solstein/database"
	"aunefyren/solstein/models"

	"github.com/google/uuid"
)

func (setup *testSetup) reconcile(t *testing.T) {
	t.Helper()
	if err := setup.pipeline.Reconcile(context.Background(), uuid.Nil); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func (setup *testSetup) cachedFileExists(t *testing.T, episode models.Episode) bool {
	t.Helper()
	path, err := setup.cache.Path(episode.CacheFile)
	if err != nil {
		t.Fatal(err)
	}
	_, err = os.Stat(path)
	return err == nil
}

func TestReconcileClearsDownloadsAfterModeChange(t *testing.T) {
	setup := newTestSetup(t)
	episode := setup.addEpisode(t, "/ok.mp3")
	setup.processOne(t)
	cached := setup.reload(t, episode)
	if cached.PreparedWith != "download through direct" {
		t.Fatalf("prepared with %q", cached.PreparedWith)
	}

	// Nothing changed: nothing happens.
	setup.reconcile(t)
	if stored := setup.reload(t, episode); stored.CacheFile == "" {
		t.Fatal("cleared without a settings change")
	}

	// Switched to stream mode: the cached copy goes.
	setup.feed.DeliveryMode = "stream"
	setup.store.UpdateFeed(context.Background(), &setup.feed)
	setup.reconcile(t)
	stored := setup.reload(t, episode)
	if stored.CacheFile != "" || stored.State != models.EpisodeReady || setup.cachedFileExists(t, cached) {
		t.Errorf("episode = %+v, file still there: %v", stored, setup.cachedFileExists(t, cached))
	}
}

func TestReconcileReprocessesAfterProcessorSettingsChange(t *testing.T) {
	setup := newTestSetup(t)
	processor := &fakeProcessor{recipe: "quiet"}
	server := setup.processedServer(t, processor, 5*time.Second)
	backlog := setup.addBacklog(t, "/ok.mp3")
	if _, err := serve(t, server, backlog, http.MethodGet, ""); err != nil {
		t.Fatal(err)
	}
	first := setup.reload(t, backlog)
	if first.PreparedWith != "fake: shout quiet" || first.CacheFile == "" {
		t.Fatalf("episode = %+v", first)
	}

	// The processor's settings change (say, trim_break_markers switched on
	// in config.json): the next start clears the old file ...
	processor.mutex.Lock()
	processor.recipe = "loud"
	processor.mutex.Unlock()
	setup.reconcile(t)
	if stored := setup.reload(t, backlog); stored.CacheFile != "" || stored.ProcessNote != "" || setup.cachedFileExists(t, first) {
		t.Fatalf("episode = %+v", stored)
	}
	// ... and the next play processes it again.
	if _, err := serve(t, server, backlog, http.MethodGet, ""); err != nil {
		t.Fatal(err)
	}
	if stored := setup.reload(t, backlog); stored.PreparedWith != "fake: shout loud" || processor.jobCount() != 2 {
		t.Errorf("episode = %+v after %d jobs", stored, processor.jobCount())
	}
}

func TestServeChecksSettingsOfCachedFile(t *testing.T) {
	// A file recorded with old settings (a preparation that was running
	// during the change) isn't served: it is processed again.
	setup := newTestSetup(t)
	processor := &fakeProcessor{recipe: "loud"}
	server := setup.processedServer(t, processor, 5*time.Second)
	backlog := setup.addBacklog(t, "/ok.mp3")
	if _, err := serve(t, server, backlog, http.MethodGet, ""); err != nil {
		t.Fatal(err)
	}
	stale := setup.reload(t, backlog)
	stale.PreparedWith = "fake: shout quiet"
	setup.store.UpdateEpisode(context.Background(), &stale)

	if recorder, err := serve(t, server, stale, http.MethodGet, ""); err != nil || recorder.Body.String() != strings.ToUpper(audio) {
		t.Fatalf("got %q, %v", recorder.Body.String(), err)
	}
	if processor.jobCount() != 2 {
		t.Errorf("%d jobs, want the stale file processed again", processor.jobCount())
	}
}

func TestReconcileEpisodesFromBeforeSettingsWereRecorded(t *testing.T) {
	setup := newTestSetup(t)
	processor := &fakeProcessor{}
	setup.withProcessor(t, processor)
	addCached := func(guid, note string) models.Episode {
		t.Helper()
		episode := models.Episode{FeedID: setup.feed.ID, GUID: guid, SourceURL: setup.host.URL + "/ok.mp3", State: models.EpisodeReady, Backlog: true, ProcessNote: note}
		if err := setup.store.CreateEpisode(context.Background(), &episode); err != nil {
			t.Fatal(err)
		}
		file, commit, _, _ := setup.cache.create(setup.feed.ID, episode.ID, "mp3")
		file.Write([]byte(audio))
		episode.CacheFile, _ = commit()
		episode.CacheSize = int64(len(audio))
		setup.store.UpdateEpisode(context.Background(), &episode)
		return episode
	}
	// Cached before region diff was switched on for the feed: not processed.
	plain := addCached("plain", "")
	// Processed, with settings nobody recorded: taken to be the current ones.
	processed := addCached("processed", "removed 1m of ads")

	setup.reconcile(t)
	if stored := setup.reload(t, plain); stored.CacheFile != "" {
		t.Errorf("unprocessed file kept for a processed feed: %+v", stored)
	}
	if stored := setup.reload(t, processed); stored.CacheFile == "" || stored.PreparedWith != "fake: shout " {
		t.Errorf("processed file: %+v", stored)
	}
}

func TestReconcileRetriesFailuresAfterChanges(t *testing.T) {
	setup := newTestSetup(t)
	processor := &fakeProcessor{err: fmt.Errorf("%w: not MP3", ErrPermanent), hide: true}
	server := setup.processedServer(t, processor, 5*time.Second)
	episode := setup.addEpisode(t, "/ok.mp3")
	setup.processOne(t)
	backlog := setup.addBacklog(t, "/ok.mp3")
	if _, err := serve(t, server, backlog, http.MethodGet, ""); !errors.Is(err, database.ErrEpisodeNotFound) {
		t.Fatalf("withheld backlog: err = %v", err)
	}
	for _, e := range []models.Episode{episode, backlog} {
		if stored := setup.reload(t, e); stored.State != models.EpisodeFailed || !stored.Withheld || !strings.HasSuffix(stored.PreparedWith, "failed, withheld") {
			t.Fatalf("%s: %+v", e.GUID, stored)
		}
	}

	// The same settings again: nothing happens.
	setup.reconcile(t)
	if stored := setup.reload(t, episode); stored.State != models.EpisodeFailed {
		t.Fatal("reset without a change")
	}

	// The failure policy changes to publish: both get a fresh start.
	processor.mutex.Lock()
	processor.hide, processor.err = false, nil
	processor.mutex.Unlock()
	setup.reconcile(t)
	if stored := setup.reload(t, episode); stored.State != models.EpisodeDiscovered || stored.Withheld || stored.Attempts != 0 || stored.LastError != "" {
		t.Errorf("new episode: %+v, want waiting for the pipeline again", stored)
	}
	if stored := setup.reload(t, backlog); stored.State != models.EpisodeReady || stored.Withheld {
		t.Errorf("backlog: %+v, want published and processed on request", stored)
	}
	setup.processOne(t)
	if stored := setup.reload(t, episode); stored.State != models.EpisodeReady || stored.CacheFile == "" {
		t.Errorf("after the retry: %+v", stored)
	}
}
