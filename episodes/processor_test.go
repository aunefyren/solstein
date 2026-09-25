package episodes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"aunefyren/solstein/database"
	"aunefyren/solstein/feeds"
	"aunefyren/solstein/models"
	"aunefyren/solstein/outbound"
)

// fakeProcessor fetches the source through "direct" and returns it
// upper-cased, or fails with err.
type fakeProcessor struct {
	err  error
	hide bool
	jobs []Job
}

func (processor *fakeProcessor) Name() string                        { return "fake" }
func (processor *fakeProcessor) Handles(feed models.Feed) bool       { return feed.Title == "Processed" }
func (processor *fakeProcessor) HideOnFailure(feed models.Feed) bool { return processor.hide }

func (processor *fakeProcessor) Process(ctx context.Context, job Job) (Processed, error) {
	processor.jobs = append(processor.jobs, job)
	if processor.err != nil {
		return Processed{}, processor.err
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
	processor.err = nil
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
	server := NewServer(setup.store, exits, setup.cache, feedService, Options{Now: setup.clock.Now})

	backlog := setup.addBacklog(t, "/ok.mp3")
	if recorder, err := serve(t, server, backlog, http.MethodGet, ""); err != nil || recorder.Body.String() != audio {
		t.Fatalf("stream: %v", err)
	}
	if stored := setup.reload(t, backlog); stored.CacheFile != "" {
		t.Error("unprocessed backlog episode was cached")
	}
}
