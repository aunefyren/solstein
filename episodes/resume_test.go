package episodes

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aunefyren/solstein/outbound"
)

// longAudio is big enough to break partway through.
var longAudio = bytes.Repeat([]byte("0123456789abcdef"), 4096) // 64 KB

// breakAfter sends the headers for the whole file, then n bytes of it, then
// drops the connection, as a connection through a VPN was seen to die.
func breakAfter(writer http.ResponseWriter, data []byte, n int) {
	writer.Header().Set("Content-Length", strconv.Itoa(len(data)))
	writer.Write(data[:n])
	writer.(http.Flusher).Flush()
	panic(http.ErrAbortHandler)
}

// newResumeSetup is newTestSetup with an idle timeout longer than the pause
// before a resume, as in production (2 minutes against half a second).
func newResumeSetup(t *testing.T) *testSetup {
	t.Helper()
	setup := newTestSetup(t)
	setup.pipeline.options.IdleTimeout = 10 * time.Second
	return setup
}

// startBreakingHost serves longAudio at paths whose first response breaks
// halfway, and counts the requests with a Range header.
func startBreakingHost(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var ranged atomic.Int32
	requests := map[string]*atomic.Int32{}
	for _, path := range []string{"/resumes.mp3", "/changes.mp3", "/ignores-range.mp3", "/always-breaks.mp3", "/unknown-length.mp3"} {
		requests[path] = &atomic.Int32{}
	}
	host := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		count := requests[request.URL.Path]
		if count == nil {
			http.NotFound(writer, request)
			return
		}
		first := count.Add(1) == 1
		if request.Header.Get("Range") != "" {
			ranged.Add(1)
		}
		writer.Header().Set("Content-Type", "audio/mpeg")
		switch path := request.URL.Path; {
		case path == "/unknown-length.mp3" && first:
			writer.Write(longAudio[:1000]) // chunked: no length to resume against
			writer.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		case path == "/always-breaks.mp3" && first:
			breakAfter(writer, longAudio, 1000)
		case path == "/always-breaks.mp3":
			// The rest, as asked, breaking again after a little of it.
			start, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(request.Header.Get("Range"), "bytes="), "-"))
			writer.Header().Set("Content-Range", "bytes "+strconv.Itoa(start)+"-"+strconv.Itoa(len(longAudio)-1)+"/"+strconv.Itoa(len(longAudio)))
			writer.Header().Set("Content-Length", strconv.Itoa(len(longAudio)-start))
			writer.WriteHeader(http.StatusPartialContent)
			writer.Write(longAudio[start : start+1000])
			writer.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		case first:
			writer.Header().Set("ETag", `"v1"`)
			breakAfter(writer, longAudio, len(longAudio)/2)
		case path == "/changes.mp3":
			writer.Header().Set("ETag", `"v2"`) // If-Range no longer matches: the whole new file
			http.ServeContent(writer, request, "", time.Time{}, bytes.NewReader(longAudio))
		case path == "/ignores-range.mp3":
			writer.Write(longAudio)
		default:
			writer.Header().Set("ETag", `"v1"`)
			http.ServeContent(writer, request, "", time.Time{}, bytes.NewReader(longAudio))
		}
	}))
	t.Cleanup(host.Close)
	return host, &ranged
}

func TestDownloadResumesAfterBrokenConnection(t *testing.T) {
	setup := newResumeSetup(t)
	host, ranged := startBreakingHost(t)
	download, err := setup.pipeline.fetchForJob(host.URL+"/resumes.mp3")(context.Background(), outbound.DirectExit, false)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !bytes.Equal(download.Data, longAudio) {
		t.Errorf("got %d bytes, want the whole %d-byte file", len(download.Data), len(longAudio))
	}
	if ranged.Load() != 1 {
		t.Errorf("%d range requests, want 1", ranged.Load())
	}
}

func TestDownloadResumeRefusals(t *testing.T) {
	cases := map[string]string{
		"/changes.mp3":        "can't resume",
		"/ignores-range.mp3":  "can't resume",
		"/always-breaks.mp3":  "unexpected EOF",
		"/unknown-length.mp3": "unexpected EOF",
	}
	for path, want := range cases {
		t.Run(strings.TrimPrefix(path, "/"), func(t *testing.T) {
			setup := newResumeSetup(t)
			host, ranged := startBreakingHost(t)
			_, err := setup.pipeline.fetchForJob(host.URL+path)(context.Background(), outbound.DirectExit, false)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("err = %v, want one mentioning %q", err, want)
			}
			switch path {
			case "/always-breaks.mp3":
				if ranged.Load() != maxResumes {
					t.Errorf("%d resumes, want %d", ranged.Load(), maxResumes)
				}
			case "/unknown-length.mp3":
				if ranged.Load() != 0 {
					t.Errorf("resumed a download of unknown length")
				}
			}
		})
	}
}

func TestDownloadIntoCacheResumes(t *testing.T) {
	setup := newResumeSetup(t)
	host, _ := startBreakingHost(t)
	episode := setup.addEpisode(t, "/ok.mp3")
	episode.SourceURL = host.URL + "/resumes.mp3"
	if _, err := setup.pipeline.download(context.Background(), setup.feed, episode); err != nil {
		t.Fatalf("download: %v", err)
	}
	assertNoPartFiles(t, setup.cache)
}

func TestParseContentRange(t *testing.T) {
	cases := []struct {
		value        string
		start, total int64
		ok           bool
	}{
		{"bytes 100-199/200", 100, 200, true},
		{"bytes 0-0/1", 0, 1, true},
		{"bytes 100-199/*", 0, 0, false},
		{"bytes */200", 0, 0, false},
		{"items 1-2/3", 0, 0, false},
		{"bytes x-199/200", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, c := range cases {
		start, total, ok := parseContentRange(c.value)
		if ok != c.ok || ok && (start != c.start || total != c.total) {
			t.Errorf("parseContentRange(%q) = %d, %d, %v", c.value, start, total, ok)
		}
	}
}
