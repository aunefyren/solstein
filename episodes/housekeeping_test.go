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
	housekeeper := NewHousekeeper(setup.store, setup.cache, 14*24*time.Hour, setup.clock.Now)

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

func TestSweepRemovesStrayFiles(t *testing.T) {
	setup := newTestSetup(t)
	ctx := context.Background()
	housekeeper := NewHousekeeper(setup.store, setup.cache, 14*24*time.Hour, nil)

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
	housekeeper := NewHousekeeper(setup.store, setup.cache, time.Hour, nil)
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
