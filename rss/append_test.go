package rss

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

const appendFeed = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd">
  <channel>
    <title>Show</title>
    <item>
      <title>New</title>
      <guid>new</guid>
      <enclosure url="https://cdn.example.com/new.mp3" type="audio/mpeg" length="10"/>
    </item>
  </channel>
</rss>
`

func TestParseKeepsRawItems(t *testing.T) {
	feed, err := Parse([]byte(appendFeed))
	if err != nil {
		t.Fatal(err)
	}
	raw := string(feed.Items[0].Raw)
	if !strings.HasPrefix(raw, "<item>") || !strings.HasSuffix(raw, "</item>") || !strings.Contains(raw, "<guid>new</guid>") {
		t.Errorf("Raw = %q, want the whole item element", raw)
	}
}

func TestRewriteAppendsItems(t *testing.T) {
	old := []byte(`<item>
      <title>Old</title>
      <guid>old</guid>
      <itunes:duration>12:00</itunes:duration>
      <enclosure url="https://cdn.example.com/old.mp3" type="audio/mpeg" length="5"/>
    </item>`)
	var seen []string
	output, err := Rewrite{
		Append: [][]byte{old},
		Item: func(item Item) ItemChange {
			seen = append(seen, item.Key)
			return ItemChange{EnclosureURL: "https://solstein.example.com/" + item.Key + ".mp3", EpisodeType: "full"}
		},
	}.Apply([]byte(appendFeed))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(seen, ",") != "new,old" {
		t.Errorf("Item called for %v, want new then old", seen)
	}
	text := string(output)
	if !strings.Contains(text, "</item>\n    <item>\n      <title>Old</title>") {
		t.Errorf("appended item isn't after the last one with its indentation:\n%s", text)
	}
	if !strings.Contains(text, `url="https://solstein.example.com/old.mp3"`) || strings.Contains(text, "cdn.example.com/old.mp3") {
		t.Errorf("appended item wasn't rewritten:\n%s", text)
	}
	parsed, err := Parse(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Items) != 2 || parsed.Items[1].Duration != "12:00" || parsed.Items[1].EpisodeType != "full" {
		t.Errorf("parsed items = %+v", parsed.Items)
	}
}

func TestRewriteAppendsToChannelWithoutItems(t *testing.T) {
	feed := []byte(`<rss version="2.0"><channel><title>Empty</title></channel></rss>`)
	item := MinimalItem("Gone", "gone-1", nil, "https://cdn.example.com/gone.mp3", "audio/mpeg", 0, "")
	output, err := Rewrite{Append: [][]byte{item}}.Apply(feed)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Items) != 1 || parsed.Items[0].Key != "gone-1" || !bytes.HasSuffix(output, []byte("</channel></rss>")) {
		t.Errorf("output = %s", output)
	}
}

func TestMinimalItem(t *testing.T) {
	date := time.Date(2026, 8, 26, 13, 20, 0, 0, time.UTC)
	item := MinimalItem(`Tom & "Jerry"`, "https://cdn.example.com/a.mp3", &date, "https://cdn.example.com/a.mp3?x=1&y=2", "audio/mpeg", 1234, "1:02:03")
	// A feed binding iTunes to another prefix: the item declares its own.
	feed := []byte(`<rss version="2.0" xmlns:it="http://www.itunes.com/dtds/podcast-1.0.dtd"><channel><title>T</title></channel></rss>`)
	output, err := Rewrite{Append: [][]byte{item}}.Apply(feed)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(output)
	if err != nil {
		t.Fatal(err)
	}
	got := parsed.Items[0]
	if got.Title != `Tom & "Jerry"` || got.Key != "https://cdn.example.com/a.mp3" || got.Duration != "1:02:03" ||
		got.PublishedAt == nil || !got.PublishedAt.Equal(date) ||
		got.Enclosure == nil || got.Enclosure.URL != "https://cdn.example.com/a.mp3?x=1&y=2" || got.Enclosure.Length != 1234 {
		t.Errorf("parsed minimal item = %+v (enclosure %+v)", got, got.Enclosure)
	}
}
