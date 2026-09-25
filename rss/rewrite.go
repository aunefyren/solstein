package rss

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
)

// Rewrite describes how to turn a source feed into the feed Solstein serves.
type Rewrite struct {
	// FeedURL replaces <itunes:new-feed-url> and <atom:link rel="self">, so
	// clients that follow them stay on Solstein. Empty leaves them as they are.
	FeedURL string
	// Item decides what happens to each item. Nil keeps every item unchanged.
	Item func(Item) ItemChange
}

// ItemChange is what to do with one item. The zero value keeps it unchanged.
type ItemChange struct {
	// Omit leaves the item out of the served feed.
	Omit bool
	// EnclosureURL replaces the audio URL: on <enclosure>, and on any other
	// attribute in the item holding the same URL (<media:content>,
	// <podcast:source>), so no route to the original audio is left.
	EnclosureURL string
	// Length, when positive, sets the enclosure's length in bytes, and the
	// fileSize of a <media:content> holding the same audio.
	Length int64
	// Duration, when non-empty, replaces <itunes:duration>. See FormatDuration.
	Duration string
	// PublishedAt, when set, replaces the item's <pubDate>.
	PublishedAt *time.Time
}

// Apply rewrites a feed. Bytes outside the changed values are copied through
// unchanged; the output is always UTF-8.
func (rewrite Rewrite) Apply(data []byte) ([]byte, error) {
	data, err := normalise(data)
	if err != nil {
		return nil, err
	}
	// First pass: read every item, because an item's <guid> can come after
	// its <enclosure> and the change for an item depends on both.
	feed, err := parse(data)
	if err != nil {
		return nil, err
	}

	var (
		output    bytes.Buffer
		cursor    int64 // data before this offset has been handled
		itemIndex = -1
		change    ItemChange
		source    string      // current item's original enclosure URL
		skipUntil = -1        // depth at which an omitted item closes
		textFrom  = int64(-1) // start of the text being replaced, or -1
		textDepth int         // depth of the element whose text is replaced
		textValue string
	)
	copyUpTo := func(offset int64) {
		output.Write(data[cursor:offset])
		cursor = offset
	}

	walker := newWalker(data)
	for {
		tok, err := walker.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		depth := walker.depth()

		if skipUntil >= 0 {
			if _, ok := tok.value.(xml.EndElement); ok && depth == skipUntil {
				cursor = tok.end
				skipUntil = -1
			}
			continue
		}

		switch value := tok.value.(type) {
		case xml.StartElement:
			inChannel := walker.is(1, "", "channel")

			if depth == 3 && inChannel && tok.frame.space == "" && tok.frame.local == "item" {
				itemIndex++
				item := feed.Items[itemIndex]
				change = ItemChange{}
				if rewrite.Item != nil {
					change = rewrite.Item(item)
				}
				source = ""
				if item.Enclosure != nil {
					source = item.Enclosure.URL
				}
				if change.Omit {
					copyUpTo(tok.start)
					// Drop the indentation before the item too, so omitted
					// items don't leave blank lines behind.
					output.Truncate(len(bytes.TrimRight(output.Bytes(), " \t\r\n")))
					skipUntil = depth - 1
					if tok.selfClosing {
						cursor = tok.end
						skipUntil = -1
					}
				}
				continue
			}

			var (
				attrs   = value.Attr
				changed bool
				text    string
				hasText bool
			)
			switch {
			case depth == 3 && inChannel && rewrite.FeedURL != "" && tok.frame.space == atomNS && tok.frame.local == "link" && attribute(value, "rel") == "self":
				attrs, changed = setAttribute(attrs, "href", rewrite.FeedURL)
			case depth == 3 && inChannel && rewrite.FeedURL != "" && tok.frame.space == itunesNS && tok.frame.local == "new-feed-url":
				text, hasText = rewrite.FeedURL, true
			case depth >= 4 && itemIndex >= 0 && walker.is(2, "", "item"):
				if depth == 4 && change.Duration != "" && tok.frame.space == itunesNS && tok.frame.local == "duration" {
					text, hasText = change.Duration, true
				}
				if depth == 4 && change.PublishedAt != nil && tok.frame.space == "" && tok.frame.local == "pubDate" {
					text, hasText = change.PublishedAt.UTC().Format(time.RFC1123Z), true
				}
				holdsAudio := source != "" && hasAttributeValue(attrs, source)
				if change.EnclosureURL != "" && holdsAudio {
					attrs, changed = replaceAttributeValue(attrs, source, change.EnclosureURL)
				}
				if change.Length > 0 && holdsAudio && tok.frame.space == mediaNS && tok.frame.local == "content" && attribute(value, "fileSize") != "" {
					var sizeChanged bool
					attrs, sizeChanged = setAttribute(attrs, "fileSize", strconv.FormatInt(change.Length, 10))
					changed = changed || sizeChanged
				}
				if depth == 4 && change.Length > 0 && tok.frame.space == "" && tok.frame.local == "enclosure" {
					var lengthChanged bool
					attrs, lengthChanged = setAttribute(attrs, "length", strconv.FormatInt(change.Length, 10))
					changed = changed || lengthChanged
				}
			}

			switch {
			case hasText && tok.selfClosing:
				// <itunes:duration/> gets content: write it out in full.
				copyUpTo(tok.start)
				writeStart(&output, value.Name, attrs, false)
				xml.EscapeText(&output, []byte(text))
				fmt.Fprintf(&output, "</%s>", qualifiedName(value.Name))
				cursor = tok.end
			case hasText:
				if changed {
					copyUpTo(tok.start)
					writeStart(&output, value.Name, attrs, false)
					cursor = tok.end
				}
				textFrom, textDepth, textValue = tok.end, depth, text
			case changed:
				copyUpTo(tok.start)
				writeStart(&output, value.Name, attrs, tok.selfClosing)
				cursor = tok.end
			}

		case xml.EndElement:
			if textFrom >= 0 && depth == textDepth-1 {
				copyUpTo(textFrom)
				xml.EscapeText(&output, []byte(textValue))
				cursor = tok.start
				textFrom = -1
			}
			if depth == 2 && tok.frame.space == "" && tok.frame.local == "item" {
				change, source = ItemChange{}, ""
			}
		}
	}

	copyUpTo(int64(len(data)))
	return output.Bytes(), nil
}

// setAttribute sets a no-namespace attribute, adding it if missing. It
// reports whether anything changed.
func setAttribute(attrs []xml.Attr, local, value string) ([]xml.Attr, bool) {
	result := make([]xml.Attr, len(attrs))
	copy(result, attrs)
	for i, attr := range result {
		if attr.Name.Space == "" && attr.Name.Local == local {
			if attr.Value == value {
				return attrs, false
			}
			result[i].Value = value
			return result, true
		}
	}
	return append(result, xml.Attr{Name: xml.Name{Local: local}, Value: value}), true
}

func hasAttributeValue(attrs []xml.Attr, value string) bool {
	for _, attr := range attrs {
		if attr.Value == value && attr.Name.Space != "xmlns" {
			return true
		}
	}
	return false
}

// replaceAttributeValue replaces every attribute whose value is exactly from.
// Namespace declarations are never touched.
func replaceAttributeValue(attrs []xml.Attr, from, to string) ([]xml.Attr, bool) {
	var result []xml.Attr
	for i, attr := range attrs {
		if attr.Value != from || attr.Name.Space == "xmlns" || (attr.Name.Space == "" && attr.Name.Local == "xmlns") {
			continue
		}
		if result == nil {
			result = make([]xml.Attr, len(attrs))
			copy(result, attrs)
		}
		result[i].Value = to
	}
	if result == nil {
		return attrs, false
	}
	return result, true
}

// writeStart writes a start tag with its prefixes as they were in the source.
func writeStart(output *bytes.Buffer, name xml.Name, attrs []xml.Attr, selfClosing bool) {
	output.WriteByte('<')
	output.WriteString(qualifiedName(name))
	for _, attr := range attrs {
		output.WriteByte(' ')
		output.WriteString(qualifiedName(attr.Name))
		output.WriteString(`="`)
		xml.EscapeText(output, []byte(attr.Value))
		output.WriteByte('"')
	}
	if selfClosing {
		output.WriteString("/>")
	} else {
		output.WriteByte('>')
	}
}

// FormatDuration formats a duration for <itunes:duration> as H:MM:SS, or
// M:SS under an hour, rounded to whole seconds.
func FormatDuration(duration time.Duration) string {
	seconds := int64(duration.Round(time.Second) / time.Second)
	if seconds < 0 {
		seconds = 0
	}
	hours, minutes, secs := seconds/3600, seconds/60%60, seconds%60
	if hours > 0 {
		return fmt.Sprintf("%d:%02d:%02d", hours, minutes, secs)
	}
	return fmt.Sprintf("%d:%02d", minutes, secs)
}

// ParseDuration reads <itunes:duration>: seconds ("2392", "2392.5"), M:SS
// or H:MM:SS. It reports false for anything else, including negative values.
func ParseDuration(text string) (time.Duration, bool) {
	parts := strings.Split(strings.TrimSpace(text), ":")
	if len(parts) > 3 || parts[0] == "" {
		return 0, false
	}
	seconds, err := strconv.ParseFloat(parts[len(parts)-1], 64)
	if err != nil || seconds < 0 || math.IsInf(seconds, 0) || math.IsNaN(seconds) || (len(parts) > 1 && seconds >= 60) {
		return 0, false
	}
	total := seconds
	multiplier := 60.0
	for i := len(parts) - 2; i >= 0; i-- {
		value, err := strconv.Atoi(parts[i])
		if err != nil || value < 0 || (i > 0 && value >= 60) {
			return 0, false
		}
		total += float64(value) * multiplier
		multiplier *= 60
	}
	if total > float64(math.MaxInt64/int64(time.Second)) {
		return 0, false
	}
	return time.Duration(total * float64(time.Second)), true
}
