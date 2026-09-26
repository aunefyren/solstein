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
	"sync/atomic"
	"testing"
	"time"

	"aunefyren/solstein/episodes"
	"aunefyren/solstein/models"

	"github.com/google/uuid"
)

// fakeSource serves a download per exit, recording which exits were used.
// fresh, when set for an exit, is what a fresh fetch gets instead.
type fakeSource struct {
	mutex     sync.Mutex
	downloads map[string][]byte
	fresh     map[string][]byte
	failures  map[string]error
	fetched   []string
}

func (source *fakeSource) job() episodes.Job {
	return episodes.Job{Fetch: func(ctx context.Context, exit string, fresh bool) (episodes.Download, error) {
		source.mutex.Lock()
		label := exit
		if fresh {
			label += " (fresh)"
		}
		source.fetched = append(source.fetched, label)
		data, err := source.downloads[exit], source.failures[exit]
		if again, ok := source.fresh[exit]; ok && fresh {
			data = again
		}
		source.mutex.Unlock()
		if err == nil && data == nil {
			err = errors.New("no such exit in this test") // as a real fetch never succeeds empty
		}
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
	job := episodes.Job{Fetch: func(ctx context.Context, exit string, fresh bool) (episodes.Download, error) {
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

// TestProcessorSkipsUnreachableFallbacks covers a fallback exit that can't
// be downloaded through (seen live: its VPN's resolver failing on the
// host's name). The next fallback is tried, and with none left the pair's
// agreement stands.
func TestProcessorSkipsUnreachableFallbacks(t *testing.T) {
	home := join(show1, audio(200, 10), show2)
	unavailable := errors.New("lookup timed out")

	source := &fakeSource{
		downloads: map[string][]byte{"norway": home, "sweden": home, "denmark": join(show1, audio(200, 12), show2)},
		failures:  map[string]error{"germany": unavailable},
	}
	processed, err := newTestProcessor(t, "germany", "denmark").Process(context.Background(), source.job())
	if err != nil {
		t.Fatalf("with a later fallback working: %v", err)
	}
	if !bytes.Equal(processed.Audio, join(show1, show2)) {
		t.Error("the ad wasn't removed by comparing with the fallback after the failed one")
	}

	source = &fakeSource{downloads: map[string][]byte{"norway": home, "sweden": home}, failures: map[string]error{"germany": unavailable, "denmark": unavailable}}
	processed, err = newTestProcessor(t, "germany", "denmark").Process(context.Background(), source.job())
	if err != nil {
		t.Fatalf("with every fallback failing: %v", err)
	}
	if !bytes.Equal(processed.Audio, home) || processed.Note != "no dynamic ads found (not compared through germany, denmark: the download failed)" {
		t.Errorf("got %d bytes, note %q; want the home download kept, noting the failed fallbacks", len(processed.Audio), processed.Note)
	}
}

// TestProcessorFallbackStopsWhenCancelled makes sure a shutdown during a
// fallback download fails the attempt instead of keeping the episode.
func TestProcessorFallbackStopsWhenCancelled(t *testing.T) {
	home := join(show1, audio(200, 10), show2)
	ctx, cancel := context.WithCancel(context.Background())
	source := &fakeSource{downloads: map[string][]byte{"norway": home, "sweden": home}}
	job := source.job()
	fetch := job.Fetch
	job.Fetch = func(ctx context.Context, exit string, fresh bool) (episodes.Download, error) {
		if exit == "germany" {
			cancel()
			return episodes.Download{}, ctx.Err()
		}
		return fetch(ctx, exit, fresh)
	}
	if _, err := newTestProcessor(t, "germany").Process(ctx, job); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
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
		// The partner and the fallback fail: nothing to compare with.
		{"download fails", &fakeSource{downloads: map[string][]byte{"norway": good}, failures: map[string]error{"sweden": unavailable, "germany": unavailable}}, 0, false, `exit "sweden": tunnel down`},
		{"home download fails", &fakeSource{downloads: map[string][]byte{"sweden": good, "germany": good}, failures: map[string]error{"norway": unavailable}}, 0, false, `exit "norway": tunnel down`},
		{"not MP3", &fakeSource{downloads: map[string][]byte{"norway": []byte("not audio at all"), "sweden": []byte("other bytes, not audio")}}, 0, true, "frame by frame"},
		{"implausible", &fakeSource{downloads: map[string][]byte{"norway": good, "sweden": join(show1, audio(200, 11), show2)}}, time.Hour, false, "implausible"},
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

func TestProcessorRefetchesIncompleteDownloads(t *testing.T) {
	full := join(show1, audio(200, 11), show2)
	source := &fakeSource{
		downloads: map[string][]byte{"norway": join(show1, audio(200, 10), show2), "sweden": show1}, // cut off
		fresh:     map[string][]byte{"sweden": full},
	}
	job := source.job()
	job.ExpectedDuration = 18 * time.Second
	processed, err := newTestProcessor(t).Process(context.Background(), job)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !bytes.Equal(processed.Audio, join(show1, show2)) {
		t.Error("the diff didn't use the fresh download")
	}
	if !slices.Contains(source.fetched, "sweden (fresh)") || slices.Contains(source.fetched, "norway (fresh)") {
		t.Errorf("fetched %v; want only the short side again, fresh", source.fetched)
	}

	// Without a stated duration, the other download is the yardstick.
	source = &fakeSource{
		downloads: map[string][]byte{"norway": join(show1, audio(200, 10), show2), "sweden": show1},
		fresh:     map[string][]byte{"sweden": full},
	}
	if _, err := newTestProcessor(t).Process(context.Background(), source.job()); err != nil || !slices.Contains(source.fetched, "sweden (fresh)") {
		t.Errorf("err = %v, fetched %v", err, source.fetched)
	}

	// Complete downloads are never fetched twice.
	source = &fakeSource{downloads: map[string][]byte{"norway": join(show1, audio(200, 10), show2), "sweden": full}}
	job = source.job()
	job.ExpectedDuration = 18 * time.Second
	if _, err := newTestProcessor(t).Process(context.Background(), job); err != nil || len(source.fetched) != 2 {
		t.Errorf("err = %v, fetched %v", err, source.fetched)
	}
}

func TestProcessorKeepsFailedDownloads(t *testing.T) {
	directory := t.TempDir()
	processor, err := NewProcessor(ProcessorOptions{Exits: [2]string{"norway", "sweden"}, Diff: DefaultOptions(), FailureDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	// Nothing in common, and a fresh fetch doesn't help: implausible.
	source := &fakeSource{downloads: map[string][]byte{"norway": join(audio(300, 1)), "sweden": join(audio(300, 2))}}
	job := source.job()
	job.Feed.ID, job.Episode.ID, job.Episode.Title, job.Episode.SourceURL = uuid.New(), uuid.New(), "Bad day", "https://host.example/e.mp3?token=secret"
	_, err = processor.Process(context.Background(), job)
	if !errors.Is(err, ErrImplausible) || !strings.Contains(err.Error(), "(norway: 0.1 MB, 8s; sweden: 0.1 MB, 8s)") {
		t.Fatalf("err = %v; want implausible, describing both downloads", err)
	}

	kept := filepath.Join(directory, job.Feed.ID.String(), job.Episode.ID.String())
	for _, name := range []string{"norway.mp3", "sweden.mp3", "failure.txt"} {
		if _, err := os.Stat(filepath.Join(kept, name)); err != nil {
			t.Errorf("%s not kept: %v", name, err)
		}
	}
	note, _ := os.ReadFile(filepath.Join(kept, "failure.txt"))
	if !strings.Contains(string(note), "Bad day") || !strings.Contains(string(note), "implausible") || strings.Contains(string(note), "secret") {
		t.Errorf("note = %s", note)
	}

	// Old sets go when the next failure is kept.
	old := time.Now().Add(-keptRetention - time.Hour)
	os.Chtimes(kept, old, old)
	job.Episode.ID = uuid.New()
	processor.Process(context.Background(), job)
	if _, err := os.Stat(kept); !os.IsNotExist(err) {
		t.Errorf("old set still there: %v", err)
	}

	// Off without a directory.
	processor.options.FailureDir = ""
	processor.keepFailed(job, err, nil) // must not panic or write
}

func TestProcessorKeepsSuccessfulDownloads(t *testing.T) {
	directory := t.TempDir()
	processor, err := NewProcessor(ProcessorOptions{Exits: [2]string{"norway", "sweden"}, Diff: DefaultOptions(), SuccessDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	source := &fakeSource{downloads: map[string][]byte{"norway": join(show1, audio(200, 10), show2), "sweden": join(show1, audio(200, 11), show2)}}
	job := source.job()
	job.Feed.ID, job.Episode.ID, job.Episode.Title = uuid.New(), uuid.New(), "Good day"
	processed, err := processor.Process(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}

	kept := filepath.Join(directory, job.Feed.ID.String(), job.Episode.ID.String())
	for _, name := range []string{"norway.mp3", "sweden.mp3", "result.txt"} {
		if _, err := os.Stat(filepath.Join(kept, name)); err != nil {
			t.Errorf("%s not kept: %v", name, err)
		}
	}
	// The ad sits after the first show segment: its offset is that
	// segment's length.
	note, _ := os.ReadFile(filepath.Join(kept, "result.txt"))
	offset := formatOffset(audioDuration(join(show1)))
	if !strings.Contains(string(note), "Good day") || !strings.Contains(string(note), processed.Note) || !strings.Contains(string(note), "- at "+offset+", ") {
		t.Errorf("note = %s; want the ad at %s", note, offset)
	}

	// Identical downloads are kept too, without cuts.
	same := &fakeSource{downloads: map[string][]byte{"norway": join(show1, show2), "sweden": join(show1, show2)}}
	job = same.job()
	job.Feed.ID, job.Episode.ID = uuid.New(), uuid.New()
	if _, err := processor.Process(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	note, err = os.ReadFile(filepath.Join(directory, job.Feed.ID.String(), job.Episode.ID.String(), "result.txt"))
	if err != nil || !strings.Contains(string(note), "no dynamic ads found") || strings.Contains(string(note), "Removed from") {
		t.Errorf("identical: %s, %v", note, err)
	}
}

func TestFormatOffset(t *testing.T) {
	if got := formatOffset(time.Hour + 2*time.Minute + 3*time.Second + 45*time.Millisecond); got != "1:02:03.045" {
		t.Errorf("got %s", got)
	}
}

func TestProcessorFallsBackWhenThePartnerFails(t *testing.T) {
	source := &fakeSource{
		downloads: map[string][]byte{"norway": join(show1, audio(200, 10), show2), "germany": join(show1, audio(200, 12), show2)},
		failures:  map[string]error{"sweden": errors.New("http2: timeout awaiting response headers")},
	}
	processed, err := newTestProcessor(t, "germany").Process(context.Background(), source.job())
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !strings.HasSuffix(processed.Note, "comparing norway with germany") || !bytes.Equal(processed.Audio, join(show1, show2)) {
		t.Errorf("note %q", processed.Note)
	}
	// The home download isn't repeated; germany is fetched once.
	if count := func(exit string) (n int) {
		for _, fetched := range source.fetched {
			if fetched == exit {
				n++
			}
		}
		return n
	}; count("norway") != 1 || count("germany") != 1 {
		t.Errorf("fetched %v", source.fetched)
	}
}

func TestProcessorNotesSharedAdsLeftIn(t *testing.T) {
	source := &fakeSource{downloads: map[string][]byte{
		"norway": join(show1, audio(200, 10), show2),
		"sweden": join(show1, audio(200, 11), show2),
	}}
	job := source.job()
	job.ExpectedDuration = 12 * time.Second // the result is 18 s
	processed, err := newTestProcessor(t).Process(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(processed.Note, "; still 6s longer than stated: ads the same in every compared region may be left") {
		t.Errorf("note = %q", processed.Note)
	}
}

func TestProcessorComparesReencodedDownloadsByAudio(t *testing.T) {
	// RedCircle: the home region gets the original, the other a re-encoded
	// copy with ads, so no frame is shared.
	home := fixture(t, "home-clean.mp3")
	source := &fakeSource{downloads: map[string][]byte{"norway": home, "sweden": fixture(t, "other-ads.mp3")}}
	processed, err := newTestProcessor(t).Process(context.Background(), source.job())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(processed.Audio, home) || processed.Duration != 0 {
		t.Error("the home download wasn't kept whole")
	}
	if want := "no ads in the norway download: sweden's has 2 breaks (13s) more; compared by audio"; !strings.HasPrefix(processed.Note, want) {
		t.Errorf("note %q, want it to start %q", processed.Note, want)
	}

	// A home download with an ad of its own is cut.
	source = &fakeSource{downloads: map[string][]byte{"norway": fixture(t, "home-ad.mp3"), "sweden": fixture(t, "other-ads.mp3")}}
	processed, err = newTestProcessor(t).Process(context.Background(), source.job())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(processed.Note, "in 1 break, comparing norway with sweden by audio") || processed.Duration < 39*time.Second || processed.Duration > 41*time.Second {
		t.Errorf("note %q, duration %s", processed.Note, processed.Duration)
	}
}

// fakeBudgeter answers ExitsFit with what the test wants.
type fakeBudgeter struct {
	fits bool
	why  string
}

func (budgeter fakeBudgeter) ExitsFit([]string) (bool, string) {
	return budgeter.fits, budgeter.why
}

// Short of tunnels, attempts are prepared one at a time: two at once would
// close and reopen each other's tunnels (docs/exits.md).
func TestProcessorOneAtATimeWhenTunnelsAreShort(t *testing.T) {
	// One attempt downloads through both exits at once, so only the home
	// exit's download marks an attempt having started.
	inFetch := make(chan struct{})
	release := make(chan struct{})
	job := episodes.Job{Fetch: func(ctx context.Context, exit string, fresh bool) (episodes.Download, error) {
		if exit != "norway" {
			return episodes.Download{}, errors.New("the partner is not what this test measures")
		}
		inFetch <- struct{}{}
		<-release
		return episodes.Download{}, errors.New("that is all this test needs")
	}}

	processor, err := NewProcessor(ProcessorOptions{
		Exits: [2]string{"norway", "sweden"}, Diff: DefaultOptions(),
		Budgeter: fakeBudgeter{fits: false, why: "provider 'proton' can hold 1 tunnel at once"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if processor.oneAtATime == nil {
		t.Fatal("attempts are not held to one at a time although the exits don't fit")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for range 2 {
		go func() { processor.Process(ctx, job) }()
	}
	// The first attempt is downloading; the second must not have started.
	<-inFetch
	select {
	case <-inFetch:
		t.Fatal("a second attempt started while the first held the turn")
	case <-time.After(100 * time.Millisecond):
	}

	// Once the first is done, the second gets its turn.
	close(release)
	select {
	case <-inFetch:
	case <-time.After(5 * time.Second):
		t.Error("the second attempt never got its turn")
	}
}

func TestProcessorRunsTogetherWhenTunnelsFit(t *testing.T) {
	inFetch := make(chan struct{}, 4)
	release := make(chan struct{})
	job := episodes.Job{Fetch: func(ctx context.Context, exit string, fresh bool) (episodes.Download, error) {
		if exit != "norway" {
			return episodes.Download{}, errors.New("the partner is not what this test measures")
		}
		inFetch <- struct{}{}
		<-release
		return episodes.Download{}, errors.New("that is all this test needs")
	}}
	defer close(release)

	for _, budgeter := range []Budgeter{fakeBudgeter{fits: true}, nil} {
		processor, err := NewProcessor(ProcessorOptions{
			Exits: [2]string{"norway", "sweden"}, Diff: DefaultOptions(), Budgeter: budgeter,
		})
		if err != nil {
			t.Fatal(err)
		}
		if processor.oneAtATime != nil {
			t.Fatalf("attempts held to one at a time with budgeter %v", budgeter)
		}
		ctx, cancel := context.WithCancel(context.Background())
		for range 2 {
			go func() { processor.Process(ctx, job) }()
		}
		for range 2 {
			select {
			case <-inFetch:
			case <-time.After(5 * time.Second):
				t.Error("both attempts didn't run together")
			}
		}
		cancel()
	}
}

// In turn, the two downloads never overlap, so one tunnel is enough — and the
// result is the same as downloading them together.
func TestProcessorDownloadsInTurn(t *testing.T) {
	var live, most int32
	data := map[string][]byte{"norway": join(tag("home"), show1, audio(200, 10), show2), "sweden": join(show1, audio(250, 11), show2)}
	job := episodes.Job{Fetch: func(ctx context.Context, exit string, fresh bool) (episodes.Download, error) {
		now := atomic.AddInt32(&live, 1)
		defer atomic.AddInt32(&live, -1)
		if now > atomic.LoadInt32(&most) {
			atomic.StoreInt32(&most, now)
		}
		time.Sleep(20 * time.Millisecond) // long enough to overlap if they were together
		body, ok := data[exit]
		if !ok {
			return episodes.Download{}, errors.New("no such exit in this test")
		}
		return episodes.Download{Data: body, ContentType: "audio/mpeg"}, nil
	}}

	processor, err := NewProcessor(ProcessorOptions{
		Exits: [2]string{"norway", "sweden"}, Diff: DefaultOptions(), PairDownloads: "in_turn",
	})
	if err != nil {
		t.Fatal(err)
	}
	processed, err := processor.Process(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&most); got != 1 {
		t.Errorf("%d downloads ran at once, want one at a time", got)
	}
	if want := join(tag("home"), show1, show2); !bytes.Equal(processed.Audio, want) {
		t.Errorf("output is %d bytes, want the home tag and the show's %d", len(processed.Audio), len(want))
	}
	if processed.Note != "removed 5s of ads in 1 break, comparing norway with sweden" {
		t.Errorf("note = %q", processed.Note)
	}
}

// In turn, a home download that fails ends the attempt, and a partner that
// fails hands over to the fallback exits as usual.
func TestProcessorInTurnFailures(t *testing.T) {
	data := map[string][]byte{"germany": join(show1, audio(250, 11), show2)}
	home := join(tag("home"), show1, audio(200, 10), show2)
	inTurn := func(t *testing.T) *Processor {
		t.Helper()
		processor, err := NewProcessor(ProcessorOptions{
			Exits: [2]string{"norway", "sweden"}, FallbackExits: []string{"germany"},
			Diff: DefaultOptions(), PairDownloads: "in_turn",
		})
		if err != nil {
			t.Fatal(err)
		}
		return processor
	}

	t.Run("the home download fails", func(t *testing.T) {
		var fetched []string
		job := episodes.Job{Fetch: func(ctx context.Context, exit string, fresh bool) (episodes.Download, error) {
			fetched = append(fetched, exit)
			return episodes.Download{}, errors.New("no answer")
		}}
		if _, err := inTurn(t).Process(context.Background(), job); err == nil {
			t.Error("the attempt went on without the home download")
		}
		if len(fetched) != 1 || fetched[0] != "norway" {
			t.Errorf("fetched %v, want the home exit only: there is nothing to compare with", fetched)
		}
	})

	t.Run("the partner fails and a fallback takes over", func(t *testing.T) {
		job := episodes.Job{Fetch: func(ctx context.Context, exit string, fresh bool) (episodes.Download, error) {
			switch exit {
			case "norway":
				return episodes.Download{Data: home, ContentType: "audio/mpeg"}, nil
			case "sweden":
				return episodes.Download{}, errors.New("no answer through sweden")
			}
			return episodes.Download{Data: data[exit], ContentType: "audio/mpeg"}, nil
		}}
		processed, err := inTurn(t).Process(context.Background(), job)
		if err != nil {
			t.Fatal(err)
		}
		if processed.Note != "removed 5s of ads in 1 break, comparing norway with germany" {
			t.Errorf("note = %q", processed.Note)
		}
	})
}

// auto downloads together when the pair's exits fit, and in turn when they
// don't; together and in_turn ignore what the keys allow.
func TestProcessorPairDownloadsAuto(t *testing.T) {
	cases := []struct {
		setting string
		fits    bool
		inTurn  bool
	}{
		{"auto", true, false},
		{"auto", false, true},
		{"together", false, false},
		{"in_turn", true, true},
		{"", true, false}, // unset behaves as auto
	}
	for _, test := range cases {
		processor, err := NewProcessor(ProcessorOptions{
			Exits: [2]string{"norway", "sweden"}, Diff: DefaultOptions(),
			PairDownloads: test.setting, Budgeter: fakeBudgeter{fits: test.fits, why: "one key"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := processor.downloadsInTurn(processor.options.Exits); got != test.inTurn {
			t.Errorf("pair_downloads %q with fits=%v: in turn = %v, want %v", test.setting, test.fits, got, test.inTurn)
		}
	}

	// Without a Budgeter nothing says the tunnels are short.
	processor, _ := NewProcessor(ProcessorOptions{Exits: [2]string{"norway", "sweden"}, Diff: DefaultOptions(), PairDownloads: "auto"})
	if processor.downloadsInTurn(processor.options.Exits) {
		t.Error("auto chose in turn with nothing to say the tunnels are short")
	}
}
