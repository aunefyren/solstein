// Package rss parses podcast RSS feeds and rewrites them for serving through
// Solstein. It knows nothing about the rest of Solstein.
//
// Rewriting works on the original bytes: everything is copied through
// unchanged except the few values Solstein must change (enclosure URLs,
// lengths, durations, self-links), and items it leaves out. Re-encoding with
// encoding/xml is not an option: it rewrites namespace prefixes, and clients
// such as Audiobookshelf look elements up by their literal prefix
// ("itunes:new-feed-url"). Copying bytes also keeps CDATA sections, entities
// and formatting exactly as the source had them.
package rss

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"golang.org/x/text/encoding/charmap"
)

// Namespaces Solstein reads or rewrites. Elements are matched by namespace,
// not by prefix, so a feed that binds iTunes to another prefix still works.
const (
	itunesNS = "http://www.itunes.com/dtds/podcast-1.0.dtd"
	atomNS   = "http://www.w3.org/2005/Atom"
	mediaNS  = "http://search.yahoo.com/mrss/"
	xmlNS    = "http://www.w3.org/XML/1998/namespace"
)

var (
	ErrNotRSS              = errors.New("not an RSS feed")
	ErrUnsupportedEncoding = errors.New("unsupported character encoding")
)

// frame is one open element during a walk.
type frame struct {
	raw        xml.Name          // as written: Space is the prefix
	space      string            // resolved namespace URI
	local      string            //
	namespaces map[string]string // prefixes declared on this element
}

// walker streams raw tokens with their byte offsets, resolving namespaces
// and checking that tags nest properly (RawToken itself doesn't).
type walker struct {
	data    []byte
	decoder *xml.Decoder
	stack   []frame
	offset  int64 // end of the previous token
}

// token is one step of a walk. For an end tag, frame is the element being
// closed. start and end are byte offsets of the token in the input; a
// self-closing tag's end token is empty (start == end).
type token struct {
	value       xml.Token
	frame       frame
	start, end  int64
	selfClosing bool
}

func newWalker(data []byte) *walker {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	// Real feeds contain HTML entities (&nbsp;) outside CDATA. Tag nesting is
	// still checked by the walker.
	decoder.Strict = false
	decoder.Entity = xml.HTMLEntity
	return &walker{data: data, decoder: decoder}
}

// next returns the next token, or io.EOF at the end of the document.
func (walker *walker) next() (token, error) {
	value, err := walker.decoder.RawToken()
	if err == io.EOF {
		if len(walker.stack) > 0 {
			return token{}, fmt.Errorf("parse feed: element <%s> is never closed", qualifiedName(walker.stack[len(walker.stack)-1].raw))
		}
		return token{}, io.EOF
	}
	if err != nil {
		return token{}, fmt.Errorf("parse feed: %w", err)
	}

	result := token{value: xml.CopyToken(value), start: walker.offset, end: walker.decoder.InputOffset()}
	walker.offset = result.end

	switch element := result.value.(type) {
	case xml.StartElement:
		f := frame{raw: element.Name, namespaces: map[string]string{}}
		for _, attr := range element.Attr {
			switch {
			case attr.Name.Space == "xmlns":
				f.namespaces[attr.Name.Local] = attr.Value
			case attr.Name.Space == "" && attr.Name.Local == "xmlns":
				f.namespaces[""] = attr.Value
			}
		}
		walker.stack = append(walker.stack, f)
		f.space = walker.resolve(element.Name.Space)
		f.local = element.Name.Local
		walker.stack[len(walker.stack)-1] = f
		result.frame = f
		result.selfClosing = bytes.HasSuffix(bytes.TrimRight(walker.data[result.start:result.end], " \t\r\n"), []byte("/>"))
	case xml.EndElement:
		if len(walker.stack) == 0 {
			return token{}, fmt.Errorf("parse feed: unexpected </%s>", qualifiedName(element.Name))
		}
		top := walker.stack[len(walker.stack)-1]
		if top.raw != element.Name {
			return token{}, fmt.Errorf("parse feed: </%s> closes <%s>", qualifiedName(element.Name), qualifiedName(top.raw))
		}
		walker.stack = walker.stack[:len(walker.stack)-1]
		result.frame = top
	}
	return result, nil
}

// resolve maps a prefix to its namespace URI using the open elements.
func (walker *walker) resolve(prefix string) string {
	if prefix == "xml" {
		return xmlNS
	}
	for i := len(walker.stack) - 1; i >= 0; i-- {
		if uri, ok := walker.stack[i].namespaces[prefix]; ok {
			return uri
		}
	}
	return ""
}

// depth is the number of open elements, including one just started.
func (walker *walker) depth() int {
	return len(walker.stack)
}

// is reports whether the open element at index i has the given namespace
// and local name.
func (walker *walker) is(i int, space, local string) bool {
	return i < len(walker.stack) && walker.stack[i].space == space && walker.stack[i].local == local
}

func qualifiedName(name xml.Name) string {
	if name.Space == "" {
		return name.Local
	}
	return name.Space + ":" + name.Local
}

var encodingDeclaration = regexp.MustCompile(`^(<\?xml[^>]*?encoding\s*=\s*["'])([^"']+)(["'])`)

// normalise returns the document as UTF-8 without a byte-order mark,
// converting single-byte encodings some feeds still use and updating the
// XML declaration to match.
func normalise(data []byte) ([]byte, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))

	match := encodingDeclaration.FindSubmatchIndex(data)
	if match == nil {
		return data, nil
	}
	encoding := strings.ToLower(string(data[match[4]:match[5]]))

	var decoder *charmap.Charmap
	switch encoding {
	case "utf-8", "utf8":
		return data, nil
	case "iso-8859-1", "latin1", "latin-1":
		decoder = charmap.ISO8859_1
	case "iso-8859-15":
		decoder = charmap.ISO8859_15
	case "windows-1252", "cp1252":
		decoder = charmap.Windows1252
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedEncoding, encoding)
	}

	converted, err := decoder.NewDecoder().Bytes(data)
	if err != nil {
		return nil, fmt.Errorf("convert from %s: %w", encoding, err)
	}
	// The declaration is ASCII, so its offsets are unchanged by conversion.
	var result bytes.Buffer
	result.Write(converted[:match[4]])
	result.WriteString("UTF-8")
	result.Write(converted[match[5]:])
	return result.Bytes(), nil
}
