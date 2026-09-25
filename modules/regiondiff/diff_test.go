package regiondiff

import (
	"bytes"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aunefyren/solstein/mp3"
)

// The synthetic episodes use MPEG-1 Layer III, 128 kbit/s, 44.1 kHz frames
// of 417 bytes (26.1 ms), as Acast serves.
var frameHeader = []byte{0xFF, 0xFB, 0x90, 0x00}

const frameSize = 417

// frameOf builds one frame: the header, main_data_begin, then payload.
func frameOf(mainDataBegin int, payload func(i int) byte) []byte {
	data := make([]byte, frameSize)
	copy(data, frameHeader)
	data[4] = byte(mainDataBegin >> 1)
	data[5] = byte(mainDataBegin&1) << 7
	for i := 6; i < frameSize; i++ {
		data[i] = payload(i)
	}
	return data
}

// audio is a segment of distinct frames, as show audio or an ad is. The
// first frame starts clean (main_data_begin 0), as at a splice point; the
// rest borrow from earlier frames, as in real audio.
func audio(frames int, seed uint64) []byte {
	random := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	var data []byte
	for i := range frames {
		mainDataBegin := 100 + i%300
		if i == 0 {
			mainDataBegin = 0
		}
		data = append(data, frameOf(mainDataBegin, func(int) byte { return byte(random.UintN(256)) })...)
	}
	return data
}

// silence is identical frames that borrow from earlier ones, as encoded
// silence at the end of an ad is.
func silence(frames int) []byte {
	var data []byte
	for range frames {
		data = append(data, frameOf(42, func(int) byte { return 0x55 })...)
	}
	return data
}

func tag(text string) []byte {
	payload := []byte(text)
	return append([]byte{'I', 'D', '3', 4, 0, 0, 0, 0, 0, byte(len(payload))}, payload...)
}

func join(parts ...[]byte) []byte {
	return bytes.Join(parts, nil)
}

var (
	show1 = audio(400, 1) // 10.4 s
	show2 = audio(300, 2) // 7.8 s
)

func mustDiff(t *testing.T, home, other []byte) Result {
	t.Helper()
	result, err := Diff(home, other, DefaultOptions())
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	return result
}

func TestDiffRemovesAds(t *testing.T) {
	home := join(tag("home"), audio(100, 11), show1, audio(80, 12), show2, audio(60, 13))
	other := join(tag("other"), audio(90, 21), show1, audio(120, 22), show2, audio(70, 23))

	result := mustDiff(t, home, other)
	if want := join(tag("home"), show1, show2); !bytes.Equal(result.Output, want) {
		t.Fatalf("output is %d bytes, want %d (home tag, show1, show2)", len(result.Output), len(want))
	}
	if len(result.Kept) != 2 || len(result.Removed) != 3 {
		t.Errorf("kept %d, removed %d segments; want 2 and 3", len(result.Kept), len(result.Removed))
	}
	if result.Removed[0].Start != 0 || result.Removed[0].End != 100 || result.Kept[0].Start != 100 || result.Kept[0].End != 500 {
		t.Errorf("segments = %+v / %+v", result.Kept, result.Removed)
	}
	if want := 700 * 26122448 * time.Nanosecond; result.Duration != want {
		t.Errorf("duration %s, want %s", result.Duration, want)
	}
	if parsed, err := mp3.Parse(result.Output); err != nil || len(parsed.Frames) != 700 || parsed.Skipped != 0 {
		t.Errorf("output doesn't parse cleanly: %v", err)
	}
}

func TestDiffSharedSilenceIsNotShow(t *testing.T) {
	// Both regions' ad breaks end in the same 3 seconds of silence: longer
	// than MinShared, so only the clean-start rule tells it from show audio.
	home := join(tag("h"), audio(100, 11), silence(120), show1, audio(80, 12), silence(120), show2)
	other := join(tag("o"), audio(90, 21), silence(120), show1, audio(60, 22), silence(120), show2)

	// This made-up episode is over 30% ads and silence, so the removal
	// limit is loosened for it.
	options := DefaultOptions()
	options.MaxRemovedShare = 0.6
	result, err := Diff(home, other, options)
	if err != nil {
		t.Fatal(err)
	}
	if want := join(tag("h"), show1, show2); !bytes.Equal(result.Output, want) {
		t.Errorf("silence before the show was kept: output %d bytes, want %d", len(result.Output), len(want))
	}
}

func TestDiffSharedJingleAfterShowIsCut(t *testing.T) {
	// Both ad breaks open with the same jingle (clean start), so the match
	// runs on past the end of the show until the ads themselves differ.
	jingle := audio(30, 99)
	home := join(tag("h"), show1, jingle, audio(100, 12)[frameSize:], show2)
	other := join(tag("o"), show1, jingle, audio(100, 22)[frameSize:], show2)

	result := mustDiff(t, home, other)
	if want := join(tag("h"), show1, show2); !bytes.Equal(result.Output, want) {
		t.Errorf("jingle after the show was kept: output %d bytes, want %d", len(result.Output), len(want))
	}
}

func TestDiffIdentical(t *testing.T) {
	episode := join(tag("h"), audio(50, 11), show1, show2)
	if _, err := Diff(episode, episode, DefaultOptions()); !errors.Is(err, ErrIdentical) {
		t.Errorf("same bytes: err = %v", err)
	}
	// Same audio, different tags: still nothing to remove.
	if _, err := Diff(episode, join(tag("other tag"), audio(50, 11), show1, show2), DefaultOptions()); !errors.Is(err, ErrIdentical) {
		t.Errorf("same audio: err = %v", err)
	}
}

func TestDiffHomeWithoutAds(t *testing.T) {
	home := join(tag("h"), show1, show2)
	other := join(tag("o"), audio(100, 21), show1, audio(80, 22), show2)
	result := mustDiff(t, home, other)
	if !bytes.Equal(result.Output, home) || len(result.Removed) != 0 {
		t.Errorf("home without ads changed: removed %+v", result.Removed)
	}
}

func TestDiffSameAdInBothRegionsStays(t *testing.T) {
	// A campaign running in both markets can't be told from the show: the
	// known limit (docs/region-diff.md), which a third region may resolve.
	campaign := audio(100, 50)
	home := join(tag("h"), campaign, show1, audio(80, 12), show2)
	other := join(tag("o"), campaign, show1, audio(90, 22), show2)
	result := mustDiff(t, home, other)
	if want := join(tag("h"), campaign, show1, show2); !bytes.Equal(result.Output, want) {
		t.Errorf("output %d bytes, want %d", len(result.Output), len(want))
	}
}

func TestDiffMinimumShared(t *testing.T) {
	short := audio(50, 7) // 1.3 s: shared, but shorter than MinShared
	home := join(tag("h"), show1, audio(80, 12), short, audio(80, 13), show2)
	other := join(tag("o"), show1, audio(90, 22), short, audio(70, 23), show2)
	result := mustDiff(t, home, other)
	if want := join(tag("h"), show1, show2); !bytes.Equal(result.Output, want) {
		t.Errorf("a shared run under MinShared was kept")
	}
}

func TestDiffSanityChecks(t *testing.T) {
	home := join(tag("h"), audio(300, 11), show1, audio(300, 12))
	other := join(tag("o"), audio(300, 21), show1, audio(300, 22))
	if _, err := Diff(home, other, DefaultOptions()); !errors.Is(err, ErrImplausible) || !strings.Contains(err.Error(), "remove") {
		t.Errorf("removing most of the episode: err = %v", err)
	}

	home = join(tag("h"), show1, audio(50, 12), show2)
	other = join(tag("o"), show1, audio(60, 22), show2)
	options := DefaultOptions()
	options.ExpectedDuration = 30 * time.Minute
	if _, err := Diff(home, other, options); !errors.Is(err, ErrImplausible) || !strings.Contains(err.Error(), "feed says") {
		t.Errorf("far from the stated duration: err = %v", err)
	}
	options.ExpectedDuration = 18 * time.Second // show1 + show2 = 18.3 s
	if _, err := Diff(home, other, options); err != nil {
		t.Errorf("close to the stated duration: %v", err)
	}

	if _, err := Diff(join(tag("h"), audio(200, 1)), join(tag("o"), audio(200, 2)), DefaultOptions()); !errors.Is(err, ErrImplausible) {
		t.Errorf("nothing shared: err = %v", err)
	}
}

func TestDiffUnsupported(t *testing.T) {
	episode := join(tag("h"), show1)
	if _, err := Diff([]byte("<html>error</html>"), episode, DefaultOptions()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("home not MP3: err = %v", err)
	}
	if _, err := Diff(episode, []byte("ftyp M4A"), DefaultOptions()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("other not MP3: err = %v", err)
	}
	// 48 kHz frames: header byte 2 = 0x94, 384 bytes each.
	var other48k []byte
	for range 20 {
		f := make([]byte, 384)
		copy(f, []byte{0xFF, 0xFB, 0x94, 0x00})
		other48k = append(other48k, f...)
	}
	if _, err := Diff(episode, other48k, DefaultOptions()); !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "differ in format") {
		t.Errorf("different sample rates: err = %v", err)
	}
}

func TestInOrderDropsCrossingAnchors(t *testing.T) {
	anchors := []run{{home: 0, other: 0}, {home: 10, other: 50}, {home: 20, other: 20}, {home: 30, other: 30}, {home: 40, other: 60}}
	got := inOrder(anchors)
	var homes []int
	for _, anchor := range got {
		homes = append(homes, anchor.home)
	}
	// {10, 50} crosses {20, 20} and {30, 30}; keeping it would lose two.
	if len(got) != 4 || homes[1] != 20 || homes[2] != 30 {
		t.Errorf("inOrder = %v", homes)
	}
}

// TestDiffLiveDownloads diffs the real Norway/Sweden downloads of an episode
// with Norwegian ads, when they are there (config/live is local only), and
// writes the cleaned episode next to them for listening.
func TestDiffLiveDownloads(t *testing.T) {
	directory := filepath.Join("..", "..", "config", "live")
	norway, err := os.ReadFile(filepath.Join(directory, "direct-1.mp3"))
	if err != nil {
		t.Skip("live downloads not present; run the live region test to fetch them")
	}
	sweden, err := os.ReadFile(filepath.Join(directory, "sweden-1.mp3"))
	if err != nil {
		t.Skip("live downloads not present")
	}
	options := DefaultOptions()
	options.ExpectedDuration = 39*time.Minute + 52*time.Second // the feed's itunes:duration

	start := time.Now()
	fromNorway, err := Diff(norway, sweden, options)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	t.Logf("diffed %.1f + %.1f MB in %s", float64(len(norway))/(1<<20), float64(len(sweden))/(1<<20), time.Since(start).Round(time.Millisecond))
	t.Logf("home %s, cleaned %s (feed says 39:52), %.1f MB", fromNorway.HomeDuration.Round(time.Second), fromNorway.Duration.Round(time.Second), float64(len(fromNorway.Output))/(1<<20))
	for _, segment := range fromNorway.Kept {
		t.Logf("  kept    frames %6d-%6d  %s", segment.Start, segment.End, segment.Duration.Round(time.Second))
	}
	for _, segment := range fromNorway.Removed {
		t.Logf("  removed frames %6d-%6d  %s", segment.Start, segment.End, segment.Duration.Round(100*time.Millisecond))
	}
	if len(fromNorway.Kept) != 3 || len(fromNorway.Removed) != 4 {
		t.Errorf("kept %d, removed %d; want the three show segments and four ad breaks", len(fromNorway.Kept), len(fromNorway.Removed))
	}

	// Either way round, the show audio is the same.
	fromSweden, err := Diff(sweden, norway, options)
	if err != nil {
		t.Fatalf("Diff the other way: %v", err)
	}
	norwayAudio, _ := mp3.Parse(fromNorway.Output)
	swedenAudio, _ := mp3.Parse(fromSweden.Output)
	if len(norwayAudio.Frames) != len(swedenAudio.Frames) {
		t.Fatalf("frames %d vs %d", len(norwayAudio.Frames), len(swedenAudio.Frames))
	}
	for i := range norwayAudio.Frames {
		if !bytes.Equal(norwayAudio.FrameBytes(norwayAudio.Frames[i]), swedenAudio.FrameBytes(swedenAudio.Frames[i])) {
			t.Fatalf("frame %d differs between the two directions", i)
		}
	}

	cleaned := filepath.Join(directory, "cleaned.mp3")
	if err := os.WriteFile(cleaned, fromNorway.Output, 0o644); err != nil {
		t.Errorf("write the cleaned episode: %v", err)
	} else {
		t.Logf("cleaned episode written to %s", cleaned)
	}
}
