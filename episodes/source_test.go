package episodes

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

	"aunefyren/solstein/logger"

	"github.com/sirupsen/logrus"
)

// trackerChain serves a chain of tracking redirects in front of an audio
// host, all on one TLS server told apart by host name, as podcast
// enclosures are: tracker.example redirects to its path, dead.example has
// shut down (404), parked.example serves an HTML page, audio.example has
// the episode.
type trackerChain struct {
	server *httptest.Server
	mutex  sync.Mutex
	hits   []string
}

func newTrackerChain(t *testing.T) *trackerChain {
	t.Helper()
	chain := &trackerChain{}
	chain.server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		chain.mutex.Lock()
		chain.hits = append(chain.hits, request.Host)
		chain.mutex.Unlock()
		switch request.Host {
		case "tracker.example":
			next := strings.TrimPrefix(request.URL.Path, "/t/")
			http.Redirect(writer, request, "https://"+next+"?"+request.URL.RawQuery, http.StatusFound)
		case "dead.example":
			http.NotFound(writer, request)
		case "parked.example":
			writer.Header().Set("Content-Type", "text/html")
			writer.Write([]byte("<html>This domain is for sale</html>"))
		case "audio.example":
			if request.URL.Path != "/ep.mp3" || request.URL.Query().Get("updated") != "1" {
				http.NotFound(writer, request)
				return
			}
			writer.Header().Set("Content-Type", "audio/mpeg")
			writer.Write([]byte(audio))
		default:
			http.Error(writer, "unknown host", http.StatusBadGateway)
		}
	}))
	t.Cleanup(chain.server.Close)
	return chain
}

// client sends every host name to the test server.
func (chain *trackerChain) client() *http.Client {
	address := chain.server.Listener.Addr().String()
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	}}
}

func (chain *trackerChain) visited() []string {
	chain.mutex.Lock()
	defer chain.mutex.Unlock()
	return append([]string(nil), chain.hits...)
}

func captureLogs(t *testing.T) *strings.Builder {
	t.Helper()
	original := logger.Log
	t.Cleanup(func() { logger.Log = original })
	logger.Log = logrus.New()
	var output strings.Builder
	logger.Log.SetOutput(&output)
	return &output
}

func fetchBody(t *testing.T, chain *trackerChain, sourceURL string, skip bool) (string, error) {
	t.Helper()
	response, err := requestSource(context.Background(), chain.client(), http.MethodGet, sourceURL, skip, func(*http.Request) {}, checkResponse)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return string(body), nil
}

func TestRequestSourceSkipsFailedTracker(t *testing.T) {
	for _, dead := range []string{"dead.example", "parked.example"} {
		t.Run(dead, func(t *testing.T) {
			logs := captureLogs(t)
			chain := newTrackerChain(t)
			body, err := fetchBody(t, chain, "https://tracker.example/t/"+dead+"/track/42/audio.example/ep.mp3?updated=1", false)
			if err != nil || body != audio {
				t.Fatalf("got %q, %v", body, err)
			}
			if got := strings.Join(chain.visited(), " "); got != "tracker.example "+dead+" audio.example" {
				t.Errorf("visited %s", got)
			}
			if !strings.Contains(logs.String(), "Tracking redirect at "+dead+" failed") || strings.Contains(logs.String(), "updated=1") {
				t.Errorf("log %q: want the skip noted, without the query", logs.String())
			}
		})
	}
}

func TestRequestSourceCanSkipAllTrackers(t *testing.T) {
	chain := newTrackerChain(t)
	body, err := fetchBody(t, chain, "https://tracker.example/t/dead.example/track/42/audio.example/ep.mp3?updated=1", true)
	if err != nil || body != audio {
		t.Fatalf("got %q, %v", body, err)
	}
	if got := chain.visited(); len(got) != 1 || got[0] != "audio.example" {
		t.Errorf("visited %v, want the audio host only", got)
	}
}

func TestRequestSourceFailsAtTheAudioHost(t *testing.T) {
	chain := newTrackerChain(t)
	// The episode itself is gone: nothing embedded to try.
	_, err := fetchBody(t, chain, "https://tracker.example/t/audio.example/missing.mp3?updated=1", false)
	if !errors.Is(err, ErrPermanent) || !strings.Contains(err.Error(), "404") {
		t.Errorf("err = %v, want a permanent 404", err)
	}
	// Every tracker dead and the audio host too: the last error, naming
	// what was skipped.
	_, err = fetchBody(t, chain, "https://dead.example/track/audio.example/missing.mp3?updated=1", false)
	if !errors.Is(err, ErrPermanent) || !strings.Contains(err.Error(), "after skipping the tracking redirects at dead.example") {
		t.Errorf("err = %v", err)
	}
	// Each host asked once: an error status is no reason to ask again.
	if got := strings.Join(chain.visited(), " "); got != "tracker.example audio.example dead.example audio.example" {
		t.Errorf("visited %s", got)
	}
}
