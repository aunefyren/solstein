package feeds

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"aunefyren/solstein/models"
)

func TestEmbeddedURL(t *testing.T) {
	cases := []struct {
		raw, want string
	}{
		// The Always Sunny chain, hop by hop.
		{"https://www.podtrac.com/pts/redirect.mp3/pdst.fm/e/pfx.vpixl.com/usvBQ/traffic.megaphone.fm/TPC1.mp3?updated=1688948785",
			"https://pdst.fm/e/pfx.vpixl.com/usvBQ/traffic.megaphone.fm/TPC1.mp3?updated=1688948785"},
		{"https://chrt.fm/track/9DD8D/arttrk.com/p/PRGN3/traffic.megaphone.fm/TPC1.mp3?updated=1",
			"https://arttrk.com/p/PRGN3/traffic.megaphone.fm/TPC1.mp3?updated=1"},
		{"https://arttrk.com/p/PRGN3/traffic.megaphone.fm/TPC1.mp3", "https://traffic.megaphone.fm/TPC1.mp3"},
		// Darknet Diaries: Podtrac in front of Dovetail.
		{"https://www.podtrac.com/pts/redirect.mp3/dovetail.prxu.org/7057/9d4c/darknet-diaries-ep169-mod.mp3",
			"https://dovetail.prxu.org/7057/9d4c/darknet-diaries-ep169-mod.mp3"},
		// A scheme in the path, whole or collapsed by a proxy.
		{"https://dts.podtrac.com/redirect.mp3/http://cdn.example.com/a.mp3", "http://cdn.example.com/a.mp3"},
		{"https://dts.podtrac.com/redirect.mp3/https:/cdn.example.com/a.mp3", "https://cdn.example.com/a.mp3"},
		// Audio hosts' own URLs: nothing embedded.
		{"https://traffic.megaphone.fm/TPC1.mp3?updated=1", ""},
		{"https://dovetail.prxu.org/7057/9d4c/darknet-diaries-ep169-mod.mp3", ""},
		{"https://sphinx.acast.com/p/open/s/5f1c/e/6a2b/media.mp3", ""},
		{"https://audio4.redcircle.com/episodes/1cfe3643/stream.mp3", ""},
		{"https://cdn.example.com/show/episode.m4a", ""},
		{"https://cdn.example.com/1.0.2/episode.mp3", ""}, // a version number isn't a host
		{"ftp://www.podtrac.com/pts/redirect.mp3/cdn.example.com/a.mp3", ""},
		{"not a url\x7f", ""},
	}
	for _, c := range cases {
		got, ok := EmbeddedURL(c.raw)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("EmbeddedURL(%q) = %q, %v; want %q", c.raw, got, ok, c.want)
		}
	}
}

func TestWithoutTrackers(t *testing.T) {
	chain := "https://www.podtrac.com/pts/redirect.mp3/pdst.fm/e/pfx.vpixl.com/usvBQ/verifi.podscribe.com/rss/p/claritaspod.com/measure/chrt.fm/track/9DD8D/arttrk.com/p/PRGN3/traffic.megaphone.fm/TPC6209325126.mp3?updated=1688948785"
	if got, want := WithoutTrackers(chain), "https://traffic.megaphone.fm/TPC6209325126.mp3?updated=1688948785"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if plain := "https://sphinx.acast.com/p/open/s/1/e/2/media.mp3"; WithoutTrackers(plain) != plain {
		t.Error("an audio host's own URL changed")
	}
}

// feedChain serves a feed behind tracking prefixes, one TLS server for every
// host name: tracker.example redirects to its path, dead.example is gone,
// parked.example shows an HTML page, feeds.example has the feed.
func feedChain(t *testing.T) (*http.Client, *[]string) {
	t.Helper()
	var mutex sync.Mutex
	var hits []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mutex.Lock()
		hits = append(hits, request.Host)
		mutex.Unlock()
		switch request.Host {
		case "tracker.example":
			http.Redirect(writer, request, "https://"+strings.TrimPrefix(request.URL.Path, "/t/"), http.StatusFound)
		case "dead.example":
			http.NotFound(writer, request)
		case "parked.example":
			writer.Header().Set("Content-Type", "text/html")
			writer.Write([]byte("<html>for sale</html>"))
		case "feeds.example":
			if request.Header.Get("If-None-Match") == `"v1"` {
				writer.WriteHeader(http.StatusNotModified)
				return
			}
			writer.Header().Set("Content-Type", "application/rss+xml")
			writer.Write([]byte("<rss/>"))
		default:
			http.Error(writer, "unknown host", http.StatusBadGateway)
		}
	}))
	t.Cleanup(server.Close)
	address := server.Listener.Addr().String()
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	}}
	return client, &hits
}

func TestRequestFeedSkipsFailedTracker(t *testing.T) {
	for _, dead := range []string{"dead.example", "parked.example"} {
		client, hits := feedChain(t)
		feed := models.Feed{Title: "Show", SourceURL: "https://tracker.example/t/" + dead + "/ce1080/feeds.example/show.xml"}
		response, err := requestFeed(context.Background(), client, feed)
		if err != nil {
			t.Fatalf("%s: %v", dead, err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if string(body) != "<rss/>" || strings.Join(*hits, " ") != "tracker.example "+dead+" feeds.example" {
			t.Errorf("%s: got %q via %v", dead, body, *hits)
		}
	}

	// Conditional requests still work past the skipped tracker.
	client, _ := feedChain(t)
	feed := models.Feed{SourceURL: "https://dead.example/ce1080/feeds.example/show.xml", ETag: `"v1"`}
	if response, err := requestFeed(context.Background(), client, feed); err != nil || response.StatusCode != http.StatusNotModified {
		t.Errorf("conditional: %v, %v", response, err)
	}

	// The feed host's own failure is the feed's.
	client, _ = feedChain(t)
	if _, err := requestFeed(context.Background(), client, models.Feed{SourceURL: "https://dead.example/show.xml"}); !errors.Is(err, ErrFetchFailed) || !strings.Contains(err.Error(), "404") {
		t.Errorf("gone: %v", err)
	}
}
