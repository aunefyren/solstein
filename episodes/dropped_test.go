package episodes

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"aunefyren/solstein/database"
	"aunefyren/solstein/feeds"
	"aunefyren/solstein/models"
	"aunefyren/solstein/outbound"
)

// drop has a poll of the setup's feed list only a new episode, so every
// episode stored so far is marked dropped now.
func (setup *testSetup) drop(t *testing.T) {
	t.Helper()
	listed := []models.Episode{{GUID: "still-listed", SourceURL: setup.host.URL + "/ok.mp3?listed", State: models.EpisodeReady}}
	if _, err := setup.store.SyncEpisodes(context.Background(), setup.feed.ID, listed, database.Hiding{Now: setup.clock.Now()}); err != nil {
		t.Fatal(err)
	}
}

func (setup *testSetup) feedService(t *testing.T, options feeds.Options) *feeds.Service {
	t.Helper()
	exits, err := outbound.New(outbound.Options{AllowPrivateDestinations: true})
	if err != nil {
		t.Fatal(err)
	}
	options.DefaultDeliveryMode = "cache"
	return feeds.New(setup.store, exits, options)
}

func TestSweepKeepsServedDroppedFiles(t *testing.T) {
	setup := newTestSetup(t)
	ctx := context.Background()
	episode := setup.addEpisode(t, "/ok.mp3?kept")
	setup.processOne(t)
	setup.drop(t)
	path, _ := setup.cache.Path(setup.reload(t, episode).CacheFile)
	setup.clock.advance(30 * 24 * time.Hour)

	// Served: its file outlives the retention, the size cap and having been
	// downloaded in full.
	if err := setup.store.MarkFullyServed(ctx, episode.ID, setup.clock.Now()); err != nil {
		t.Fatal(err)
	}
	served := setup.feedService(t, feeds.Options{ServeDropped: true})
	housekeeper := NewHousekeeper(setup.store, setup.cache, 24*time.Hour, 1, true, setup.clock.Now).WithDropped(served, setup.pipeline)
	if err := housekeeper.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if setup.reload(t, episode).CacheFile == "" || !fileExists(path) {
		t.Fatal("a served dropped episode lost its cached file")
	}

	// Not served: it expires like any other.
	notServed := setup.feedService(t, feeds.Options{})
	housekeeper = NewHousekeeper(setup.store, setup.cache, 24*time.Hour, 0, false, setup.clock.Now).WithDropped(notServed, setup.pipeline)
	if err := housekeeper.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if setup.reload(t, episode).CacheFile != "" || fileExists(path) {
		t.Error("a dropped episode that isn't served kept its cached file past the retention")
	}
}

func TestSweepDeletesDroppedEpisodes(t *testing.T) {
	setup := newTestSetup(t)
	ctx := context.Background()
	gone := setup.addEpisode(t, "/ok.mp3?gone")
	setup.processOne(t)
	keep := setup.addEpisode(t, "/ok.mp3?keep")
	setup.processOne(t)
	setup.drop(t)
	if err := setup.store.SetEpisodeServe(ctx, setup.feed.ID, keep.ID, "on", ""); err != nil {
		t.Fatal(err)
	}
	path, _ := setup.cache.Path(setup.reload(t, gone).CacheFile)
	housekeeper := NewHousekeeper(setup.store, setup.cache, 365*24*time.Hour, 0, false, setup.clock.Now).
		WithDropped(setup.feedService(t, feeds.Options{DeleteDropped: true}), setup.pipeline)

	setup.clock.advance(time.Hour)
	if err := housekeeper.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.store.GetEpisode(ctx, setup.feed.ID, gone.ID); err != nil {
		t.Fatal("a dropped episode was deleted before its grace was over")
	}

	setup.clock.advance(feeds.DroppedGrace)
	if err := housekeeper.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.store.GetEpisode(ctx, setup.feed.ID, gone.ID); !errors.Is(err, database.ErrEpisodeNotFound) {
		t.Errorf("dropped episode not deleted: %v", err)
	}
	if fileExists(path) {
		t.Error("the deleted episode's cached file is still there")
	}
	if setup.reload(t, keep).CacheFile == "" {
		t.Error("a dropped episode served by its own setting was deleted")
	}
}

func TestSweepChecksDroppedLinks(t *testing.T) {
	setup := newTestSetup(t)
	ctx := context.Background()
	missing := setup.addBacklog(t, "/missing.mp3")
	fine := setup.addBacklog(t, "/ok.mp3")
	setup.drop(t)
	housekeeper := NewHousekeeper(setup.store, setup.cache, 365*24*time.Hour, 0, false, setup.clock.Now).
		WithDropped(setup.feedService(t, feeds.Options{ServeDropped: true}), setup.pipeline)
	if err := housekeeper.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	gone := setup.reload(t, missing)
	if gone.Serve != "off" || !strings.Contains(gone.ServeWarning, "404 Not Found") {
		t.Errorf("gone episode: serve %q, warning %q; want off with a warning naming the 404", gone.Serve, gone.ServeWarning)
	}
	if still := setup.reload(t, fine); still.Serve != "" || still.ServeWarning != "" {
		t.Errorf("an episode whose audio is there was switched off: %+v", still)
	}
}

func TestServeStopsServingDroppedEpisodeWhoseAudioIsGone(t *testing.T) {
	setup := newTestSetup(t)
	server := newTestServer(t, setup)
	episode := setup.addBacklog(t, "/missing.mp3")
	if _, err := serve(t, server, episode, http.MethodGet, ""); !errors.Is(err, ErrSourceFailed) {
		t.Fatalf("err = %v, want ErrSourceFailed", err)
	}
	if stored := setup.reload(t, episode); stored.Serve != "" {
		t.Error("an episode still in the source was switched off")
	}

	setup.drop(t)
	if _, err := serve(t, server, setup.reload(t, episode), http.MethodGet, ""); !errors.Is(err, ErrSourceFailed) {
		t.Fatalf("err = %v, want ErrSourceFailed", err)
	}
	if stored := setup.reload(t, episode); stored.Serve != "off" || stored.ServeWarning == "" {
		t.Errorf("serve %q, warning %q; want off with a warning", stored.Serve, stored.ServeWarning)
	}
}

func TestDeleteEpisode(t *testing.T) {
	setup := newTestSetup(t)
	server := newTestServer(t, setup)
	ctx := context.Background()
	episode := setup.addEpisode(t, "/ok.mp3")
	setup.processOne(t)
	if _, err := server.DeleteEpisode(ctx, setup.feed.ID, episode.ID); !errors.Is(err, ErrNotDropped) {
		t.Fatalf("err = %v, want ErrNotDropped for an episode still in the source", err)
	}

	setup.drop(t)
	path, _ := setup.cache.Path(setup.reload(t, episode).CacheFile)
	if _, err := server.DeleteEpisode(ctx, setup.feed.ID, episode.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the cached file is still there")
	}
	if _, err := server.DeleteEpisode(ctx, setup.feed.ID, episode.ID); !errors.Is(err, database.ErrEpisodeNotFound) {
		t.Errorf("err = %v, want ErrEpisodeNotFound once deleted", err)
	}
}

func TestSetServe(t *testing.T) {
	setup := newTestSetup(t)
	server := newTestServer(t, setup)
	ctx := context.Background()
	episode := setup.addBacklog(t, "/ok.mp3")
	if err := setup.store.SetEpisodeServe(ctx, setup.feed.ID, episode.ID, "off", "gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := server.SetServe(ctx, setup.feed.ID, episode.ID, "maybe"); !errors.Is(err, feeds.ErrInvalidSettings) {
		t.Errorf("err = %v, want ErrInvalidSettings", err)
	}
	if _, err := server.SetServe(ctx, setup.feed.ID, episode.ID, "on"); err != nil {
		t.Fatal(err)
	}
	if stored := setup.reload(t, episode); stored.Serve != "on" || stored.ServeWarning != "" {
		t.Errorf("serve %q, warning %q; want on, warning cleared", stored.Serve, stored.ServeWarning)
	}

	// A save from a copy loaded earlier doesn't undo it.
	stale := episode
	stale.LastError = "something"
	if err := setup.store.UpdateEpisode(ctx, &stale); err != nil {
		t.Fatal(err)
	}
	if stored := setup.reload(t, episode); stored.Serve != "on" {
		t.Error("UpdateEpisode overwrote the serve setting")
	}
}

func TestDroppedEpisodesArentPreparedInTheBackground(t *testing.T) {
	setup := newTestSetup(t)
	episode := setup.addEpisode(t, "/ok.mp3")
	setup.drop(t)
	for setup.processOne(t) {
	}
	if stored := setup.reload(t, episode); stored.State != models.EpisodeDiscovered || stored.CacheFile != "" {
		t.Errorf("a dropped episode was prepared in the background: %+v", stored)
	}
}
