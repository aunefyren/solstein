package feeds

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"aunefyren/solstein/models"
)

func TestPollerDue(t *testing.T) {
	poller := NewPoller(nil, 15*time.Minute, nil)
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	ago := func(duration time.Duration) *time.Time {
		at := now.Add(-duration)
		return &at
	}
	cases := []struct {
		name string
		feed models.Feed
		want bool
	}{
		{"never polled", models.Feed{}, true},
		{"polled just now", models.Feed{LastPolledAt: ago(time.Minute)}, false},
		{"default interval passed", models.Feed{LastPolledAt: ago(15 * time.Minute)}, true},
		{"own longer interval not passed", models.Feed{LastPolledAt: ago(20 * time.Minute), PollIntervalMinutes: 60}, false},
		{"own shorter interval passed", models.Feed{LastPolledAt: ago(6 * time.Minute), PollIntervalMinutes: 5}, true},
	}
	for _, c := range cases {
		if got := poller.due(c.feed, now); got != c.want {
			t.Errorf("%s: due = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPollDue(t *testing.T) {
	host := newFakeHost(t)
	clock := &testClock{now: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)}
	service, store := newTestService(t, Options{Now: clock.Now})
	ctx := context.Background()
	if _, _, err := service.Subscribe(ctx, host.server.URL, Settings{}); err != nil {
		t.Fatal(err)
	}

	var woken atomic.Int32
	poller := NewPoller(service, 15*time.Minute, func() { woken.Add(1) })

	// Just subscribed, so not due: nothing fetched.
	requests := host.requests.Load()
	if err := poller.PollDue(ctx); err != nil {
		t.Fatal(err)
	}
	if host.requests.Load() != requests {
		t.Error("polled a feed that wasn't due")
	}

	// Due, with a new episode: fetched, stored, pipeline woken.
	host.addItem("ep-2", "Tue, 22 Sep 2026 06:00:00 +0000")
	clock.advance(16 * time.Minute)
	if err := poller.PollDue(ctx); err != nil {
		t.Fatal(err)
	}
	if woken.Load() != 1 {
		t.Errorf("pipeline woken %d times, want 1", woken.Load())
	}
	feeds, _ := store.ListFeeds(ctx)
	episodes, _ := store.ListEpisodes(ctx, feeds[0].ID)
	if len(episodes) != 2 {
		t.Errorf("episodes = %d, want 2", len(episodes))
	}

	// Due again with nothing new, then a failing source: no wake either time.
	clock.advance(16 * time.Minute)
	poller.PollDue(ctx)
	host.set(func(host *fakeHost) { host.status = 500 })
	clock.advance(16 * time.Minute)
	poller.PollDue(ctx)
	if woken.Load() != 1 {
		t.Errorf("pipeline woken %d times, want still 1", woken.Load())
	}
}

func TestPollerRunStops(t *testing.T) {
	service, _ := newTestService(t, Options{})
	poller := NewPoller(service, time.Minute, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		poller.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
