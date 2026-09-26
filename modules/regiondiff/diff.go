// Package regiondiff is the region-diff module: it removes dynamically
// inserted ads from an episode by comparing two downloads of it made from
// different regions. The audio both share is the show; what differs is ads.
// Where the host splices ads in as whole frames without re-encoding (Acast,
// Dovetail), it compares the MP3 frames themselves. Where the host
// re-encodes the episode around its ads (RedCircle), no frame is shared,
// and it compares the downloads' loudness over time instead (audio.go).
// Either way the output is the home download's own frames: nothing is
// re-encoded.
package regiondiff

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/maphash"
	"sort"
	"time"

	"aunefyren/solstein/mp3"
	"aunefyren/solstein/mp3/spectrum"
)

var (
	// ErrIdentical means both downloads carry the same audio: no dynamic
	// ads, or the same ones in both regions. The caller may try another
	// region.
	ErrIdentical = errors.New("the two downloads have the same audio")
	// ErrImplausible means the result failed a sanity check and must not be
	// published as the cleaned episode.
	ErrImplausible = errors.New("the diff result is implausible")
	// ErrUnsupported means the downloads can't be diffed frame by frame.
	ErrUnsupported = errors.New("the downloads can't be compared frame by frame")
)

const (
	// anchorFrames is how many consecutive frames make an anchor: 16 frames
	// is about 0.4 s, long enough that a run of show audio is unique in the
	// file.
	anchorFrames = 16
	// maxMarker is the longest a break marker can be.
	maxMarker = 5 * time.Second
	// unanchoredMinimum is how long shared audio must be to count as show
	// without a clean frame to start or end on (see toSegmentBoundaries).
	// Identical stretches inside ad breaks — silence, jingles — are a few
	// seconds at most.
	unanchoredMinimum = 30 * time.Second
)

// Options tunes the diff and its sanity checks.
type Options struct {
	// MinShared is the shortest shared run that counts as show audio.
	MinShared time.Duration
	// MaxRemovedShare is the most of the home download that may be removed.
	MaxRemovedShare float64
	// ExpectedDuration is the episode's stated duration (itunes:duration),
	// zero if unknown. The result must be within DurationTolerance of it.
	ExpectedDuration  time.Duration
	DurationTolerance float64
	// CompareByAudio compares downloads that share no frame by their
	// loudness (audio.go); off, they fail as unsupported or implausible.
	CompareByAudio bool
	// TrimBreakMarkers also removes break markers: short spliced pieces the
	// regions share, repeated at the edges of the show segments (see
	// trimMarkers).
	TrimBreakMarkers bool
}

// DefaultOptions are the decided defaults (see docs/region-diff.md).
func DefaultOptions() Options {
	return Options{MinShared: 2 * time.Second, MaxRemovedShare: 0.3, DurationTolerance: 0.05, CompareByAudio: true}
}

// Segment is a range of frames of the home download, [Start, End).
type Segment struct {
	Start, End int
	Duration   time.Duration
}

// Result is a finished diff.
type Result struct {
	// Output is the cleaned episode: the home download's ID3 tags around
	// the shared frames, in order.
	Output []byte
	// Kept are the shared runs of the home download; Removed the rest.
	Kept, Removed []Segment
	// Markers are the break markers removed with TrimBreakMarkers; they
	// are part of Removed too.
	Markers []Segment
	// Durations of the home download, the other one and the output. The
	// output's includes the silent frames kept at cuts (see write).
	HomeDuration, OtherDuration, Duration time.Duration
	// ByAudio is set when the downloads were compared by their loudness,
	// because they share no frames (the host re-encodes).
	ByAudio bool
	// OtherExtra is how much audio the other download has that the home
	// download doesn't, in OtherBreaks stretches; only known by audio.
	OtherExtra  time.Duration
	OtherBreaks int
}

// errNoSharedFrames means the downloads share no frame at all: the host
// re-encodes them, so they are compared by audio instead.
var errNoSharedFrames = errors.New("the downloads share no frames")

// Comparison compares a home download with others: by frames where the
// host splices, by audio where it re-encodes. It keeps what it learned of
// the home download (its frames, its loudness) between comparisons, as the
// processor compares it with one fallback after another.
type Comparison struct {
	home     []byte
	options  Options
	homeFile *mp3.File
	loudness *spectrum.Loudness
}

// NewComparison starts comparisons of a home download.
func NewComparison(home []byte, options Options) *Comparison {
	return &Comparison{home: home, options: options}
}

// Diff is a comparison of one pair.
func Diff(home, other []byte, options Options) (Result, error) {
	return NewComparison(home, options).With(context.Background(), other)
}

// With removes from the home download everything it doesn't share with
// other. home is the download from the listener's own region (its tags are
// kept); other is from another region. Comparing by audio takes seconds;
// it stops with ctx's error when ctx is done.
func (comparison *Comparison) With(ctx context.Context, other []byte) (Result, error) {
	if bytes.Equal(comparison.home, other) {
		return Result{}, ErrIdentical
	}
	if comparison.homeFile == nil {
		homeFile, err := mp3.Parse(comparison.home)
		if err != nil {
			return Result{}, fmt.Errorf("%w: home download: %w", ErrUnsupported, err)
		}
		comparison.homeFile = &homeFile
	}
	homeFile := *comparison.homeFile
	otherFile, err := mp3.Parse(other)
	if err != nil {
		return Result{}, fmt.Errorf("%w: other download: %w", ErrUnsupported, err)
	}
	homeFormat, otherFormat := homeFile.Frames[0].Header, otherFile.Frames[0].Header
	// Where the audio can't be measured, the frame comparison's verdict
	// stands: no frames shared is retryable, as a fresh download may be
	// better; different formats are not.
	verdict := fmt.Errorf("%w: the downloads share no show audio", ErrImplausible)
	if homeFormat.Version == otherFormat.Version && homeFormat.Layer == otherFormat.Layer && homeFormat.SampleRate == otherFormat.SampleRate {
		result, err := frameDiff(homeFile, otherFile, comparison.options)
		if !errors.Is(err, errNoSharedFrames) {
			return result, err
		}
	} else {
		verdict = fmt.Errorf("%w: the downloads differ in format (%s layer %d %d Hz vs %s layer %d %d Hz)", ErrUnsupported,
			homeFormat.Version, homeFormat.Layer, homeFormat.SampleRate, otherFormat.Version, otherFormat.Layer, otherFormat.SampleRate)
	}
	if !comparison.options.CompareByAudio {
		return Result{}, verdict
	}
	result, err := comparison.byAudio(ctx, otherFile)
	if errors.Is(err, spectrum.ErrUnsupported) {
		return result, fmt.Errorf("%w, and can't be compared by audio (%w)", verdict, err)
	}
	return result, err
}

// byAudio measures both downloads' loudness, at the same time, and lines
// them up (audioDiff).
func (comparison *Comparison) byAudio(ctx context.Context, otherFile mp3.File) (Result, error) {
	var otherLoudness spectrum.Loudness
	var otherErr error
	measured := make(chan struct{})
	go func() {
		defer close(measured)
		otherLoudness, otherErr = spectrum.Measure(ctx, otherFile)
	}()
	if comparison.loudness == nil {
		loudness, err := spectrum.Measure(ctx, *comparison.homeFile)
		if err != nil {
			<-measured
			return Result{}, fmt.Errorf("home download: %w", err)
		}
		comparison.loudness = &loudness
	}
	<-measured
	if otherErr != nil {
		return Result{}, fmt.Errorf("other download: %w", otherErr)
	}
	return audioDiff(ctx, *comparison.homeFile, otherFile, *comparison.loudness, otherLoudness, comparison.options)
}

// run is a stretch of identical frames: home[home:home+length] equals
// other[other:other+length].
type run struct{ home, other, length int }

// frameDiff compares two downloads of the same format frame by frame.
func frameDiff(homeFile, otherFile mp3.File, options Options) (Result, error) {
	homeFormat := homeFile.Frames[0].Header
	seed := maphash.MakeSeed()
	homeHashes, otherHashes := frameHashes(homeFile, seed), frameHashes(otherFile, seed)
	runs := align(homeHashes, otherHashes)
	runs = verify(runs, homeFile, otherFile)
	if len(runs) == 0 {
		return Result{}, errNoSharedFrames
	}

	frameDuration := homeFormat.Duration()
	minimum := int((options.MinShared + frameDuration - 1) / frameDuration)
	unanchored := int(unanchoredMinimum / frameDuration)
	var kept []run
	for _, candidate := range runs {
		if trimmed, ok := toSegmentBoundaries(candidate, homeFile.Frames, unanchored); ok && trimmed.length >= max(minimum, 1) {
			kept = append(kept, trimmed)
		}
	}

	result := Result{HomeDuration: homeFile.Duration(), OtherDuration: otherFile.Duration()}
	result.Kept, result.Removed = segments(kept, homeFile)
	for _, segment := range result.Kept {
		result.Duration += segment.Duration
	}

	otherShared := 0
	for _, segment := range kept {
		otherShared += segment.length
	}
	if len(result.Removed) == 0 && otherShared == len(otherFile.Frames) {
		return result, ErrIdentical
	}
	if options.TrimBreakMarkers {
		var markers []run
		kept, markers = trimMarkers(kept, homeFile, frameDuration)
		result.Kept, result.Removed = segments(kept, homeFile)
		result.Duration = 0
		for _, segment := range result.Kept {
			result.Duration += segment.Duration
		}
		for _, marker := range markers {
			result.Markers = append(result.Markers, Segment{Start: marker.home, End: marker.home + marker.length, Duration: time.Duration(marker.length) * frameDuration})
		}
	}
	if err := check(result, options); err != nil {
		return result, err
	}
	var carried time.Duration
	result.Output, carried = write(homeFile, result.Kept)
	result.Duration += carried
	return result, nil
}

func frameHashes(file mp3.File, seed maphash.Seed) []uint64 {
	hashes := make([]uint64, len(file.Frames))
	for i, frame := range file.Frames {
		hashes[i] = maphash.Bytes(seed, file.FrameBytes(frame))
	}
	return hashes
}

// align finds the runs of frames the two sequences share, in order. It
// anchors on groups of anchorFrames frames that occur exactly once in each
// sequence, keeps the largest set of anchors that is in order in both (a
// longest increasing subsequence), and extends each into the longest run of
// equal frames around it.
func align(home, other []uint64) []run {
	homeGrams, otherGrams := grams(home), grams(other)
	count := func(values []uint64) map[uint64]int {
		counts := make(map[uint64]int, len(values))
		for _, value := range values {
			counts[value]++
		}
		return counts
	}
	homeCount, otherCount := count(homeGrams), count(otherGrams)
	otherIndex := make(map[uint64]int, len(otherGrams))
	for j, gram := range otherGrams {
		if otherCount[gram] == 1 {
			otherIndex[gram] = j
		}
	}
	var anchors []run
	for i, gram := range homeGrams {
		if homeCount[gram] != 1 {
			continue
		}
		if j, ok := otherIndex[gram]; ok {
			anchors = append(anchors, run{home: i, other: j, length: anchorFrames})
		}
	}
	anchors = inOrder(anchors)

	var runs []run
	for _, anchor := range anchors {
		// An anchor inside the previous run, in either file, is already
		// covered by it (or is repeated audio that would overlap it).
		if last := len(runs) - 1; last >= 0 {
			previous := runs[last]
			if anchor.home < previous.home+previous.length || anchor.other < previous.other+previous.length {
				continue
			}
		}
		start := run{home: anchor.home, other: anchor.other}
		lowerHome, lowerOther := 0, 0
		if last := len(runs) - 1; last >= 0 {
			lowerHome, lowerOther = runs[last].home+runs[last].length, runs[last].other+runs[last].length
		}
		for start.home > lowerHome && start.other > lowerOther && home[start.home-1] == other[start.other-1] {
			start.home--
			start.other--
		}
		end := anchor.home + anchor.length
		for end < len(home) && end-anchor.home+anchor.other < len(other) && home[end] == other[end-anchor.home+anchor.other] {
			end++
		}
		start.length = end - start.home
		runs = append(runs, start)
	}
	return runs
}

// grams hashes every group of anchorFrames consecutive frame hashes.
func grams(hashes []uint64) []uint64 {
	if len(hashes) < anchorFrames {
		return nil
	}
	result := make([]uint64, len(hashes)-anchorFrames+1)
	for i := range result {
		var h uint64 = 14695981039346656037
		for _, value := range hashes[i : i+anchorFrames] {
			h = (h ^ value) * 1099511628211
		}
		result[i] = h
	}
	return result
}

// inOrder keeps the largest subset of anchors (sorted by home position)
// whose other positions also increase: anchors out of order would pair show
// audio with the wrong place in the other file.
func inOrder(anchors []run) []run {
	sort.Slice(anchors, func(a, b int) bool { return anchors[a].home < anchors[b].home })
	tails := []int{}                      // tails[k]: index of the smallest end of an increasing run of length k+1
	previous := make([]int, len(anchors)) // back pointers
	for i, anchor := range anchors {
		k := sort.Search(len(tails), func(k int) bool { return anchors[tails[k]].other >= anchor.other })
		if k > 0 {
			previous[i] = tails[k-1]
		} else {
			previous[i] = -1
		}
		if k == len(tails) {
			tails = append(tails, i)
		} else {
			tails[k] = i
		}
	}
	if len(tails) == 0 {
		return nil
	}
	result := make([]run, len(tails))
	for i, k := tails[len(tails)-1], len(tails)-1; k >= 0; i, k = previous[i], k-1 {
		result[k] = anchors[i]
	}
	return result
}

// verify compares the bytes of every run, so a hash collision can't slip
// other audio into the output; a run is cut at the first frame that differs.
func verify(runs []run, home, other mp3.File) []run {
	verified := runs[:0]
	for _, candidate := range runs {
		length := 0
		for length < candidate.length && bytes.Equal(
			home.FrameBytes(home.Frames[candidate.home+length]),
			other.FrameBytes(other.Frames[candidate.other+length])) {
			length++
		}
		if length > 0 {
			candidate.length = length
			verified = append(verified, candidate)
		}
	}
	return verified
}

// toSegmentBoundaries trims a run to where segments can really start and
// end. Acast splices at frames that borrow nothing from earlier ones
// (main_data_begin 0), so a segment of show audio starts on such a frame (or
// at the start of the file), and ends where the next segment starts, on
// another, or at the end of the file. A run that spills past a splice point —
// because both ad breaks happen to end in the same silence, say — is trimmed
// back to it; a short run with no such boundary inside is dropped.
//
// Not every host splices that way: PRX's Dovetail cuts the show's own
// stream at the ad's cue point, so the show resumes on a frame that borrows
// from the one before. A run is therefore kept from where it really starts
// (or up to where it really ends) when the stretch before its first clean
// frame (or after its last) is at least unanchored frames long: shared
// audio that long is show, not silence or a jingle. The frame after such a
// cut decodes against the wrong reservoir bytes — a 26 ms glitch the
// host's own file has at the same place, since it borrows from the ad.
func toSegmentBoundaries(candidate run, frames []mp3.Frame, unanchored int) (run, bool) {
	start, end := candidate.home, candidate.home+candidate.length
	clean := func(i int) bool { return i == 0 || i == len(frames) || frames[i].MainDataBegin == 0 }

	first := start
	for first < end && !clean(first) {
		first++
	}
	if first-start >= unanchored {
		first = start // a long stretch before any clean frame: show audio
	}
	if first == end {
		return run{}, false
	}
	start = first

	if !clean(end) {
		last := end - 1 // the last segment start inside the run
		for last > start && !clean(last) {
			last--
		}
		switch {
		case end-last >= unanchored:
			// A long stretch after the last clean frame: show audio up to
			// where the match ends.
		case last == start:
			return run{}, false
		default:
			end = last
		}
	}
	shift := start - candidate.home
	return run{home: start, other: candidate.other + shift, length: end - start}, true
}

// trimMarkers removes break markers from the kept runs: the chimes or
// stings a host splices in around ad breaks, which are the same in every
// region and so survive the diff. A piece is taken for a marker only when
// all of these hold, so show audio is never cut:
//   - it is a whole spliced segment: it starts on a clean frame (or the
//     file's first) and the frame after it starts clean (or the file ends),
//     so cutting it out leaves no decode error either side;
//   - it is at the edge of a kept run, next to removed audio or the file's
//     start or end;
//   - it is at most maxMarker long;
//   - its frames are byte-identical to another such piece in the episode.
//
// A marker encoded into the start of a show segment (no clean frame after
// it) can't be cut without breaking the frame after it, and is kept.
func trimMarkers(kept []run, file mp3.File, frameDuration time.Duration) ([]run, []run) {
	longest := int(maxMarker / frameDuration)
	cleanAt := func(i int) bool { return i == 0 || i == len(file.Frames) || file.Frames[i].MainDataBegin == 0 }

	// Candidate pieces: [start, end) of the home file.
	type piece struct{ start, end int }
	var pieces []piece
	for i, candidate := range kept {
		start, end := candidate.home, candidate.home+candidate.length
		// An edge counts only next to removed audio or the file's ends.
		openStart := i == 0 || kept[i-1].home+kept[i-1].length < start
		openEnd := i == len(kept)-1 || kept[i+1].home > end
		first := start + 1 // first segment start inside the run
		for first < end && !cleanAt(first) {
			first++
		}
		if first == end {
			// The run is one segment: a candidate only as a whole.
			if (openStart || openEnd) && end-start <= longest {
				pieces = append(pieces, piece{start, end})
			}
			continue
		}
		if openStart && first-start <= longest {
			pieces = append(pieces, piece{start, first})
		}
		last := end - 1 // last segment start inside the run
		for last > first && !cleanAt(last) {
			last--
		}
		if openEnd && end-last <= longest && last >= first {
			pieces = append(pieces, piece{last, end})
		}
	}

	same := func(a, b piece) bool {
		if a.end-a.start != b.end-b.start {
			return false
		}
		for i := range a.end - a.start {
			if !bytes.Equal(file.FrameBytes(file.Frames[a.start+i]), file.FrameBytes(file.Frames[b.start+i])) {
				return false
			}
		}
		return true
	}
	remove := map[piece]bool{}
	for i, a := range pieces {
		for _, b := range pieces[i+1:] {
			if a != b && same(a, b) {
				remove[a], remove[b] = true, true
			}
		}
	}
	if len(remove) == 0 {
		return kept, nil
	}

	var result, markers []run
	for _, candidate := range kept {
		start, end := candidate.home, candidate.home+candidate.length
		for marker := range remove {
			if marker.start == start && marker.end <= end {
				markers = append(markers, run{home: marker.start, length: marker.end - marker.start})
				start = marker.end
			}
		}
		for marker := range remove {
			if marker.end == end && marker.start >= start && marker.start > candidate.home {
				markers = append(markers, run{home: marker.start, length: marker.end - marker.start})
				end = marker.start
			}
		}
		if end > start {
			shift := start - candidate.home
			result = append(result, run{home: start, other: candidate.other + shift, length: end - start})
		}
	}
	sort.Slice(markers, func(a, b int) bool { return markers[a].home < markers[b].home })
	return result, markers
}

// segments turns kept runs into kept and removed ranges of the home file.
func segments(kept []run, file mp3.File) (keptSegments, removed []Segment) {
	duration := func(start, end int) time.Duration {
		var total time.Duration
		for _, frame := range file.Frames[start:end] {
			total += frame.Header.Duration()
		}
		return total
	}
	position := 0
	for _, segment := range kept {
		if segment.home > position {
			removed = append(removed, Segment{Start: position, End: segment.home, Duration: duration(position, segment.home)})
		}
		end := segment.home + segment.length
		keptSegments = append(keptSegments, Segment{Start: segment.home, End: end, Duration: duration(segment.home, end)})
		position = end
	}
	if position < len(file.Frames) {
		removed = append(removed, Segment{Start: position, End: len(file.Frames), Duration: duration(position, len(file.Frames))})
	}
	return keptSegments, removed
}

// check applies the sanity checks that stand between a diff and publishing.
func check(result Result, options Options) error {
	if len(result.Kept) == 0 {
		return fmt.Errorf("%w: the downloads share no show audio", ErrImplausible)
	}
	removedShare := 1 - float64(result.Duration)/float64(result.HomeDuration)
	if options.MaxRemovedShare > 0 && removedShare > options.MaxRemovedShare {
		return fmt.Errorf("%w: it would remove %.0f%% of the episode (at most %.0f%% allowed)", ErrImplausible, removedShare*100, options.MaxRemovedShare*100)
	}
	// Only too short is implausible: that is a bad cut. The result can't be
	// longer than the home download, so being longer than stated just means
	// ads the same in both regions are left in — still better than all of
	// them (the processor notes it).
	if options.ExpectedDuration > 0 && options.DurationTolerance > 0 {
		off := float64(result.Duration-options.ExpectedDuration) / float64(options.ExpectedDuration)
		if off < -options.DurationTolerance {
			return fmt.Errorf("%w: the result is %s long, but the feed says %s", ErrImplausible, result.Duration.Round(time.Second), options.ExpectedDuration.Round(time.Second))
		}
	}
	return nil
}

// write builds the output: the home file's ID3v2 tag, the kept frames, and
// its ID3v1 tag. Frames are copied one by one, so junk between them is left
// behind. An info frame isn't copied: it describes the uncut file.
//
// A kept frame after a cut may borrow bytes from the removed frames before
// it (the bit reservoir); a re-encoded file's frames nearly all do. Those
// removed frames are kept as silent frames (mp3.File.SilentFrame), so the
// borrowed bytes are still where the frame looks for them and the cut
// decodes cleanly: a moment of silence (26 ms a frame, usually one or two)
// instead of a corrupted frame. It returns the output and how long the
// silent frames play.
func write(file mp3.File, kept []Segment) ([]byte, time.Duration) {
	size := len(file.ID3v2) + len(file.ID3v1)
	for _, segment := range kept {
		for _, frame := range file.Frames[segment.Start:segment.End] {
			size += frame.Size
		}
	}
	output := make([]byte, 0, size+4*1500)
	output = append(output, file.ID3v2...)
	var silent time.Duration
	keptUpTo := 0 // frames before this were written
	for _, segment := range kept {
		first := carriers(file, keptUpTo, segment.Start)
		for _, frame := range file.Frames[first:segment.Start] {
			output = append(output, file.SilentFrame(frame)...)
			silent += frame.Header.Duration()
		}
		for _, frame := range file.Frames[segment.Start:segment.End] {
			output = append(output, file.FrameBytes(frame)...)
		}
		keptUpTo = segment.End
	}
	return append(output, file.ID3v1...), silent
}

// carriers is the first of the removed frames [removedFrom, start) that
// have to stay, as silent frames, for frame start to find the bytes it
// borrows: as few as carry that many bytes. Frames before removedFrom are
// in the output already, right before them.
func carriers(file mp3.File, removedFrom, start int) int {
	if start <= removedFrom || start >= len(file.Frames) {
		return start
	}
	need := file.Frames[start].MainDataBegin
	first := start
	for carried := 0; carried < need && first > removedFrom; {
		first--
		carried += file.Frames[first].MainDataSize()
	}
	return first
}
