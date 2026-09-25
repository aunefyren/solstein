package feeds

import (
	"errors"
	"testing"

	"aunefyren/solstein/models"

	"github.com/google/uuid"
)

func TestSourceFromPrefixPath(t *testing.T) {
	cases := []struct {
		rest, query, want string
	}{
		{"https://feeds.acast.com/public/shows/abc", "", "https://feeds.acast.com/public/shows/abc"},
		{"https:/feeds.acast.com/public/shows/abc", "", "https://feeds.acast.com/public/shows/abc"},
		{"feeds.acast.com/public/shows/abc", "", "https://feeds.acast.com/public/shows/abc"},
		{"/https://feeds.acast.com/x", "", "https://feeds.acast.com/x"},
		{"http://Feeds.Example.COM:80/Show", "", "http://feeds.example.com/Show"},
		{"HTTPS://example.com/feed", "", "https://example.com/feed"},
		{"example.com/feed", "token=abc&b=%2F", "https://example.com/feed?token=abc&b=%2F"},
		{"example.com/a%20b/feed.xml", "", "https://example.com/a%20b/feed.xml"},
		{"example.com:8443/feed", "", "https://example.com:8443/feed"},
		{"example.com", "", "https://example.com/"},
	}
	for _, c := range cases {
		got, err := SourceFromPrefixPath(c.rest, c.query)
		if err != nil || got != c.want {
			t.Errorf("SourceFromPrefixPath(%q, %q) = %q, %v; want %q", c.rest, c.query, got, err, c.want)
		}
	}

	for _, rest := range []string{"", "https://", "https:/", "ftp://example.com/feed", "https://user:pass@example.com/feed"} {
		if _, err := SourceFromPrefixPath(rest, ""); !errors.Is(err, ErrInvalidSourceURL) {
			t.Errorf("SourceFromPrefixPath(%q): err = %v, want ErrInvalidSourceURL", rest, err)
		}
	}
}

func TestNormaliseSourceURL(t *testing.T) {
	cases := []struct{ raw, want string }{
		{" https://Example.com/Feed#top ", "https://example.com/Feed"},
		{"https://example.com:443/feed", "https://example.com/feed"},
		{"http://[2001:db8::1]:8080/feed", "http://[2001:db8::1]:8080/feed"},
		{"http://[2001:db8::1]/feed", "http://[2001:db8::1]/feed"},
	}
	for _, c := range cases {
		got, err := NormaliseSourceURL(c.raw)
		if err != nil || got != c.want {
			t.Errorf("NormaliseSourceURL(%q) = %q, %v; want %q", c.raw, got, err, c.want)
		}
	}
	for _, raw := range []string{"not a url", "/relative/path", "mailto:x@example.com", "https://%zz"} {
		if _, err := NormaliseSourceURL(raw); !errors.Is(err, ErrInvalidSourceURL) {
			t.Errorf("NormaliseSourceURL(%q): err = %v, want ErrInvalidSourceURL", raw, err)
		}
	}
}

func TestHostAllowed(t *testing.T) {
	allowed := []string{"acast.com", "example.org"}
	cases := []struct {
		url  string
		want bool
	}{
		{"https://acast.com/feed", true},
		{"https://feeds.acast.com/feed", true},
		{"https://FEEDS.ACAST.COM/feed", true},
		{"https://notacast.com/feed", false},
		{"https://acast.com.evil.example/feed", false},
		{"https://example.org:8443/feed", true},
	}
	for _, c := range cases {
		if got := hostAllowed(c.url, allowed); got != c.want {
			t.Errorf("hostAllowed(%q) = %v, want %v", c.url, got, c.want)
		}
	}
	if !hostAllowed("https://anything.example/feed", nil) {
		t.Error("empty list should allow everything")
	}
}

func TestAudioExtension(t *testing.T) {
	cases := []struct{ url, mimeType, want string }{
		{"https://media.example.com/ep.mp3?aid=1", "audio/mpeg", "mp3"},
		{"https://media.example.com/ep.M4A", "", "m4a"},
		{"https://media.example.com/stream?id=1", "audio/mp4", "m4a"},
		{"https://media.example.com/stream", "audio/mpeg; charset=binary", "mp3"},
		{"https://media.example.com/file.php", "application/octet-stream", "mp3"},
		{"https://media.example.com/ep.opus", "audio/ogg", "opus"},
	}
	for _, c := range cases {
		if got := AudioExtension(c.url, c.mimeType); got != c.want {
			t.Errorf("AudioExtension(%q, %q) = %q, want %q", c.url, c.mimeType, got, c.want)
		}
	}
}

func TestPublishedEpisodes(t *testing.T) {
	episode := func(state models.EpisodeState, backlog bool) models.Episode {
		return models.Episode{Base: models.Base{ID: uuid.New()}, State: state, Backlog: backlog}
	}
	// Oldest first, as ListEpisodes returns them.
	backlog := episode(models.EpisodeReady, true)
	monday := episode(models.EpisodeReady, false)
	tuesday := episode(models.EpisodeAcquiring, false)
	wednesday := episode(models.EpisodeReady, false)
	failed := episode(models.EpisodeFailed, false)
	episodes := []models.Episode{backlog, monday, tuesday, wednesday}

	cache := publishedEpisodes(episodes, true)
	if !cache[backlog.ID] || !cache[monday.ID] {
		t.Error("backlog and ready episodes should be published")
	}
	if cache[tuesday.ID] {
		t.Error("pending episode published")
	}
	if cache[wednesday.ID] {
		t.Error("ready episode newer than a pending one published; ABS would skip the pending one")
	}

	withFailure := publishedEpisodes([]models.Episode{monday, failed, wednesday}, true)
	if !withFailure[failed.ID] || !withFailure[wednesday.ID] {
		t.Error("a failed episode must not hold back newer ones")
	}

	// Nothing ready and no backlog: the oldest is published anyway.
	first, second := episode(models.EpisodeDiscovered, false), episode(models.EpisodeAcquiring, false)
	onlyPending := publishedEpisodes([]models.Episode{first, second}, true)
	if len(onlyPending) != 1 || !onlyPending[first.ID] {
		t.Errorf("feed with only pending episodes published %v, want just the oldest", onlyPending)
	}
	if empty := publishedEpisodes(nil, true); len(empty) != 0 {
		t.Errorf("no episodes: published %v", empty)
	}

	// A withheld failure is left out without holding newer episodes back.
	withheld := episode(models.EpisodeFailed, false)
	withheld.Withheld = true
	withWithheld := publishedEpisodes([]models.Episode{monday, withheld, wednesday}, true)
	if withWithheld[withheld.ID] || !withWithheld[wednesday.ID] {
		t.Errorf("withheld episode: published %v, want monday and wednesday only", withWithheld)
	}
	// Withheld beats backlog: the unprocessed version isn't served.
	withheldBacklog := episode(models.EpisodeFailed, true)
	withheldBacklog.Withheld = true
	if publishedEpisodes([]models.Episode{withheldBacklog, monday}, true)[withheldBacklog.ID] {
		t.Error("withheld backlog episode published")
	}
	// Never an empty feed, but a withheld episode is the last resort.
	onlyWithheld := publishedEpisodes([]models.Episode{withheld, first}, true)
	if len(onlyWithheld) != 1 || !onlyWithheld[first.ID] {
		t.Errorf("feed with a withheld and a pending episode published %v, want the pending one", onlyWithheld)
	}

	all := publishedEpisodes(episodes, false)
	if len(all) != len(episodes) {
		t.Errorf("without preparing, published %d of %d", len(all), len(episodes))
	}
}
