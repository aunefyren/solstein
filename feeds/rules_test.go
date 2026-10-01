package feeds

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"aunefyren/solstein/database"
	"aunefyren/solstein/models"
	"aunefyren/solstein/rss"

	"github.com/google/uuid"
)

func TestCompileRules(t *testing.T) {
	cases := []struct {
		name  string
		rule  Rule
		valid bool
	}{
		{"hide", Rule{Action: RuleHide, MaxSeconds: 1800}, true},
		{"tag", Rule{Action: RuleTag, EpisodeType: "bonus", TitleMatches: `^\d+`}, true},
		{"no conditions", Rule{Action: RuleTag, EpisodeType: "full"}, true},
		{"unknown action", Rule{Action: "delete"}, false},
		{"no action", Rule{}, false},
		{"tag without type", Rule{Action: RuleTag}, false},
		{"tag with unknown type", Rule{Action: RuleTag, EpisodeType: "extra"}, false},
		{"hide with a type", Rule{Action: RuleHide, EpisodeType: "bonus"}, false},
		{"negative duration", Rule{Action: RuleHide, MinSeconds: -1}, false},
		{"min above max", Rule{Action: RuleHide, MinSeconds: 600, MaxSeconds: 60}, false},
		{"bad expression", Rule{Action: RuleHide, TitleMatches: "(unclosed"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateRules([]Rule{{Action: RuleHide}, c.rule})
			if c.valid && err != nil {
				t.Errorf("err = %v, want valid", err)
			}
			if !c.valid {
				if !errors.Is(err, ErrInvalidSettings) || !strings.Contains(err.Error(), "rule 2") {
					t.Errorf("err = %v, want ErrInvalidSettings naming rule 2", err)
				}
			}
		})
	}
	if err := ValidateRules(make([]Rule, maxRules+1)); !errors.Is(err, ErrInvalidSettings) {
		t.Errorf("too many rules: err = %v", err)
	}
}

func TestRulesMatch(t *testing.T) {
	list, err := compileRules([]models.FeedRule{
		{Action: RuleHide, TitleMatches: "trailer"},
		{Action: RuleTag, EpisodeType: "bonus", MaxSeconds: 1800},
		{Action: RuleTag, EpisodeType: "full", MinSeconds: 3600},
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		title   string
		seconds int
		hides   bool
		tag     string
	}{
		{"Season 2 TRAILER", 60, true, ""}, // first match wins, case-insensitive
		{"Fluff!", 1000, false, "bonus"},
		{"Fluff!", 1800, false, "bonus"}, // bounds are inclusive
		{"A / B / C", 9000, false, "full"},
		{"A / B / C", 3600, false, "full"},
		{"Middling", 2400, false, ""}, // no rule matches
		{"No duration", 0, false, ""}, // duration rules need a stated one
	}
	for _, c := range cases {
		episode := models.Episode{Title: c.title, SourceSeconds: c.seconds}
		if got := list.hides(episode); got != c.hides {
			t.Errorf("%q, %d s: hides = %v, want %v", c.title, c.seconds, got, c.hides)
		}
		if got := list.episodeType(episode); got != c.tag {
			t.Errorf("%q, %d s: tag = %q, want %q", c.title, c.seconds, got, c.tag)
		}
	}
}

// addTimedItem adds an item with a title of its own and a duration.
func (host *fakeHost) addTimedItem(guid, title, pubDate, duration string) {
	host.mutex.Lock()
	defer host.mutex.Unlock()
	host.items = append([]string{fmt.Sprintf(
		`<item xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd"><title>%s</title><guid>%s</guid><pubDate>%s</pubDate><itunes:duration>%s</itunes:duration><enclosure url="https://media.example.com/%s.mp3" type="audio/mpeg" length="100"/></item>`,
		title, guid, pubDate, duration, guid)}, host.items...)
}

func TestRulesHideAndTag(t *testing.T) {
	host := newFakeHost(t)
	service, store := newTestService(t, Options{})
	ctx := context.Background()
	feed, _, err := service.Subscribe(ctx, host.server.URL, Settings{})
	if err != nil {
		t.Fatal(err)
	}

	rules := []Rule{
		{Action: RuleHide, TitleMatches: "trailer"},
		{Action: RuleTag, EpisodeType: "bonus", MaxSeconds: 1800},
		{Action: RuleTag, EpisodeType: "full"},
	}
	if shown, err := service.SetRules(ctx, feed.ID, rules); err != nil || shown != 0 {
		t.Fatalf("SetRules = %d, %v", shown, err)
	}
	saved, err := service.Rules(ctx, feed.ID)
	if err != nil || len(saved) != 3 || saved[0].Action != RuleHide || saved[2].EpisodeType != "full" {
		t.Fatalf("Rules = %+v, %v", saved, err)
	}

	// The trailer is older than the bonus clip: shown, it would be prepared
	// first and hold the clip back.
	host.addTimedItem("trailer", "Trailer: season 2", "Tue, 22 Sep 2026 06:00:00 +0000", "2:00")
	host.addTimedItem("clip", "Fluff!", "Wed, 23 Sep 2026 15:45:00 +0000", "16:41")
	added, err := service.Refresh(ctx, &feed)
	if err != nil || len(added) != 2 {
		t.Fatalf("Refresh = %+v, %v", added, err)
	}
	for _, episode := range added {
		if episode.Hidden != (episode.GUID == "trailer") {
			t.Errorf("%s: hidden = %v", episode.GUID, episode.Hidden)
		}
	}

	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	claimed, err := store.ClaimNextEpisode(ctx, now, "cache", nil)
	if err != nil || claimed.GUID != "clip" {
		t.Fatalf("claimed %q, %v; want the clip, not the hidden trailer", claimed.GUID, err)
	}
	if _, err := store.ClaimNextEpisode(ctx, now, "cache", nil); !errors.Is(err, database.ErrNoWork) {
		t.Fatalf("second claim: err = %v, want ErrNoWork", err)
	}
	claimed.State, claimed.CacheFile, claimed.CacheSize = models.EpisodeReady, "clip.mp3", 1
	if err := store.UpdateEpisode(ctx, &claimed); err != nil {
		t.Fatal(err)
	}

	output, err := service.Render(ctx, feed, testURLs)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := rss.Parse(output)
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]string{}
	for _, item := range parsed.Items {
		types[item.GUID] = item.EpisodeType
	}
	if len(types) != 2 || types["clip"] != "bonus" || types["ep-1"] != "full" {
		t.Errorf("served items and types = %v, want the clip as bonus and ep-1 as full, no trailer", types)
	}

	// Only the rules change it: a pipeline saving a copy it loaded doesn't.
	episodes, _ := store.ListEpisodes(ctx, feed.ID)
	for _, episode := range episodes {
		if episode.GUID == "trailer" {
			episode.Hidden = false
			if err := store.UpdateEpisode(ctx, &episode); err != nil {
				t.Fatal(err)
			}
		}
	}
	if stored, _ := store.ListEpisodes(ctx, feed.ID); !stored[1].Hidden || stored[1].GUID != "trailer" {
		t.Errorf("UpdateEpisode changed hidden: %+v", stored[1])
	}

	// Without the rules the trailer comes back, to be prepared.
	shown, err := service.SetRules(ctx, feed.ID, nil)
	if err != nil || shown != 1 {
		t.Fatalf("SetRules(nil) = %d, %v; want the trailer shown", shown, err)
	}
	claimed, err = store.ClaimNextEpisode(ctx, now, "cache", nil)
	if err != nil || claimed.GUID != "trailer" {
		t.Errorf("claimed %q, %v; want the trailer", claimed.GUID, err)
	}
	output, _ = service.Render(ctx, feed, testURLs)
	if strings.Contains(string(output), "episodeType") {
		t.Errorf("rules removed, but episodes still tagged:\n%s", output)
	}
}

func TestSetRulesRefusesInvalidRules(t *testing.T) {
	host := newFakeHost(t)
	service, _ := newTestService(t, Options{})
	ctx := context.Background()
	feed, _, _ := service.Subscribe(ctx, host.server.URL, Settings{})
	if _, err := service.SetRules(ctx, feed.ID, []Rule{{Action: RuleHide}}); err != nil {
		t.Fatal(err)
	}

	if _, err := service.SetRules(ctx, feed.ID, []Rule{{Action: RuleTag}}); !errors.Is(err, ErrInvalidSettings) {
		t.Errorf("err = %v, want ErrInvalidSettings", err)
	}
	if saved, _ := service.Rules(ctx, feed.ID); len(saved) != 1 || saved[0].Action != RuleHide {
		t.Errorf("rules after a refused change = %+v, want the old one", saved)
	}

	if _, err := service.SetRules(ctx, uuid.New(), nil); !errors.Is(err, database.ErrFeedNotFound) {
		t.Errorf("unknown feed: err = %v", err)
	}
	if _, err := service.Rules(ctx, uuid.New()); !errors.Is(err, database.ErrFeedNotFound) {
		t.Errorf("unknown feed: Rules err = %v", err)
	}

	// Deleting the feed takes its rules with it.
	if err := service.Delete(ctx, feed.ID); err != nil {
		t.Fatal(err)
	}
	if left, err := service.store.ListFeedRules(ctx, feed.ID); err != nil || len(left) != 0 {
		t.Errorf("rules left after delete: %+v, %v", left, err)
	}
}
