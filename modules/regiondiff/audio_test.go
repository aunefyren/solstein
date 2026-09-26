package regiondiff

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aunefyren/solstein/episodes"
	"aunefyren/solstein/models"
	"aunefyren/solstein/mp3"
	"aunefyren/solstein/mp3/spectrum"
)

// fixture reads one of the audio fixtures made by testdata/generate.sh: a
// synthetic 40-second show, re-encoded with ads as RedCircle does.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func near(got, want, tolerance time.Duration) bool {
	return got >= want-tolerance && got <= want+tolerance
}

func TestAudioDiffKeepsCleanHomeWhole(t *testing.T) {
	home := fixture(t, "home-clean.mp3")
	result, err := Diff(home, fixture(t, "other-ads.mp3"), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !result.ByAudio || len(result.Removed) != 0 || !bytes.Equal(result.Output, home) {
		t.Fatalf("result: by audio %v, removed %+v, output unchanged %v", result.ByAudio, result.Removed, bytes.Equal(result.Output, home))
	}
	// The other copy's ads: 8 s at 15 s, 5 s at the end.
	if result.OtherBreaks != 2 || !near(result.OtherExtra, 13*time.Second, 500*time.Millisecond) {
		t.Errorf("other has %d breaks, %s more; want 2, 13s", result.OtherBreaks, result.OtherExtra)
	}
}

func TestAudioDiffCutsHomeAds(t *testing.T) {
	home := fixture(t, "home-ad.mp3")
	result, err := Diff(home, fixture(t, "other-ads.mp3"), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !result.ByAudio || len(result.Removed) != 1 {
		t.Fatalf("removed %+v, want the home copy's one ad", result.Removed)
	}
	file, _ := mp3.Parse(home)
	var at time.Duration
	for _, frame := range file.Frames[:result.Removed[0].Start] {
		at += frame.Header.Duration()
	}
	// The ad: 6 s at 25 s. Cuts are at frame boundaries, within the pause
	// precision allows.
	if !near(at, 25*time.Second, 300*time.Millisecond) || !near(result.Removed[0].Duration, 6*time.Second, 300*time.Millisecond) {
		t.Errorf("cut %s at %s, want 6s at 25s", result.Removed[0].Duration, at)
	}
	// The show, plus the silent frames at the cut: at 32 kbit/s a frame
	// carries little, so the reservoir's 511 bytes span up to five of them.
	if !near(result.Duration, 40*time.Second, 500*time.Millisecond) {
		t.Errorf("output plays %s, want the 40 s show", result.Duration)
	}

	// The frame after the cut borrows from the ad; the frames it borrows
	// from are kept as silent frames, so it decodes as it did.
	output, err := mp3.Parse(result.Output)
	if err != nil {
		t.Fatal(err)
	}
	resumed := file.Frames[result.Removed[0].End]
	carriers := result.Removed[0].End - carriersFor(file, result.Removed[0])
	if resumed.MainDataBegin > 0 && carriers == 0 {
		t.Fatal("no silent frames kept before a frame that borrows")
	}
	index := result.Removed[0].Start // in the output, the first silent frame
	for i := range carriers {
		silent := output.Frames[index+i]
		if silent.MainDataBegin != 0 || !bytes.Equal(output.FrameBytes(silent), file.SilentFrame(file.Frames[result.Removed[0].End-carriers+i])) {
			t.Errorf("output frame %d isn't the silent copy of the removed frame", index+i)
		}
	}
	if !bytes.Equal(output.FrameBytes(output.Frames[index+carriers]), file.FrameBytes(resumed)) {
		t.Error("the show doesn't resume right after the silent frames")
	}
	if want := len(file.Frames) - (result.Removed[0].End - result.Removed[0].Start) + carriers; len(output.Frames) != want {
		t.Errorf("output has %d frames, want %d", len(output.Frames), want)
	}
}

// carriersFor is the first frame of a removed segment that write keeps as
// a silent frame.
func carriersFor(file mp3.File, removed Segment) int {
	return carriers(file, removed.Start, removed.End)
}

func TestAudioDiffSameShowReencodedIsIdentical(t *testing.T) {
	if _, err := Diff(fixture(t, "home-clean.mp3"), fixture(t, "other-plain.mp3"), DefaultOptions()); !errors.Is(err, ErrIdentical) {
		t.Errorf("err = %v, want ErrIdentical", err)
	}
}

func TestAudioDiffUnrelatedAudio(t *testing.T) {
	_, err := Diff(fixture(t, "home-clean.mp3"), fixture(t, "unrelated.mp3"), DefaultOptions())
	if !errors.Is(err, ErrImplausible) {
		t.Errorf("err = %v, want implausible", err)
	}
}

func TestComparisonMeasuresHomeOnce(t *testing.T) {
	comparison := NewComparison(fixture(t, "home-clean.mp3"), DefaultOptions())
	if _, err := comparison.With(context.Background(), fixture(t, "other-plain.mp3")); !errors.Is(err, ErrIdentical) {
		t.Fatalf("first: %v", err)
	}
	measured := comparison.loudness
	if measured == nil {
		t.Fatal("home loudness not kept")
	}
	if result, err := comparison.With(context.Background(), fixture(t, "other-ads.mp3")); err != nil || !result.ByAudio || comparison.loudness != measured {
		t.Errorf("second: %v, measured again %v", err, comparison.loudness != measured)
	}
}

func TestAudioDiffStopsWhenCancelledOrOverBudget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewComparison(fixture(t, "home-clean.mp3"), DefaultOptions()).With(ctx, fixture(t, "other-ads.mp3")); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: err = %v", err)
	}

	home, other := testEnvelopes(t, "home-clean.mp3", "other-ads.mp3")
	if _, err := findRuns(context.Background(), home, other, DefaultOptions(), 1); !errors.Is(err, errSearchBudget) || !errors.Is(err, ErrImplausible) {
		t.Errorf("over budget: err = %v", err)
	}
	if runs, err := findRuns(context.Background(), home, other, DefaultOptions(), searchBudget); err != nil || len(runs) == 0 {
		t.Errorf("within budget: %d runs, %v", len(runs), err)
	}
}

// testEnvelopes are two fixtures' loudness, as audioDiff compares it.
func testEnvelopes(t *testing.T, homeName, otherName string) ([]float64, []float64) {
	t.Helper()
	var envelopes [2][]float64
	for i, name := range []string{homeName, otherName} {
		file, err := mp3.Parse(fixture(t, name))
		if err != nil {
			t.Fatal(err)
		}
		loudness, err := spectrum.Measure(context.Background(), file)
		if err != nil {
			t.Fatal(err)
		}
		envelopes[i] = quieted(loudness.Envelope(envelopeStep))
	}
	return envelopes[0], envelopes[1]
}

func TestInOrderRunsMergesSplitRuns(t *testing.T) {
	window := windowSteps * coarseFactor
	runs := []audioRun{
		{home: 0, length: 20000, offset: 4860},
		{home: 20250, length: 9000, offset: 4860}, // the same run, split by a stray window
		{home: 5000, length: 1000, offset: 90000}, // a false match out of order
		{home: 29250 + window, length: 20000, offset: 10130},
	}
	got := inOrderRuns(runs)
	if len(got) != 2 || got[0].home != 0 || got[0].end() != 29250 || got[1].offset != 10130 {
		t.Errorf("runs = %+v", got)
	}
}

func TestCompareByAudioSwitchedOff(t *testing.T) {
	options := DefaultOptions()
	options.CompareByAudio = false
	// Different sample rates: as before comparing by audio existed.
	_, err := Diff(fixture(t, "home-clean.mp3"), fixture(t, "other-ads.mp3"), options)
	if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "differ in format") {
		t.Errorf("err = %v, want ErrUnsupported for different formats", err)
	}
}

func TestProcessorCompareByAudioPerFeed(t *testing.T) {
	processor := newTestProcessor(t)
	feed := models.Feed{}
	if !processor.comparesByAudio(feed) || strings.Contains(processor.Recipe(feed), "not by audio") {
		t.Error("on by default")
	}
	feed.RegionDiffCompareByAudio = "off"
	if processor.comparesByAudio(feed) || !strings.Contains(processor.Recipe(feed), "not by audio") {
		t.Errorf("feed switched off: recipe %q", processor.Recipe(feed))
	}
	source := &fakeSource{downloads: map[string][]byte{"norway": fixture(t, "home-clean.mp3"), "sweden": fixture(t, "other-ads.mp3")}}
	job := source.job()
	job.Feed = feed
	if _, err := processor.Process(context.Background(), job); !errors.Is(err, episodes.ErrPermanent) {
		t.Errorf("feed switched off: err = %v, want a permanent failure", err)
	}
}

func TestAudioDiffCutsVariableBitrateStereo(t *testing.T) {
	// A stereo encoder's variable-bitrate, joint-stereo copy with the ad
	// at 25 s (6 s), against the other region's re-encode.
	home := fixture(t, "home-ad-vbr-stereo.mp3")
	file, _ := mp3.Parse(home)
	bitrates := map[int]bool{}
	for _, frame := range file.Frames {
		if frame.Header.Mono {
			t.Fatal("fixture isn't stereo")
		}
		bitrates[frame.Header.Bitrate] = true
	}
	if len(bitrates) < 3 {
		t.Fatalf("fixture has %d bitrates, want a variable bitrate", len(bitrates))
	}

	result, err := Diff(home, fixture(t, "other-ads.mp3"), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Removed) != 1 {
		t.Fatalf("removed %+v, want the one ad", result.Removed)
	}
	var at time.Duration
	for _, frame := range file.Frames[:result.Removed[0].Start] {
		at += frame.Header.Duration()
	}
	if !near(at, 25*time.Second, 300*time.Millisecond) || !near(result.Removed[0].Duration, 6*time.Second, 300*time.Millisecond) {
		t.Errorf("cut %s at %s, want 6s at 25s", result.Removed[0].Duration, at)
	}
	if output, err := mp3.Parse(result.Output); err != nil || len(output.Frames) == 0 {
		t.Errorf("output doesn't parse: %v", err)
	}
}
