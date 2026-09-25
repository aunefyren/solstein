package regiondiff

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"aunefyren/solstein/episodes"
	"aunefyren/solstein/models"
)

// fakeSource serves a download per exit, recording which exits were used.
type fakeSource struct {
	mutex     sync.Mutex
	downloads map[string][]byte
	failures  map[string]error
	fetched   []string
}

func (source *fakeSource) job() episodes.Job {
	return episodes.Job{Fetch: func(ctx context.Context, exit string) (episodes.Download, error) {
		source.mutex.Lock()
		source.fetched = append(source.fetched, exit)
		data, err := source.downloads[exit], source.failures[exit]
		source.mutex.Unlock()
		if err != nil {
			return episodes.Download{}, err
		}
		return episodes.Download{Data: data, ContentType: "audio/mpeg"}, nil
	}}
}

func newTestProcessor(t *testing.T, fallback ...string) *Processor {
	t.Helper()
	processor, err := NewProcessor(ProcessorOptions{Exits: [2]string{"norway", "sweden"}, FallbackExits: fallback, Diff: DefaultOptions()})
	if err != nil {
		t.Fatal(err)
	}
	return processor
}

func TestProcessorRemovesAds(t *testing.T) {
	home := join(tag("home"), show1, audio(200, 10), show2)
	source := &fakeSource{downloads: map[string][]byte{
		"norway": home,
		"sweden": join(tag("other"), show1, audio(250, 11), show2),
	}}
	processed, err := newTestProcessor(t).Process(context.Background(), source.job())
	if err != nil {
		t.Fatal(err)
	}
	if want := join(tag("home"), show1, show2); !bytes.Equal(processed.Audio, want) {
		t.Errorf("output is %d bytes, want the home tag and the show's %d", len(processed.Audio), len(want))
	}
	if processed.ContentType != "audio/mpeg" || processed.Duration != 700*26122448*time.Nanosecond {
		t.Errorf("content type %q, duration %v", processed.ContentType, processed.Duration)
	}
	if processed.Note != "removed 5s of ads in 1 break, comparing norway with sweden" {
		t.Errorf("note = %q", processed.Note)
	}
}

func TestProcessorDownloadsInParallel(t *testing.T) {
	var started sync.WaitGroup
	started.Add(2)
	both := make(chan struct{})
	go func() { started.Wait(); close(both) }()
	data := map[string][]byte{"norway": join(show1, audio(200, 10), show2), "sweden": join(show1, audio(200, 11), show2)}
	job := episodes.Job{Fetch: func(ctx context.Context, exit string) (episodes.Download, error) {
		started.Done()
		select {
		case <-both:
		case <-time.After(5 * time.Second):
			return episodes.Download{}, errors.New("the other download never started")
		}
		return episodes.Download{Data: data[exit]}, nil
	}}
	if _, err := newTestProcessor(t).Process(context.Background(), job); err != nil {
		t.Fatal(err)
	}
}

func TestProcessorIdenticalTriesFallbacks(t *testing.T) {
	home := join(show1, audio(200, 10), show2)
	source := &fakeSource{downloads: map[string][]byte{
		"norway":  home,
		"sweden":  home, // the same campaign in both markets
		"germany": home,
		"denmark": join(show1, audio(200, 12), show2),
	}}
	processed, err := newTestProcessor(t, "germany", "denmark").Process(context.Background(), source.job())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(processed.Audio, join(show1, show2)) {
		t.Error("the ad wasn't removed by comparing with the second fallback")
	}
	if !strings.HasSuffix(processed.Note, "comparing norway with denmark") {
		t.Errorf("note = %q", processed.Note)
	}
	if last := source.fetched[len(source.fetched)-1]; len(source.fetched) != 4 || last != "denmark" {
		t.Errorf("fetched %v", source.fetched)
	}
}

func TestProcessorIdenticalEverywhereKeepsEpisode(t *testing.T) {
	home := join(show1, show2)
	source := &fakeSource{downloads: map[string][]byte{"norway": home, "sweden": home, "germany": home}}
	processor := newTestProcessor(t, "germany")
	processed, err := processor.Process(context.Background(), source.job())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(processed.Audio, home) || processed.Duration != 0 || processed.Note != "no dynamic ads found" {
		t.Errorf("got %d bytes, duration %v, note %q; want the home download unchanged", len(processed.Audio), processed.Duration, processed.Note)
	}

	// Much longer than stated: the same ads may be everywhere.
	job := source.job()
	job.ExpectedDuration = 10 * time.Second
	processed, err = processor.Process(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(processed.Note, "8s longer than stated") {
		t.Errorf("note = %q", processed.Note)
	}
}

func TestProcessorFailures(t *testing.T) {
	good := join(show1, audio(200, 10), show2)
	unavailable := errors.New("tunnel down")
	cases := []struct {
		name      string
		source    *fakeSource
		expected  time.Duration
		permanent bool
		contains  string
	}{
		{"download fails", &fakeSource{downloads: map[string][]byte{"norway": good}, failures: map[string]error{"sweden": unavailable}}, 0, false, `exit "sweden": tunnel down`},
		{"not MP3", &fakeSource{downloads: map[string][]byte{"norway": []byte("not audio at all"), "sweden": []byte("other bytes, not audio")}}, 0, true, "frame by frame"},
		{"implausible", &fakeSource{downloads: map[string][]byte{"norway": good, "sweden": join(show1, audio(200, 11), show2)}}, time.Hour, false, "implausible"},
		{"fallback fails", &fakeSource{downloads: map[string][]byte{"norway": good, "sweden": good}, failures: map[string]error{"germany": unavailable}}, 0, false, `fallback exit "germany"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			job := c.source.job()
			job.ExpectedDuration = c.expected
			_, err := newTestProcessor(t, "germany").Process(context.Background(), job)
			if err == nil || !strings.Contains(err.Error(), c.contains) {
				t.Fatalf("err = %v, want one mentioning %q", err, c.contains)
			}
			if errors.Is(err, episodes.ErrPermanent) != c.permanent {
				t.Errorf("permanent = %v, want %v", errors.Is(err, episodes.ErrPermanent), c.permanent)
			}
		})
	}
}

func TestNewProcessorChecksExits(t *testing.T) {
	for _, options := range []ProcessorOptions{
		{Exits: [2]string{"norway", ""}},
		{Exits: [2]string{"sweden", "sweden"}},
		{Exits: [2]string{"norway", "sweden"}, FallbackExits: []string{"norway"}},
		{Exits: [2]string{"norway", "sweden"}, FallbackExits: []string{"germany", "germany"}},
	} {
		if _, err := NewProcessor(options); err == nil {
			t.Errorf("NewProcessor(%+v) accepted", options)
		}
	}
	processor, err := NewProcessor(ProcessorOptions{Exits: [2]string{"direct", "sweden"}, HideOnFailure: true})
	if err != nil {
		t.Fatal(err)
	}
	if processor.Name() != "region diff" || !processor.HideOnFailure(models.Feed{}) {
		t.Error("name or failure policy wrong")
	}
}

func TestProcessorLiveDownloads(t *testing.T) {
	directory := filepath.Join("..", "..", "config", "live")
	norway, errNorway := os.ReadFile(filepath.Join(directory, "direct-1.mp3"))
	sweden, errSweden := os.ReadFile(filepath.Join(directory, "sweden-1.mp3"))
	if errNorway != nil || errSweden != nil {
		t.Skip("live downloads not present; run the live region test to fetch them")
	}
	source := &fakeSource{downloads: map[string][]byte{"norway": norway, "sweden": sweden}}
	job := source.job()
	job.ExpectedDuration = 39*time.Minute + 52*time.Second
	processed, err := newTestProcessor(t).Process(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s; %.1f MB, %s", processed.Note, float64(len(processed.Audio))/(1<<20), processed.Duration.Round(time.Second))
	if processed.Note != "removed 4m32s of ads in 4 breaks, comparing norway with sweden" {
		t.Errorf("note = %q", processed.Note)
	}
}

func TestProcessorUsesFeedExits(t *testing.T) {
	source := &fakeSource{downloads: map[string][]byte{
		"norway":  join(show1, audio(200, 10), show2),
		"denmark": join(show1, audio(200, 12), show2),
	}}
	job := source.job()
	job.Feed = models.Feed{RegionDiffExits: []string{"norway", "denmark"}}
	processed, err := newTestProcessor(t).Process(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(processed.Note, "comparing norway with denmark") || len(source.fetched) != 2 {
		t.Errorf("note %q, fetched %v", processed.Note, source.fetched)
	}
}

func TestProcessorSkipsExitsInTheHomeCountry(t *testing.T) {
	good := map[string][]byte{
		"norway":  join(show1, audio(200, 10), show2),
		"sweden":  join(show1, audio(200, 11), show2),
		"germany": join(show1, audio(200, 12), show2),
	}
	newProcessor := func(locator fakeLocator) *Processor {
		processor, err := NewProcessor(ProcessorOptions{Exits: [2]string{"norway", "sweden"}, FallbackExits: []string{"germany"}, Diff: DefaultOptions(), Locator: locator})
		if err != nil {
			t.Fatal(err)
		}
		return processor
	}

	// Sweden has fallen back to a Norwegian server: Germany is compared instead.
	source := &fakeSource{downloads: good}
	processed, err := newProcessor(fakeLocator{"norway": {"NO"}, "sweden": {"NO"}, "germany": {"DE"}}).Process(context.Background(), source.job())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(processed.Note, "comparing norway with germany") || slices.Contains(source.fetched, "sweden") {
		t.Errorf("note %q, fetched %v", processed.Note, source.fetched)
	}

	// Everything in Norway: nothing is downloaded, and it is retried later.
	source = &fakeSource{downloads: good}
	_, err = newProcessor(fakeLocator{"norway": {"NO"}, "sweden": {"NO"}, "germany": {"NO"}}).Process(context.Background(), source.job())
	if err == nil || errors.Is(err, episodes.ErrPermanent) || !strings.Contains(err.Error(), "all come out in NO") || len(source.fetched) != 0 {
		t.Errorf("err = %v, fetched %v; want a retryable error before any download", err, source.fetched)
	}

	// Unknown countries (direct) are trusted.
	source = &fakeSource{downloads: good}
	if _, err := newProcessor(fakeLocator{"sweden": {"SE"}}).Process(context.Background(), source.job()); err != nil {
		t.Errorf("unknown home country: %v", err)
	}
}

func TestProcessorTrimsBreakMarkersPerFeed(t *testing.T) {
	marker := audio(87, 99)
	source := &fakeSource{downloads: map[string][]byte{
		"norway": join(audio(100, 11), marker, show1, marker, audio(80, 12), show2),
		"sweden": join(audio(90, 21), marker, show1, marker, audio(70, 22), show2),
	}}
	options := DefaultOptions()
	options.MaxRemovedShare = 0.6
	processor, err := NewProcessor(ProcessorOptions{Exits: [2]string{"norway", "sweden"}, Diff: options})
	if err != nil {
		t.Fatal(err)
	}
	process := func(feed models.Feed) episodes.Processed {
		t.Helper()
		job := source.job()
		job.Feed = feed
		processed, err := processor.Process(context.Background(), job)
		if err != nil {
			t.Fatal(err)
		}
		return processed
	}
	// Off globally; a feed switches it on.
	if note := process(models.Feed{}).Note; strings.Contains(note, "marker") {
		t.Errorf("trimmed with trimming off: %q", note)
	}
	if note := process(models.Feed{RegionDiffTrimBreakMarkers: "on"}).Note; note != "removed 9s of ads in 2 breaks, comparing norway with sweden, including 2 break markers (5s)" {
		t.Errorf("note = %q", note)
	}
	// On globally; a feed switches it off.
	processor.options.Diff.TrimBreakMarkers = true
	if note := process(models.Feed{RegionDiffTrimBreakMarkers: "off"}).Note; strings.Contains(note, "marker") {
		t.Errorf("trimmed for a feed that switched it off: %q", note)
	}
}
