package episodes

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"aunefyren/solstein/models"
)

// age backdates a file's modification time.
func age(t *testing.T, path string, by time.Duration) {
	t.Helper()
	old := time.Now().Add(-by)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

func TestSweepExpiresOldCacheEntries(t *testing.T) {
	setup := newTestSetup(t)
	ctx := context.Background()
	housekeeper := NewHousekeeper(setup.store, setup.cache, 14*24*time.Hour, 0, false, setup.clock.Now)

	oldEpisode := setup.addEpisode(t, "/ok.mp3?old")
	setup.processOne(t)
	setup.clock.advance(10 * 24 * time.Hour)
	newEpisode := setup.addEpisode(t, "/ok.mp3?new")
	setup.processOne(t)
	setup.clock.advance(5 * 24 * time.Hour) // old is now 15 days, new 5

	oldPath, _ := setup.cache.Path(setup.reload(t, oldEpisode).CacheFile)
	if err := housekeeper.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	expired := setup.reload(t, oldEpisode)
	if expired.CacheFile != "" || expired.CacheSize != 0 || expired.CachedAt != nil || expired.State != models.EpisodeReady {
		t.Errorf("expired episode = %+v, want uncached but still ready", expired)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Errorf("expired file still on disk: %v", err)
	}
	kept := setup.reload(t, newEpisode)
	if keptPath, _ := setup.cache.Path(kept.CacheFile); kept.CacheFile == "" || !fileExists(keptPath) {
		t.Errorf("recent episode lost its cache: %+v", kept)
	}
}

func TestSweepEnforcesSizeCap(t *testing.T) {
	setup := newTestSetup(t)
	ctx := context.Background()
	// Retention long enough that expire() never fires; only the size cap
	// should. Each /ok.mp3 download is len(audio) = 23 bytes; a cap of 50
	// fits the two newest (46) but not all three (69).
	housekeeper := NewHousekeeper(setup.store, setup.cache, 365*24*time.Hour, 50, false, setup.clock.Now)

	oldest := setup.addEpisode(t, "/ok.mp3?1")
	setup.processOne(t)
	setup.clock.advance(time.Hour)
	middle := setup.addEpisode(t, "/ok.mp3?2")
	setup.processOne(t)
	setup.clock.advance(time.Hour)
	newest := setup.addEpisode(t, "/ok.mp3?3")
	setup.processOne(t)

	if err := housekeeper.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if setup.reload(t, oldest).CacheFile != "" {
		t.Error("oldest cache entry wasn't evicted over the size cap")
	}
	if setup.reload(t, middle).CacheFile == "" || setup.reload(t, newest).CacheFile == "" {
		t.Error("entries under the cap were evicted")
	}
}

func TestSweepEvictsFullyServedEpisodes(t *testing.T) {
	setup := newTestSetup(t)
	ctx := context.Background()
	episode := setup.addEpisode(t, "/ok.mp3")
	setup.processOne(t)
	if err := setup.store.MarkFullyServed(ctx, episode.ID, setup.clock.Now()); err != nil {
		t.Fatal(err)
	}

	// Off by default: a fully served copy stays.
	housekeeper := NewHousekeeper(setup.store, setup.cache, 365*24*time.Hour, 0, false, setup.clock.Now)
	if err := housekeeper.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if setup.reload(t, episode).CacheFile == "" {
		t.Error("evicted although cache_evict_after_serve is off")
	}

	// On: it goes, and a fresh cache copy would start unserved again.
	housekeeper = NewHousekeeper(setup.store, setup.cache, 365*24*time.Hour, 0, true, setup.clock.Now)
	if err := housekeeper.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if stored := setup.reload(t, episode); stored.CacheFile != "" || stored.FullyServedAt != nil {
		t.Errorf("fully served episode wasn't evicted with cache_evict_after_serve on: %+v", stored)
	}
}

func TestSweepRemovesStrayFiles(t *testing.T) {
	setup := newTestSetup(t)
	ctx := context.Background()
	housekeeper := NewHousekeeper(setup.store, setup.cache, 14*24*time.Hour, 0, false, nil)

	episode := setup.addEpisode(t, "/ok.mp3")
	setup.processOne(t)
	knownPath, _ := setup.cache.Path(setup.reload(t, episode).CacheFile)
	age(t, knownPath, 48*time.Hour)

	// A deleted feed's leftovers, an abandoned download, and a fresh file
	// the database may not have recorded yet.
	deletedFeed := filepath.Join(setup.cache.directory, "00000000-0000-0000-0000-000000000009")
	os.MkdirAll(deletedFeed, 0o750)
	stray := filepath.Join(deletedFeed, "episode.mp3")
	part := filepath.Join(setup.cache.directory, setup.feed.ID.String(), "x.mp3.123.part")
	fresh := filepath.Join(setup.cache.directory, setup.feed.ID.String(), "fresh.mp3")
	for _, path := range []string{stray, part, fresh} {
		if err := os.WriteFile(path, []byte("data"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	age(t, stray, 48*time.Hour)
	age(t, part, 48*time.Hour)

	if err := housekeeper.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if fileExists(stray) || fileExists(part) {
		t.Error("stray files not removed")
	}
	if fileExists(deletedFeed) {
		t.Error("empty feed directory not removed")
	}
	if !fileExists(fresh) {
		t.Error("recent unrecorded file removed; it may belong to a download that just finished")
	}
	if !fileExists(knownPath) {
		t.Error("a file an episode refers to was removed")
	}
}

func TestRemoveFeed(t *testing.T) {
	setup := newTestSetup(t)
	server := newTestServer(t, setup)
	episode := setup.addEpisode(t, "/ok.mp3")
	setup.processOne(t)
	path, _ := setup.cache.Path(setup.reload(t, episode).CacheFile)

	if err := server.RemoveFeed(setup.feed.ID); err != nil {
		t.Fatal(err)
	}
	if fileExists(path) || fileExists(filepath.Dir(path)) {
		t.Error("feed cache not removed")
	}
	if err := server.RemoveFeed(setup.feed.ID); err != nil {
		t.Errorf("removing an already-removed feed: %v", err)
	}
}

func TestHousekeeperRunStops(t *testing.T) {
	setup := newTestSetup(t)
	housekeeper := NewHousekeeper(setup.store, setup.cache, time.Hour, 0, false, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		housekeeper.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestSweepErrors(t *testing.T) {
	ctx := context.Background()

	// Without the cache directory, stray files can't be looked for.
	setup := newTestSetup(t)
	housekeeper := NewHousekeeper(setup.store, setup.cache, time.Hour, 0, false, setup.clock.Now)
	if err := os.RemoveAll(setup.cache.directory); err != nil {
		t.Fatal(err)
	}
	if err := housekeeper.Sweep(ctx); err == nil {
		t.Error("Sweep without the cache directory: no error")
	}

	// A cached file that can't be deleted (here a non-empty directory) stays
	// for the next sweep.
	setup = newTestSetup(t)
	housekeeper = NewHousekeeper(setup.store, setup.cache, time.Hour, 0, false, setup.clock.Now)
	episode := setup.addEpisode(t, "/ok.mp3")
	setup.processOne(t)
	cached := setup.reload(t, episode)
	fullPath, _ := setup.cache.Path(cached.CacheFile)
	os.Remove(fullPath)
	os.MkdirAll(filepath.Join(fullPath, "inside"), 0o750)
	setup.clock.advance(2 * time.Hour)
	if err := housekeeper.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if setup.reload(t, episode).CacheFile == "" {
		t.Error("the entry of a file that couldn't be deleted was forgotten")
	}

	// A failing database fails the sweep, and Run logs it and carries on.
	setup.store.Close()
	if err := housekeeper.Sweep(ctx); err == nil {
		t.Error("Sweep with a closed store: no error")
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		housekeeper.Run(runCtx)
		close(done)
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done
}
