package main

import (
	"context"
	"io"
	"strings"
	"testing"

	"aunefyren/solstein/database"
	"aunefyren/solstein/feeds"
	"aunefyren/solstein/logger"
	"aunefyren/solstein/models"

	"github.com/sirupsen/logrus"
	logTest "github.com/sirupsen/logrus/hooks/test"
)

// newFeedService stores the given feeds as they are, unvalidated, and
// returns a feed service over them that processes the feeds processed
// accepts. The store is returned so a test can make it fail.
func newFeedService(t *testing.T, processed func(models.Feed) bool, list ...models.Feed) (*feeds.Service, *database.Store) {
	t.Helper()
	store, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	for i := range list {
		if err := store.CreateFeed(context.Background(), &list[i]); err != nil {
			t.Fatal(err)
		}
	}
	return feeds.New(store, nil, feeds.Options{Processed: processed}), store
}

// captureWarnings swaps logger.Log for one recording its entries.
func captureWarnings(t *testing.T) *logTest.Hook {
	t.Helper()
	original := logger.Log
	t.Cleanup(func() { logger.Log = original })
	logger.Log = logrus.New()
	logger.Log.SetOutput(io.Discard)
	return logTest.NewLocal(logger.Log)
}

func messages(hook *logTest.Hook, level logrus.Level) string {
	var lines []string
	for _, entry := range hook.AllEntries() {
		if entry.Level == level {
			lines = append(lines, entry.Message)
		}
	}
	return strings.Join(lines, "\n")
}

func TestWarnAboutFeedSettings(t *testing.T) {
	hook := captureWarnings(t)
	service, store := newFeedService(t, nil,
		models.Feed{SourceURL: "https://a.example/feed", Title: "Gone Exit", Exit: "gone"},
		models.Feed{SourceURL: "https://b.example/feed", Title: "Diff On", RegionDiff: "on"},
		models.Feed{SourceURL: "https://c.example/feed", Title: "Diff Exits", RegionDiffExits: []string{"direct", "missing"}},
		models.Feed{SourceURL: "https://d.example/feed", Title: "Fine", Exit: "direct"},
	)
	ctx := context.Background()

	warnAboutFeedSettings(ctx, service, []string{"direct"}, false)
	warnings := messages(hook, logrus.WarnLevel)
	for _, want := range []string{"'Gone Exit' uses exit 'gone'", "'Diff On' has region diff switched on"} {
		if !strings.Contains(warnings, want) {
			t.Errorf("no warning %q in:\n%s", want, warnings)
		}
	}
	if strings.Contains(warnings, "Fine") || strings.Contains(warnings, "missing") {
		t.Errorf("unexpected warning in:\n%s", warnings)
	}

	hook.Reset()
	warnAboutFeedSettings(ctx, service, []string{"direct"}, true)
	warnings = messages(hook, logrus.WarnLevel)
	if !strings.Contains(warnings, "'Diff Exits' compares through exit 'missing'") || strings.Contains(warnings, "Diff On") {
		t.Errorf("region diff running:\n%s", warnings)
	}

	hook.Reset()
	store.Close()
	warnAboutFeedSettings(ctx, service, nil, false)
	if errors := messages(hook, logrus.ErrorLevel); !strings.Contains(errors, "Failed to check feeds' settings") {
		t.Errorf("store failure not logged: %q", errors)
	}
}

func TestWarnAboutWithholding(t *testing.T) {
	ctx := context.Background()
	all := func(models.Feed) bool { return true }
	named := func(title string) func(models.Feed) bool {
		return func(feed models.Feed) bool { return feed.Title == title }
	}
	twoFeeds := []models.Feed{
		{SourceURL: "https://a.example/feed", Title: "One"},
		{SourceURL: "https://b.example/feed", Title: "Two"},
	}

	cases := []struct {
		name       string
		processed  func(models.Feed) bool
		globalHide bool
		hide       func(models.Feed) bool
		want       string // "" for no warning
	}{
		{"every feed hides", all, true, all, "for every feed,"},
		{"some feeds hide", all, false, named("Two"), "for feeds 'Two',"},
		{"hide globally, nothing processed", nil, true, all, "every feed region diff handles"},
		{"nothing hides", all, false, func(models.Feed) bool { return false }, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hook := captureWarnings(t)
			service, _ := newFeedService(t, c.processed, twoFeeds...)
			warnAboutWithholding(ctx, service, c.globalHide, c.hide)
			warnings := messages(hook, logrus.WarnLevel)
			if c.want == "" && warnings != "" || !strings.Contains(warnings, c.want) {
				t.Errorf("warnings %q, want %q", warnings, c.want)
			}
		})
	}

	hook := captureWarnings(t)
	service, store := newFeedService(t, all)
	store.Close()
	warnAboutWithholding(ctx, service, true, all)
	if errors := messages(hook, logrus.ErrorLevel); !strings.Contains(errors, "Failed to check feeds' failure policies") {
		t.Errorf("store failure not logged: %q", errors)
	}
}
