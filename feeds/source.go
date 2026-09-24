package feeds

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

var (
	ErrInvalidSourceURL = errors.New("invalid source feed URL")
	ErrSourceNotAllowed = errors.New("source host is not in allowed_source_hosts")
)

// SourceFromPrefixPath rebuilds a source feed URL from what follows
// /api/rss/{token}/ in a request. rest must be the raw (still escaped) path
// and rawQuery the raw query string, so the source's own escaping and query
// parameters survive untouched.
//
// Reverse proxies such as nginx collapse "//" by default, so "https:/host"
// is accepted as well as "https://host", and a missing scheme means https.
func SourceFromPrefixPath(rest, rawQuery string) (string, error) {
	rest = strings.TrimLeft(rest, "/")
	scheme := "https"
	lower := strings.ToLower(rest)
	for _, candidate := range []string{"https:", "http:"} {
		if strings.HasPrefix(lower, candidate) {
			scheme = strings.TrimSuffix(candidate, ":")
			rest = strings.TrimLeft(rest[len(candidate):], "/")
			break
		}
	}
	if rest == "" {
		return "", fmt.Errorf("%w: no host", ErrInvalidSourceURL)
	}
	// "ftp://host" would otherwise be read as a host called "ftp:".
	if firstSegment, _, _ := strings.Cut(rest, "/"); strings.HasSuffix(firstSegment, ":") {
		return "", fmt.Errorf("%w: scheme must be http or https", ErrInvalidSourceURL)
	}
	source := scheme + "://" + rest
	if rawQuery != "" {
		source += "?" + rawQuery
	}
	return NormaliseSourceURL(source)
}

// NormaliseSourceURL checks a source feed URL and puts it in canonical form,
// so the same feed added twice maps to one feed record: scheme and host
// lower-cased, default port and fragment dropped. Path and query are kept as
// written, since servers may treat them case-sensitively.
func NormaliseSourceURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidSourceURL, err)
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("%w: scheme must be http or https", ErrInvalidSourceURL)
	}
	if parsed.User != nil {
		// Credentials in the URL would end up stored and logged.
		return "", fmt.Errorf("%w: credentials in the URL are not supported", ErrInvalidSourceURL)
	}

	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return "", fmt.Errorf("%w: no host", ErrInvalidSourceURL)
	}
	port := parsed.Port()
	if (parsed.Scheme == "http" && port == "80") || (parsed.Scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]" // IPv6 literal
	}
	parsed.Host = host
	parsed.Fragment = ""
	parsed.RawFragment = ""
	if parsed.Path == "" {
		parsed.Path = "/"
	}
	return parsed.String(), nil
}

// hostAllowed reports whether a source URL's host is allowed. An entry
// allows itself and its subdomains; an empty list allows everything.
func hostAllowed(sourceURL string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	parsed, err := url.Parse(sourceURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	for _, entry := range allowed {
		if host == entry || strings.HasSuffix(host, "."+entry) {
			return true
		}
	}
	return false
}
