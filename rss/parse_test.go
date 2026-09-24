package rss

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestParseShow(t *testing.T) {
	feed, err := Parse(readFixture(t, "show.xml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if feed.Title != "Example Show" {
		t.Errorf("title = %q", feed.Title)
	}
	if len(feed.Items) != 5 {
		t.Fatalf("got %d items, want 5", len(feed.Items))
	}

	first := feed.Items[0]
	if first.Index != 0 || first.GUID != "ep-3" || first.Key != "ep-3" {
		t.Errorf("first item identity = %+v", first)
	}
	if first.Title != "Episode 3: The third one" {
		t.Errorf("title with &nbsp; = %q", first.Title)
	}
	if first.Enclosure == nil || first.Enclosure.URL != "https://media.example.com/ep3.mp3?aid=abc&chunk=1" ||
		first.Enclosure.Length != 40480000 || first.Enclosure.Type != "audio/mpeg" {
		t.Errorf("enclosure = %+v", first.Enclosure)
	}
	if first.Duration != "00:42:10" {
		t.Errorf("duration = %q", first.Duration)
	}
	wantDate := time.Date(2026, 9, 22, 4, 0, 0, 0, time.UTC)
	if first.PublishedAt == nil || !first.PublishedAt.Equal(wantDate) {
		t.Errorf("pubDate = %v, want %v", first.PublishedAt, wantDate)
	}

	// <guid> after <enclosure> still counts.
	if second := feed.Items[1]; second.GUID != "ep-2" || second.Enclosure == nil {
		t.Errorf("second item = %+v", second)
	}

	// No guid: the enclosure URL is the key.
	if third := feed.Items[2]; third.GUID != "" || third.Key != "https://media.example.com/ep1.mp3" {
		t.Errorf("third item key = %q", third.Key)
	}

	// Only <media:content>: the first audio one becomes the enclosure.
	fourth := feed.Items[3]
	if fourth.Enclosure == nil || fourth.Enclosure.URL != "https://media.example.com/bonus.m4a" || fourth.Enclosure.Length != 1234 {
		t.Errorf("media:content fallback = %+v", fourth.Enclosure)
	}

	if fifth := feed.Items[4]; fifth.Enclosure != nil || fifth.Key != "text-1" {
		t.Errorf("item without audio = %+v", fifth)
	}
}

func TestParseLatin1(t *testing.T) {
	feed, err := Parse(readFixture(t, "latin1.xml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if feed.Title != "Blåbærsyltetøy" {
		t.Errorf("title = %q", feed.Title)
	}
	if len(feed.Items) != 1 || feed.Items[0].Title != "Første" {
		t.Errorf("items = %+v", feed.Items)
	}
}

func TestParseByteOrderMark(t *testing.T) {
	data := append([]byte("\xef\xbb\xbf"), []byte(`<?xml version="1.0"?><rss><channel><title>T</title></channel></rss>`)...)
	feed, err := Parse(data)
	if err != nil || feed.Title != "T" {
		t.Errorf("Parse = %+v, %v", feed, err)
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name    string
		data    string
		wantErr error
	}{
		{name: "atom feed", data: `<feed xmlns="http://www.w3.org/2005/Atom"><title>T</title></feed>`, wantErr: ErrNotRSS},
		{name: "html page", data: `<html><body>Not found</body></html>`, wantErr: ErrNotRSS},
		{name: "rss without channel", data: `<rss version="2.0"></rss>`, wantErr: ErrNotRSS},
		{name: "empty", data: ``, wantErr: ErrNotRSS},
		{name: "unsupported encoding", data: `<?xml version="1.0" encoding="Shift_JIS"?><rss/>`, wantErr: ErrUnsupportedEncoding},
		{name: "mismatched tags", data: `<rss><channel><title>T</channel></rss>`},
		{name: "unclosed", data: `<rss><channel><title>T</title>`},
		{name: "stray end tag", data: `</rss>`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.data))
			if err == nil {
				t.Fatal("expected an error")
			}
			if c.wantErr != nil && !errors.Is(err, c.wantErr) {
				t.Errorf("err = %v, want %v", err, c.wantErr)
			}
		})
	}
}

func TestParseDate(t *testing.T) {
	cases := []struct {
		text string
		want time.Time
	}{
		{"Tue, 22 Sep 2026 04:00:00 GMT", time.Date(2026, 9, 22, 4, 0, 0, 0, time.UTC)},
		{"Tue, 22 Sep 2026 06:00:00 +0200", time.Date(2026, 9, 22, 4, 0, 0, 0, time.UTC)},
		{"Tue, 2 Sep 2026 06:00:00 +0200", time.Date(2026, 9, 2, 4, 0, 0, 0, time.UTC)},
		{"Tue, 22 Sep 2026 06:00 +0200", time.Date(2026, 9, 22, 4, 0, 0, 0, time.UTC)},
		{"22 Sep 2026 04:00:00 +0000", time.Date(2026, 9, 22, 4, 0, 0, 0, time.UTC)},
		{"Sun, 20 Sep 2026 06:00:00 EST", time.Date(2026, 9, 20, 11, 0, 0, 0, time.UTC)},
		{"Sun, 20 Sep 2026 06:00:00 PDT", time.Date(2026, 9, 20, 13, 0, 0, 0, time.UTC)},
		{"2026-09-22T04:00:00Z", time.Date(2026, 9, 22, 4, 0, 0, 0, time.UTC)},
		{"2026-09-22", time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)},
		{"  Tue, 22 Sep 2026 04:00:00 GMT  ", time.Date(2026, 9, 22, 4, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		got := ParseDate(c.text)
		if got == nil || !got.Equal(c.want) {
			t.Errorf("ParseDate(%q) = %v, want %v", c.text, got, c.want)
		}
	}

	for _, text := range []string{"", "yesterday", "32 Foo 2026"} {
		if got := ParseDate(text); got != nil {
			t.Errorf("ParseDate(%q) = %v, want nil", text, got)
		}
	}
}
