package episodes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"aunefyren/solstein/database"
	"aunefyren/solstein/feeds"
	"aunefyren/solstein/models"
	"aunefyren/solstein/outbound"
)

// fakeProcessor fetches the source through "direct" and returns it
// upper-cased, or fails with err. With gate set, each job waits for a value
// on it first.
type fakeProcessor struct {
	mutex sync.Mutex
	err   error
	hide  bool
	gate  chan struct{}
	jobs  []Job
	// recipe stands for the processor's settings.
	recipe string
}

func (processor *fakeProcessor) setErr(err error) {
	processor.mutex.Lock()
	defer processor.mutex.Unlock()
	processor.err = err
}

func (processor *fakeProcessor) jobCount() int {
	processor.mutex.Lock()
	defer processor.mutex.Unlock()
	return len(processor.jobs)
}

func (processor *fakeProcessor) Name() string                        { return "fake" }
func (processor *fakeProcessor) Handles(feed models.Feed) bool       { return feed.Title == "Processed" }
func (processor *fakeProcessor) HideOnFailure(feed models.Feed) bool { return processor.hide }
func (processor *fakeProcessor) Recipe(feed models.Feed) string {
	processor.mutex.Lock()
	defer processor.mutex.Unlock()
	return "shout " + processor.recipe
}

func (processor *fakeProcessor) Process(ctx context.Context, job Job) (Processed, error) {
	processor.mutex.Lock()
	processor.jobs = append(processor.jobs, job)
	err, gate := processor.err, processor.gate
	processor.mutex.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return Processed{}, ctx.Err()
		}
	}
	if err != nil {
		return Processed{}, err
	}
	download, err := job.Fetch(ctx, outbound.DirectExit)
	if err != nil {
		return Processed{}, err
	}
	return Processed{Audio: []byte(strings.ToUpper(string(download.Data))), ContentType: download.ContentType, Duration: 90 * time.Second, Note: "shouted"}, nil
}

// withProcessor rebuilds the setup's pipeline with a processor, and makes
// its feed one the processor handles, in stream mode: processing must not
// depend on cache delivery.
func (setup *testSetup) withProcessor(t *testing.T, processor Processor) {
	t.Helper()
	exits, err := outbound.New(outbound.Options{AllowPrivateDestinations: true})
	if err != nil {
		t.Fatal(err)
	}
	setup.pipeline = NewPipeline(setup.store, exits, setup.cache, Options{DefaultDeliveryMode: "cache", Processor: processor, IdleTimeout: 200 * time.Millisecond, Now: setup.clock.Now})
	setup.feed.Title, setup.feed.DeliveryMode = "Processed", "stream"
	if err := setup.store.UpdateFeed(context.Background(), &setup.feed); err != nil {
		t.Fatal(err)
	}
}

func TestPipelineRunsProcessor(t *testing.T) {
	setup := newTestSetup(t)
	processor := &fakeProcessor{}
	setup.withProcessor(t, processor)
	episode := setup.addEpisode(t, "/ok.mp3")
	episode.SourceSeconds = 125
	if err := setup.store.UpdateEpisode(context.Background(), &episode); err != nil {
		t.Fatal(err)
	}

	if !setup.processOne(t) {
		t.Fatal("the processed stream-mode feed's episode wasn't claimed")
	}
	stored := setup.reload(t, episode)
	if stored.State != models.EpisodeReady || stored.CacheSize != int64(len(audio)) || stored.CacheSeconds != 90 || stored.ProcessNote != "shouted" {
		t.Fatalf("episode = %+v", stored)
	}
	fullPath, _ := setup.cache.Path(stored.CacheFile)
	if data, err := os.ReadFile(fullPath); err != nil || string(data) != strings.ToUpper(audio) {
		t.Errorf("cached %q, %v; want the processor's output", data, err)
	}
	if !strings.HasSuffix(stored.CacheFile, ".mp3") {
		t.Errorf("cache file %q", stored.CacheFile)
	}
	if len(processor.jobs) != 1 || processor.jobs[0].ExpectedDuration != 125*time.Second || processor.jobs[0].Feed.ID != setup.feed.ID {
		t.Errorf("jobs = %+v", processor.jobs)
	}
	assertNoPartFiles(t, setup.cache)
}

func TestProcessorFetchHasDownloadChecks(t *testing.T) {
	setup := newTestSetup(t)
	fetch := setup.pipeline.fetchForJob(setup.host.URL + "/html.mp3")
	if _, err := fetch(context.Background(), outbound.DirectExit); !errors.Is(err, ErrPermanent) {
		t.Errorf("HTML page: err = %v, want permanent", err)
	}
	fetch = setup.pipeline.fetchForJob(setup.host.URL + "/truncated.mp3")
	if _, err := fetch(context.Background(), outbound.DirectExit); err == nil || errors.Is(err, ErrPermanent) {
		t.Errorf("short body: err = %v, want a retryable error", err)
	}
	fetch = setup.pipeline.fetchForJob(setup.host.URL + "/ok.mp3")
	if _, err := fetch(context.Background(), "nowhere"); !errors.Is(err, ErrPermanent) || !errors.Is(err, outbound.ErrUnknownExit) {
		t.Errorf("unknown exit: err = %v", err)
	}
	download, err := fetch(context.Background(), outbound.DirectExit)
	if err != nil || string(download.Data) != audio || download.ContentType != "audio/mpeg" {
		t.Errorf("download = %q %q, %v", download.Data, download.ContentType, err)
	}
}

func TestProcessorFailurePolicy(t *testing.T) {
	for _, hide := range []bool{false, true} {
		t.Run(fmt.Sprintf("hide=%v", hide), func(t *testing.T) {
			setup := newTestSetup(t)
			setup.withProcessor(t, &fakeProcessor{err: fmt.Errorf("%w: can't process this", ErrPermanent), hide: hide})
			episode := setup.addEpisode(t, "/ok.mp3")
			setup.processOne(t)
			stored := setup.reload(t, episode)
			if stored.State != models.EpisodeFailed || stored.Withheld != hide || stored.CacheFile != "" {
				t.Fatalf("episode = %+v", stored)
			}

			server := newTestServer(t, setup)
			_, err := serve(t, server, stored, http.MethodGet, "")
			if hide && !errors.Is(err, database.ErrEpisodeNotFound) {
				t.Errorf("withheld episode served: err = %v", err)
			}
			if !hide && err != nil {
				t.Errorf("episode published unprocessed wasn't served: %v", err)
			}
		})
	}
}

func TestProcessorTemporaryFailureRetries(t *testing.T) {
	setup := newTestSetup(t)
	processor := &fakeProcessor{err: errors.New("tunnel down")}
	setup.withProcessor(t, processor)
	episode := setup.addEpisode(t, "/ok.mp3")
	setup.processOne(t)
	stored := setup.reload(t, episode)
	if stored.State != models.EpisodeDiscovered || stored.NextAttemptAt == nil || stored.Withheld {
		t.Fatalf("episode = %+v, want a retry", stored)
	}
	processor.setErr(nil)
	setup.clock.advance(retryDelays[0])
	setup.processOne(t)
	if stored = setup.reload(t, episode); stored.State != models.EpisodeReady || stored.LastError != "" {
		t.Errorf("episode = %+v after the retry", stored)
	}
}

// For a processed feed, a stream must never put the unprocessed audio in
// the cache in place of the processed file.
func TestServeProcessedFeedDoesNotCacheUnprocessed(t *testing.T) {
	setup := newTestSetup(t)
	setup.feed.Title = "Processed"
	if err := setup.store.UpdateFeed(context.Background(), &setup.feed); err != nil {
		t.Fatal(err)
	}
	exits, err := outbound.New(outbound.Options{AllowPrivateDestinations: true})
	if err != nil {
		t.Fatal(err)
	}
	processor := &fakeProcessor{}
	feedService := feeds.New(setup.store, exits, feeds.Options{DefaultDeliveryMode: "cache", Processed: processor.Handles})
	server := NewServer(setup.store, exits, setup.cache, feedService, nil, Options{Now: setup.clock.Now})

	backlog := setup.addBacklog(t, "/ok.mp3")
	if recorder, err := serve(t, server, backlog, http.MethodGet, ""); err != nil || recorder.Body.String() != audio {
		t.Fatalf("stream: %v", err)
	}
	if stored := setup.reload(t, backlog); stored.CacheFile != "" {
		t.Error("unprocessed backlog episode was cached")
	}
}

// processedServer is a Server for the setup's feed, which processor
// handles, preparing episodes on request through the setup's pipeline.
func (setup *testSetup) processedServer(t *testing.T, processor *fakeProcessor, wait time.Duration) *Server {
	t.Helper()
	setup.withProcessor(t, processor)
	exits, err := outbound.New(outbound.Options{AllowPrivateDestinations: true})
	if err != nil {
		t.Fatal(err)
	}
	feedService := feeds.New(setup.store, exits, feeds.Options{DefaultDeliveryMode: "cache", Processed: processor.Handles})
	return NewServer(setup.store, exits, setup.cache, feedService, setup.pipeline, Options{ProcessingWait: wait, Now: setup.clock.Now})
}

func TestServeProcessesBacklogOnRequest(t *testing.T) {
	setup := newTestSetup(t)
	processor := &fakeProcessor{}
	server := setup.processedServer(t, processor, 5*time.Second)
	backlog := setup.addBacklog(t, "/ok.mp3")

	recorder, err := serve(t, server, backlog, http.MethodGet, "")
	if err != nil || recorder.Code != http.StatusOK || recorder.Body.String() != strings.ToUpper(audio) {
		t.Fatalf("first play: %d %q, %v; want the processed file", recorder.Code, recorder.Body.String(), err)
	}
	stored := setup.reload(t, backlog)
	if stored.CacheFile == "" || stored.State != models.EpisodeReady || !stored.Backlog || stored.ProcessNote != "shouted" {
		t.Errorf("episode = %+v", stored)
	}

	// Later plays, including Range, come from the cache.
	recorder, err = serve(t, server, stored, http.MethodGet, "bytes=0-2")
	if err != nil || recorder.Code != http.StatusPartialContent || recorder.Body.String() != "ID3" {
		t.Errorf("range: %d %q, %v", recorder.Code, recorder.Body.String(), err)
	}
	if processor.jobCount() != 1 {
		t.Errorf("processed %d times, want once", processor.jobCount())
	}

	// Once the cached file expires, the next play processes it again rather
	// than streaming the version with ads.
	fullPath, _ := setup.cache.Path(stored.CacheFile)
	os.Remove(fullPath)
	if recorder, err = serve(t, server, stored, http.MethodGet, ""); err != nil || recorder.Body.String() != strings.ToUpper(audio) {
		t.Errorf("after expiry: %q, %v", recorder.Body.String(), err)
	}
	if processor.jobCount() != 2 {
		t.Errorf("processed %d times, want twice", processor.jobCount())
	}
}

func TestServeSlowProcessingAnswersRetryAfter(t *testing.T) {
	setup := newTestSetup(t)
	processor := &fakeProcessor{gate: make(chan struct{})}
	server := setup.processedServer(t, processor, 50*time.Millisecond)
	backlog := setup.addBacklog(t, "/ok.mp3")

	// Two clients at once: both are told to come back, and there is only
	// one job.
	var wait sync.WaitGroup
	for range 2 {
		wait.Go(func() {
			recorder, err := serve(t, server, backlog, http.MethodGet, "")
			if err != nil || recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "30" {
				t.Errorf("slow processing: %d, Retry-After %q, %v", recorder.Code, recorder.Header().Get("Retry-After"), err)
			}
		})
	}
	wait.Wait()
	if processor.jobCount() != 1 {
		t.Fatalf("%d jobs, want 1", processor.jobCount())
	}
	if stored := setup.reload(t, backlog); stored.CacheFile != "" {
		t.Fatal("cached before processing finished")
	}

	// Processing carries on without the clients and the next one gets it.
	processor.gate <- struct{}{}
	deadline := time.Now().Add(5 * time.Second)
	for setup.reload(t, backlog).CacheFile == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if recorder, err := serve(t, server, backlog, http.MethodGet, ""); err != nil || recorder.Body.String() != strings.ToUpper(audio) {
		t.Errorf("after processing: %d %q, %v", recorder.Code, recorder.Body.String(), err)
	}
}

func TestServeOnRequestFailures(t *testing.T) {
	permanent := fmt.Errorf("%w: not MP3", ErrPermanent)
	cases := []struct {
		name     string
		err      error
		hide     bool
		wantCode int
		wantBody string
		wantErr  error
		state    models.EpisodeState
	}{
		// Retryable: the client is asked to come back; the episode stays
		// published and the next request tries again.
		{"temporary", errors.New("tunnel down"), false, http.StatusServiceUnavailable, "", nil, models.EpisodeReady},
		// Published unprocessed: streamed from the source and cached, and
		// still failed, so a change of settings retries it.
		{"permanent, publish", permanent, false, http.StatusOK, audio, nil, models.EpisodeFailed},
		{"permanent, hide", permanent, true, 0, "", database.ErrEpisodeNotFound, models.EpisodeFailed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setup := newTestSetup(t)
			processor := &fakeProcessor{err: c.err, hide: c.hide}
			server := setup.processedServer(t, processor, 5*time.Second)
			// The feed is in stream mode; cache mode is where the tee applies.
			setup.feed.DeliveryMode = "cache"
			setup.store.UpdateFeed(context.Background(), &setup.feed)
			backlog := setup.addBacklog(t, "/ok.mp3")

			recorder, err := serve(t, server, backlog, http.MethodGet, "")
			if !errors.Is(err, c.wantErr) || (c.wantCode != 0 && recorder.Code != c.wantCode) || (c.wantBody != "" && recorder.Body.String() != c.wantBody) {
				t.Fatalf("got %d %q, %v", recorder.Code, recorder.Body.String(), err)
			}
			stored := setup.reload(t, backlog)
			// The failure is on record; published unprocessed, the stream was
			// cached too.
			if cachedUnprocessed := c.name == "permanent, publish"; stored.State != c.state || (stored.CacheFile != "") != cachedUnprocessed || stored.LastError == "" {
				t.Errorf("episode = %+v", stored)
			}
			if c.name == "temporary" {
				processor.setErr(nil)
				if recorder, err := serve(t, server, backlog, http.MethodGet, ""); err != nil || recorder.Body.String() != strings.ToUpper(audio) {
					t.Errorf("retry: %d %q, %v", recorder.Code, recorder.Body.String(), err)
				}
			}
		})
	}
}

func TestServeClaimsWaitingEpisodeOnRequest(t *testing.T) {
	setup := newTestSetup(t)
	processor := &fakeProcessor{}
	server := setup.processedServer(t, processor, 5*time.Second)
	// Waiting for a retry an hour away, e.g. published by the never-empty
	// rule: a request doesn't wait for the retry.
	later := setup.clock.Now().Add(time.Hour)
	episode := setup.addEpisode(t, "/ok.mp3")
	episode.NextAttemptAt, episode.Attempts = &later, 1
	setup.store.UpdateEpisode(context.Background(), &episode)

	if recorder, err := serve(t, server, episode, http.MethodGet, ""); err != nil || recorder.Body.String() != strings.ToUpper(audio) {
		t.Fatalf("got %d %q, %v", recorder.Code, recorder.Body.String(), err)
	}
	if stored := setup.reload(t, episode); stored.State != models.EpisodeReady || stored.NextAttemptAt != nil || stored.Attempts != 2 {
		t.Errorf("episode = %+v", stored)
	}
	if setup.processOne(t) {
		t.Error("the pipeline found the episode again")
	}
}

func TestPrepareJoinsWorkerAndStopsWithPipeline(t *testing.T) {
	setup := newTestSetup(t)
	processor := &fakeProcessor{gate: make(chan struct{})}
	setup.withProcessor(t, processor)
	episode := setup.addEpisode(t, "/ok.mp3")

	// A worker has the episode: a request joins its job instead of
	// starting another.
	worked := make(chan bool)
	go func() {
		ok, _ := setup.pipeline.ProcessNext(context.Background())
		worked <- ok
	}()
	for processor.jobCount() == 0 {
		time.Sleep(time.Millisecond)
	}
	claimed := setup.reload(t, episode)
	done, err := setup.pipeline.Prepare(setup.feed, claimed)
	if err != nil {
		t.Fatal(err)
	}
	processor.gate <- struct{}{}
	<-done
	if !<-worked || processor.jobCount() != 1 || setup.reload(t, episode).State != models.EpisodeReady {
		t.Errorf("%d jobs, state %s", processor.jobCount(), setup.reload(t, episode).State)
	}

	// Run returns only after work started on request has stopped, and no
	// more is started afterwards.
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { setup.pipeline.Run(ctx); close(stopped) }()
	backlog := setup.addBacklog(t, "/ok.mp3")
	for {
		setup.pipeline.mutex.Lock()
		running := setup.pipeline.lifetime == ctx
		setup.pipeline.mutex.Unlock()
		if running {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := setup.pipeline.Prepare(setup.feed, backlog); err != nil {
		t.Fatal(err)
	}
	cancel() // the gated job is cancelled with the pipeline
	<-stopped
	if _, err := setup.pipeline.Prepare(setup.feed, backlog); !errors.Is(err, ErrBusy) {
		t.Errorf("after Run: err = %v, want ErrBusy", err)
	}
	if stored := setup.reload(t, backlog); stored.CacheFile != "" || stored.LastError != "" {
		t.Errorf("abandoned job recorded: %+v", stored)
	}
}
