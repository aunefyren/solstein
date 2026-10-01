package feeds

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"aunefyren/solstein/models"
	"aunefyren/solstein/rss"
)

// subscribeAndDrop subscribes to a feed of ep-2 and ep-1, then has the source
// drop ep-1 and polls it.
func subscribeAndDrop(t *testing.T, service *Service, host *fakeHost) models.Feed {
	t.Helper()
	ctx := context.Background()
	host.addItem("ep-2", "Tue, 22 Sep 2026 06:00:00 +0000")
	feed, _, err := service.Subscribe(ctx, host.server.URL, Settings{})
	if err != nil {
		t.Fatal(err)
	}
	host.set(func(host *fakeHost) { host.items = host.items[:1] })
	if _, err := service.Refresh(ctx, &feed); err != nil {
		t.Fatal(err)
	}
	return feed
}

func renderedGUIDs(t *testing.T, service *Service, feed models.Feed) ([]string, string) {
	t.Helper()
	output, err := service.Render(context.Background(), feed, testURLs)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := rss.Parse(output)
	if err != nil {
		t.Fatalf("rendered feed doesn't parse: %v\n%s", err, output)
	}
	var guids []string
	for _, item := range parsed.Items {
		guids = append(guids, item.GUID)
	}
	return guids, string(output)
}

func TestRefreshMarksDroppedEpisodes(t *testing.T) {
	host := newFakeHost(t)
	service, store := newTestService(t, Options{})
	ctx := context.Background()
	feed := subscribeAndDrop(t, service, host)

	episodes, _ := store.ListEpisodes(ctx, feed.ID)
	if episodes[0].GUID != "ep-1" || episodes[0].DroppedAt == nil || episodes[1].DroppedAt != nil {
		t.Fatalf("ep-1 should be dropped, ep-2 not: %+v", episodes)
	}
	if !strings.Contains(string(episodes[1].SourceItem), "<guid>ep-2</guid>") {
		t.Errorf("ep-2's source item not kept: %q", episodes[1].SourceItem)
	}

	// A feed briefly listing nothing drops nothing more.
	host.set(func(host *fakeHost) { host.items = nil })
	if _, err := service.Refresh(ctx, &feed); err != nil {
		t.Fatal(err)
	}
	episodes, _ = store.ListEpisodes(ctx, feed.ID)
	if episodes[1].DroppedAt != nil {
		t.Error("an empty poll marked ep-2 dropped")
	}

	// Listed again, ep-1 isn't dropped any more.
	host.addItem("ep-1", "Mon, 21 Sep 2026 06:00:00 +0000")
	host.addItem("ep-2", "Tue, 22 Sep 2026 06:00:00 +0000")
	if _, err := service.Refresh(ctx, &feed); err != nil {
		t.Fatal(err)
	}
	episodes, _ = store.ListEpisodes(ctx, feed.ID)
	if episodes[0].DroppedAt != nil {
		t.Error("ep-1 still dropped after the source listed it again")
	}
}

func TestRenderDroppedEpisodes(t *testing.T) {
	ctx := context.Background()

	t.Run("not served by default", func(t *testing.T) {
		host := newFakeHost(t)
		service, _ := newTestService(t, Options{})
		feed := subscribeAndDrop(t, service, host)
		if guids, _ := renderedGUIDs(t, service, feed); strings.Join(guids, ",") != "ep-2" {
			t.Errorf("items = %v, want only ep-2", guids)
		}
	})

	t.Run("served from the kept item", func(t *testing.T) {
		host := newFakeHost(t)
		service, store := newTestService(t, Options{ServeDropped: true})
		feed := subscribeAndDrop(t, service, host)
		guids, output := renderedGUIDs(t, service, feed)
		if strings.Join(guids, ",") != "ep-2,ep-1" {
			t.Fatalf("items = %v, want ep-2 then the dropped ep-1", guids)
		}
		if strings.Contains(output, "media.example.com") {
			t.Error("the dropped episode's original audio URL is in the feed")
		}
		// Backlog keeps its own date.
		if !strings.Contains(output, "<pubDate>Mon, 21 Sep 2026 06:00:00 +0000</pubDate>") {
			t.Errorf("the dropped episode's date changed:\n%s", output)
		}

		// Its own setting wins over the feed's.
		episodes, _ := store.ListEpisodes(ctx, feed.ID)
		if err := store.SetEpisodeServe(ctx, feed.ID, episodes[0].ID, "off", ""); err != nil {
			t.Fatal(err)
		}
		if guids, _ := renderedGUIDs(t, service, feed); strings.Join(guids, ",") != "ep-2" {
			t.Errorf("items = %v, want ep-2 only with ep-1 switched off", guids)
		}
	})

	t.Run("per feed and per episode", func(t *testing.T) {
		host := newFakeHost(t)
		service, store := newTestService(t, Options{})
		feed := subscribeAndDrop(t, service, host)
		episodes, _ := store.ListEpisodes(ctx, feed.ID)
		if err := store.SetEpisodeServe(ctx, feed.ID, episodes[0].ID, "on", ""); err != nil {
			t.Fatal(err)
		}
		if guids, _ := renderedGUIDs(t, service, feed); strings.Join(guids, ",") != "ep-2,ep-1" {
			t.Errorf("items = %v, want ep-1 served by its own setting", guids)
		}
		if err := store.SetEpisodeServe(ctx, feed.ID, episodes[0].ID, "", ""); err != nil {
			t.Fatal(err)
		}
		feed.ServeDropped = "on"
		if err := service.Update(ctx, &feed); err != nil {
			t.Fatal(err)
		}
		if guids, _ := renderedGUIDs(t, service, feed); strings.Join(guids, ",") != "ep-2,ep-1" {
			t.Errorf("items = %v, want ep-1 served by the feed's setting", guids)
		}
	})

	t.Run("made up when the item wasn't kept", func(t *testing.T) {
		host := newFakeHost(t)
		service, store := newTestService(t, Options{ServeDropped: true})
		feed, _, err := service.Subscribe(ctx, host.server.URL, Settings{})
		if err != nil {
			t.Fatal(err)
		}
		dropped := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
		published := time.Date(2026, 8, 26, 13, 20, 0, 0, time.UTC)
		old := models.Episode{FeedID: feed.ID, GUID: "old-guid", SourceURL: "https://media.example.com/old.mp3", Title: "Old & gone",
			PublishedAt: &published, SourceSeconds: 900, Backlog: true, State: models.EpisodeReady, DroppedAt: &dropped}
		if err := store.CreateEpisode(ctx, &old); err != nil {
			t.Fatal(err)
		}
		guids, output := renderedGUIDs(t, service, feed)
		if strings.Join(guids, ",") != "ep-1,old-guid" {
			t.Fatalf("items = %v, want ep-1 then old-guid", guids)
		}
		if !strings.Contains(output, "<title>Old &amp; gone</title>") || !strings.Contains(output, "<itunes:duration>15:00</itunes:duration>") ||
			!strings.Contains(output, testURLs.Episode(feed.ID, old.ID, "mp3")) {
			t.Errorf("made-up item is missing its title, duration or Solstein URL:\n%s", output)
		}
	})
}

func TestDroppedSettings(t *testing.T) {
	service, _ := newTestService(t, Options{ServeDropped: true})
	feed := models.Feed{}
	if !service.FeedServesDropped(feed) || service.DeletesDropped(feed) {
		t.Error("defaults not followed")
	}
	feed.ServeDropped, feed.DeleteDropped = "off", "on"
	if service.FeedServesDropped(feed) || !service.DeletesDropped(feed) {
		t.Error("the feed's settings not followed")
	}
	if !service.ServesDropped(feed, models.Episode{Serve: "on"}) || service.ServesDropped(feed, models.Episode{}) {
		t.Error("the episode's setting not followed")
	}
	for _, bad := range []Settings{{ServeDropped: "yes"}, {DeleteDropped: "always"}} {
		if err := service.ValidateSettings(bad); !errors.Is(err, ErrInvalidSettings) {
			t.Errorf("%+v: err = %v, want ErrInvalidSettings", bad, err)
		}
	}
}
