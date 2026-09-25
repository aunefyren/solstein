// Package regiondiff is the region-diff module: it removes dynamically
// inserted ads from an episode by comparing two downloads of it made from
// different regions. The audio both share is the show; what differs is ads.
// It works on MP3 frames directly, because hosts such as Acast splice ads in
// as whole frames without re-encoding; nothing is decoded or re-encoded.
package regiondiff

import (
	"bytes"
	"errors"
	"fmt"
	"hash/maphash"
	"sort"
	"time"

	"aunefyren/solstein/mp3"
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

// anchorFrames is how many consecutive frames make an anchor: 16 frames is
// about 0.4 s, long enough that a run of show audio is unique in the file.
const anchorFrames = 16

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
}

// DefaultOptions are the decided defaults (see docs/design.md).
func DefaultOptions() Options {
	return Options{MinShared: 2 * time.Second, MaxRemovedShare: 0.3, DurationTolerance: 0.05}
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
	// Durations of the home download, the other one and the output.
	HomeDuration, OtherDuration, Duration time.Duration
}

// run is a stretch of identical frames: home[home:home+length] equals
// other[other:other+length].
type run struct{ home, other, length int }

// Diff removes from home everything it doesn't share with other. home is
// the download from the listener's own region (its tags are kept); other is
// from another region.
func Diff(home, other []byte, options Options) (Result, error) {
	if bytes.Equal(home, other) {
		return Result{}, ErrIdentical
	}
	homeFile, err := mp3.Parse(home)
	if err != nil {
		return Result{}, fmt.Errorf("%w: home download: %w", ErrUnsupported, err)
	}
	otherFile, err := mp3.Parse(other)
	if err != nil {
		return Result{}, fmt.Errorf("%w: other download: %w", ErrUnsupported, err)
	}
	homeFormat, otherFormat := homeFile.Frames[0].Header, otherFile.Frames[0].Header
	if homeFormat.Version != otherFormat.Version || homeFormat.Layer != otherFormat.Layer || homeFormat.SampleRate != otherFormat.SampleRate {
		return Result{}, fmt.Errorf("%w: the downloads differ in format (%s layer %d %d Hz vs %s layer %d %d Hz)", ErrUnsupported,
			homeFormat.Version, homeFormat.Layer, homeFormat.SampleRate, otherFormat.Version, otherFormat.Layer, otherFormat.SampleRate)
	}

	seed := maphash.MakeSeed()
	homeHashes, otherHashes := frameHashes(homeFile, seed), frameHashes(otherFile, seed)
	runs := align(homeHashes, otherHashes)
	runs = verify(runs, homeFile, otherFile)

	frameDuration := homeFormat.Duration()
	minimum := int((options.MinShared + frameDuration - 1) / frameDuration)
	var kept []run
	for _, candidate := range runs {
		if trimmed, ok := toSegmentBoundaries(candidate, homeFile.Frames); ok && trimmed.length >= max(minimum, 1) {
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
	if err := check(result, options); err != nil {
		return result, err
	}
	result.Output = write(homeFile, result.Kept)
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
// end. Hosts splice at frames that borrow nothing from earlier ones
// (main_data_begin 0), so a segment of show audio starts on such a frame (or
// at the start of the file), and ends where the next segment starts, on
// another, or at the end of the file. A run that spills past a splice point —
// because both ad breaks happen to end in the same silence, say — is trimmed
// back to it; a run with no such boundary inside is dropped.
func toSegmentBoundaries(candidate run, frames []mp3.Frame) (run, bool) {
	start, end := candidate.home, candidate.home+candidate.length
	for start < end && start != 0 && frames[start].MainDataBegin != 0 {
		start++
	}
	if start == end {
		return run{}, false
	}
	if end != len(frames) && frames[end].MainDataBegin != 0 {
		// Cut before the last segment start inside the run.
		cut := end - 1
		for cut > start && frames[cut].MainDataBegin != 0 {
			cut--
		}
		if cut == start {
			return run{}, false
		}
		end = cut
	}
	shift := start - candidate.home
	return run{home: start, other: candidate.other + shift, length: end - start}, true
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
	if options.ExpectedDuration > 0 && options.DurationTolerance > 0 {
		off := float64(result.Duration-options.ExpectedDuration) / float64(options.ExpectedDuration)
		if off > options.DurationTolerance || off < -options.DurationTolerance {
			return fmt.Errorf("%w: the result is %s long, but the feed says %s", ErrImplausible, result.Duration.Round(time.Second), options.ExpectedDuration.Round(time.Second))
		}
	}
	return nil
}

// write builds the output: the home file's ID3v2 tag, the kept frames, and
// its ID3v1 tag. Frames are copied one by one, so junk between them is left
// behind. An info frame isn't copied: it describes the uncut file.
func write(file mp3.File, kept []Segment) []byte {
	size := len(file.ID3v2) + len(file.ID3v1)
	for _, segment := range kept {
		for _, frame := range file.Frames[segment.Start:segment.End] {
			size += frame.Size
		}
	}
	output := make([]byte, 0, size)
	output = append(output, file.ID3v2...)
	for _, segment := range kept {
		for _, frame := range file.Frames[segment.Start:segment.End] {
			output = append(output, file.FrameBytes(frame)...)
		}
	}
	return append(output, file.ID3v1...)
}
