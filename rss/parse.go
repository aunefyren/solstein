package rss

import (
	"encoding/xml"
	"io"
	"strconv"
	"strings"
	"time"
)

// Feed is what Solstein needs to know about a source feed.
type Feed struct {
	Title string
	Items []Item
}

// Item is one <item> of the channel.
type Item struct {
	// Index is the item's position among the channel's items, starting at 0.
	Index int
	// GUID is the item's <guid> as written, possibly empty.
	GUID string
	// Key identifies the item: its GUID, or its enclosure URL when it has no
	// GUID. Empty when it has neither.
	Key         string
	Title       string
	PublishedAt *time.Time
	// Duration is <itunes:duration> as written.
	Duration string
	// Enclosure is the audio, or nil for items without any.
	Enclosure *Enclosure
}

// Enclosure is an item's audio file.
type Enclosure struct {
	URL    string
	Type   string
	Length int64
}

// Parse reads a feed. Items keep document order.
func Parse(data []byte) (Feed, error) {
	data, err := normalise(data)
	if err != nil {
		return Feed{}, err
	}
	return parse(data)
}

// parse expects normalised data.
func parse(data []byte) (Feed, error) {
	walker := newWalker(data)
	var (
		feed       Feed
		item       *Item
		media      *Enclosure // first audio <media:content>, used if no <enclosure>
		text       strings.Builder
		collect    bool
		sawRoot    bool
		sawChannel bool
	)

	for {
		tok, err := walker.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Feed{}, err
		}
		depth := walker.depth()

		switch value := tok.value.(type) {
		case xml.StartElement:
			switch {
			case depth == 1:
				if tok.frame.space != "" || tok.frame.local != "rss" {
					return Feed{}, ErrNotRSS
				}
				sawRoot = true
			case depth == 2 && walker.is(1, "", "channel"):
				sawChannel = true
			case depth == 3 && walker.is(1, "", "channel") && tok.frame.space == "" && tok.frame.local == "title":
				text.Reset()
				collect = true
			case depth == 3 && walker.is(1, "", "channel") && tok.frame.space == "" && tok.frame.local == "item":
				item = &Item{Index: len(feed.Items)}
				media = nil
			case depth == 4 && item != nil:
				switch {
				case tok.frame.space == "" && (tok.frame.local == "guid" || tok.frame.local == "title" || tok.frame.local == "pubDate"),
					tok.frame.space == itunesNS && tok.frame.local == "duration":
					text.Reset()
					collect = true
				case tok.frame.space == "" && tok.frame.local == "enclosure" && item.Enclosure == nil:
					item.Enclosure = enclosureFrom(value, "length")
				case tok.frame.space == mediaNS && tok.frame.local == "content" && media == nil:
					if strings.HasPrefix(attribute(value, "type"), "audio") {
						media = enclosureFrom(value, "fileSize")
					}
				}
			}
		case xml.CharData:
			if collect {
				text.Write(value)
			}
		case xml.EndElement:
			// depth is now the parent's depth.
			switch {
			case depth == 2 && collect && tok.frame.local == "title" && item == nil:
				feed.Title = strings.TrimSpace(text.String())
			case depth == 3 && collect && item != nil:
				content := strings.TrimSpace(text.String())
				switch {
				case tok.frame.space == itunesNS:
					item.Duration = content
				case tok.frame.local == "guid":
					item.GUID = content
				case tok.frame.local == "title":
					item.Title = content
				case tok.frame.local == "pubDate":
					item.PublishedAt = ParseDate(content)
				}
			case depth == 2 && item != nil && tok.frame.local == "item":
				if item.Enclosure == nil && media != nil {
					item.Enclosure = media
				}
				item.Key = item.GUID
				if item.Key == "" && item.Enclosure != nil {
					item.Key = item.Enclosure.URL
				}
				feed.Items = append(feed.Items, *item)
				item = nil
			}
			collect = false
		}
	}

	if !sawRoot || !sawChannel {
		return Feed{}, ErrNotRSS
	}
	return feed, nil
}

func attribute(element xml.StartElement, local string) string {
	for _, attr := range element.Attr {
		if attr.Name.Space == "" && attr.Name.Local == local {
			return attr.Value
		}
	}
	return ""
}

func enclosureFrom(element xml.StartElement, lengthAttribute string) *Enclosure {
	url := strings.TrimSpace(attribute(element, "url"))
	if url == "" {
		return nil
	}
	length, _ := strconv.ParseInt(strings.TrimSpace(attribute(element, lengthAttribute)), 10, 64)
	return &Enclosure{URL: url, Type: attribute(element, "type"), Length: length}
}

// dateLayouts are the pubDate forms seen in real feeds: RFC 822/1123 with
// and without seconds, weekday or numeric zone, and ISO 8601 from feeds that
// ignore the spec.
var dateLayouts = []string{
	time.RFC1123Z,
	time.RFC1123,
	"Mon, 2 Jan 2006 15:04:05 -0700",
	"Mon, 2 Jan 2006 15:04:05 MST",
	"Mon, 02 Jan 2006 15:04 -0700",
	"Mon, 02 Jan 2006 15:04 MST",
	"Mon, 2 Jan 2006 15:04 -0700",
	"02 Jan 2006 15:04:05 -0700",
	"2 Jan 2006 15:04:05 -0700",
	"02 Jan 2006 15:04:05 MST",
	"2 Jan 2006 15:04:05 MST",
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02",
}

// ParseDate parses a feed date, or returns nil if it can't. A named zone Go
// doesn't know (e.g. "EST" on a UTC host) is read as that zone's offset
// where it is a common US one, since that is how most such feeds mean it.
func ParseDate(text string) *time.Time {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	for _, layout := range dateLayouts {
		parsed, err := time.Parse(layout, text)
		if err != nil {
			continue
		}
		if offset, ok := usZoneOffsets[parsed.Location().String()]; ok {
			// Go gives an abbreviation it doesn't know a zero offset.
			if _, zoneOffset := parsed.Zone(); zoneOffset == 0 {
				parsed = time.Date(parsed.Year(), parsed.Month(), parsed.Day(), parsed.Hour(), parsed.Minute(), parsed.Second(), 0, time.FixedZone(parsed.Location().String(), offset))
			}
		}
		utc := parsed.UTC()
		return &utc
	}
	return nil
}

var usZoneOffsets = map[string]int{
	"EST": -5 * 3600, "EDT": -4 * 3600,
	"CST": -6 * 3600, "CDT": -5 * 3600,
	"MST": -7 * 3600, "MDT": -6 * 3600,
	"PST": -8 * 3600, "PDT": -7 * 3600,
}
