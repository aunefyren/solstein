package feeds

import (
	"net/url"
	"strings"
)

// Tracking prefixes. Podcast hosts and measurement services chain in front
// of an episode's real URL by putting it in their own URL's path:
//
//	https://www.podtrac.com/pts/redirect.mp3/pdst.fm/e/chrt.fm/track/9DD8D/traffic.megaphone.fm/TPC1.mp3?updated=1
//
// Each redirects to the rest of its path, so the audio host is reached
// through every tracker in turn. When one of them is gone (Chartable's
// chrt.fm answers 404 since it shut down), every download through the
// chain fails, although the audio is still there at the end of it.

// maxTrackers bounds how many prefixes are unwrapped from one URL.
const maxTrackers = 16

// EmbeddedURL returns the URL a tracking prefix redirects to, taken from its
// path: from the first path segment that is a host name on, with the
// prefix's query string. It reports false when the path holds no host name
// (an audio host's own URL: "/TPC1.mp3", "redirect.mp3" and the like are
// file names, not hosts).
func EmbeddedURL(raw string) (string, bool) {
	prefix, err := url.Parse(raw)
	if err != nil || (prefix.Scheme != "http" && prefix.Scheme != "https") {
		return "", false
	}
	segments := strings.Split(strings.TrimPrefix(prefix.EscapedPath(), "/"), "/")
	// The first segment belongs to the prefix itself.
	for i := 1; i < len(segments); i++ {
		scheme, rest := "https", segments[i:]
		// Some prefixes embed the whole URL, scheme included:
		// ".../redirect.mp3/https://host/..." ("https:/host" once collapsed).
		if segment := segments[i]; segment == "https:" || segment == "http:" {
			scheme, rest = strings.TrimSuffix(segment, ":"), rest[1:]
			for len(rest) > 0 && rest[0] == "" {
				rest = rest[1:]
			}
		}
		if len(rest) == 0 || !isHostName(rest[0]) {
			continue
		}
		embedded := scheme + "://" + strings.Join(rest, "/")
		if prefix.RawQuery != "" {
			embedded += "?" + prefix.RawQuery
		}
		if _, err := url.Parse(embedded); err != nil {
			return "", false
		}
		return embedded, true
	}
	return "", false
}

// WithoutTrackers unwraps every tracking prefix in front of an episode's
// URL, leaving the audio host's own.
func WithoutTrackers(raw string) string {
	for range maxTrackers {
		next, ok := EmbeddedURL(raw)
		if !ok {
			break
		}
		raw = next
	}
	return raw
}

// isHostName reports whether a path segment is a domain name such as
// "pdst.fm" or "traffic.megaphone.fm": dot-separated labels of letters,
// digits and hyphens, ending in a top-level domain of letters only. That
// rules out file names (redirect.mp3, episode.m4a), whose last part has a
// digit or is a known audio extension.
func isHostName(segment string) bool {
	labels := strings.Split(segment, ".")
	if len(labels) < 2 || len(segment) > 253 {
		return false
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	tld := strings.ToLower(labels[len(labels)-1])
	if len(tld) < 2 {
		return false
	}
	for _, r := range tld {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	switch tld {
	case "mp3", "aac", "ogg", "opus", "wav", "flac", "html", "htm", "php", "xml", "json", "js", "css":
		return false
	}
	return true
}
