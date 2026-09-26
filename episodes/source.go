package episodes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"aunefyren/solstein/feeds"
	"aunefyren/solstein/logger"
	"aunefyren/solstein/outbound"
)

// maxSkippedTrackers bounds how many dead tracking redirects one request
// steps over.
const maxSkippedTrackers = 8

// requestSource asks for an episode's audio at sourceURL, following its
// redirects, and returns the response once accept passes it; the caller
// closes the body. prepare sets the request's headers.
//
// An episode's URL is often a chain of tracking redirects, each carrying
// the next URL in its path (see feeds.EmbeddedURL). When one of them fails
// — an error status, something that isn't audio, no response — the request
// is sent again to the URL it would have redirected to, so a tracker that
// has shut down (Chartable's chrt.fm) doesn't make every episode behind it
// fail. With skipTrackers, every prefix is skipped from the start.
//
// A request that gets no response at all is first tried once more at the
// same URL, on a new connection (see requestOnce).
func requestSource(ctx context.Context, client *http.Client, method, sourceURL string, skipTrackers bool, prepare func(*http.Request), accept func(*http.Response) error) (*http.Response, error) {
	target := sourceURL
	if skipTrackers {
		target = feeds.WithoutTrackers(sourceURL)
	}
	var skipped []string
	for {
		response, failedAt, err := requestOnce(ctx, client, method, target, prepare)
		if err == nil {
			if err = accept(response); err == nil {
				return response, nil
			}
			response.Body.Close()
		}
		if errors.Is(err, outbound.ErrDestinationBlocked) || ctx.Err() != nil {
			return nil, err
		}
		next, ok := feeds.EmbeddedURL(failedAt)
		if !ok || len(skipped) >= maxSkippedTrackers {
			if len(skipped) > 0 {
				err = fmt.Errorf("%w (after skipping the tracking redirects at %s)", err, strings.Join(skipped, ", "))
			}
			return nil, err
		}
		failedHost := hostOf(failedAt)
		logger.Log.Info(fmt.Sprintf("Tracking redirect at %s failed (%s); asking %s directly.", failedHost, withoutURLs(err), hostOf(next)))
		skipped = append(skipped, failedHost)
		target = next
	}
}

// requestOnce sends one request, and reports the URL that answered (or
// failed): the last of the redirects followed.
//
// A request that gets no response — a connection dropped (seen through a
// VPN tunnel as a bare EOF), no headers in time — is tried once more after
// fetchRetryDelay. Nothing has been written anywhere yet, so this is safe.
// The retry must not go out on the connection that just failed (an HTTP/2
// connection to a host is shared by every request to it), so idle
// connections are closed first.
func requestOnce(ctx context.Context, client *http.Client, method, target string, prepare func(*http.Request)) (*http.Response, string, error) {
	send := func() (*http.Response, error) {
		request, err := http.NewRequestWithContext(ctx, method, target, nil)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrPermanent, err)
		}
		prepare(request)
		return client.Do(request)
	}
	response, err := send()
	if err != nil && !errors.Is(err, outbound.ErrDestinationBlocked) && !errors.Is(err, ErrPermanent) && ctx.Err() == nil {
		client.CloseIdleConnections()
		select {
		case <-time.After(fetchRetryDelay):
			response, err = send()
		case <-ctx.Done():
		}
	}
	if err != nil {
		if errors.Is(err, outbound.ErrDestinationBlocked) {
			err = fmt.Errorf("%w: %w", ErrPermanent, err)
		}
		failedAt := target
		var urlErr *url.Error
		if errors.As(err, &urlErr) && urlErr.URL != "" {
			failedAt = urlErr.URL
		}
		return nil, failedAt, err
	}
	return response, response.Request.URL.String(), nil
}

func hostOf(raw string) string {
	if parsed, err := url.Parse(raw); err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return "?"
}

// withoutURLs shortens an error for the log: Go's HTTP errors quote the
// whole URL, whose query may carry tokens.
func withoutURLs(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err.Error()
	}
	return err.Error()
}
