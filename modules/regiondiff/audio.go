package regiondiff

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sort"
	"time"

	"aunefyren/solstein/mp3"
	"aunefyren/solstein/mp3/spectrum"
)

// Comparing by audio, for hosts that re-encode an episode when they insert
// ads (RedCircle): then no frame is shared, and the frame diff finds
// nothing. Both downloads' loudness over time (package spectrum) is lined
// up instead. The show is the same audio in both, so a stretch of the home
// download's loudness recurs in the other's at a constant offset, and the
// offset jumps at each ad break: up where the other has ads, down where the
// home download has. What of the home download has no counterpart is cut,
// at its own frame boundaries; nothing is re-encoded.
const (
	// envelopeStep is the fine time resolution of the loudness compared.
	envelopeStep = 10 * time.Millisecond
	// coarseFactor coarsens it for the search: 100 ms.
	coarseFactor = 10
	// windowSteps is how much of the home download is looked for in the
	// other at a time (coarse steps: 5 s), windowHop how far apart the
	// windows are (2.5 s).
	windowSteps = 50
	windowHop   = 25
	// acceptCorrelation is how well a window must match somewhere to count;
	// the best match must also beat the best elsewhere by uniqueMargin, so
	// a window that would fit several places (steady noise) is left out.
	acceptCorrelation = 0.85
	uniqueMargin      = 0.1
	// followCorrelation is enough to accept a window at the offset of the
	// window before, without searching everywhere.
	followCorrelation = 0.9
	// neighbourhood is how far (coarse steps) from the previous offset the
	// quick check looks.
	neighbourhood = 3
	// minAudioRun is the shortest stretch of matched audio kept as show.
	minAudioRun = 10 * time.Second
	// minAudioBreak is the shortest difference treated as an ad, in either
	// download. Around each place a re-encoding host inserts an ad, a second
	// or two of its copy differs from the original (seen with RedCircle:
	// 1.3 s at a mid-roll, 2.3 s where a post-roll starts); that is show
	// audio in the home download, and must stay. Ads are longer.
	minAudioBreak = 5 * time.Second
	// lineUpSlack is how much two runs may overlap in the other download,
	// from edges followed a little too far, before they are taken not to
	// line up.
	lineUpSlack = time.Second
	// edgeSmoothing is how much audio (fine steps: 0.3 s) is averaged when
	// following a stretch of show to its edge.
	edgeSmoothing = 30
	// searchMargin widens the range of offsets searched beyond what the
	// removed-share limit allows, for the other download's own ads.
	searchMargin = 60 * time.Second
	// quietBelow is how far under a download's median loudness (log energy:
	// 30 dB) everything counts as the same silence. An original's digital
	// silence and a re-encode's codec noise are both inaudible, but far
	// apart in log energy; without this, following a run to its edge would
	// stop at every pause.
	quietBelow = 30 * math.Ln10 / 10
	// searchBudget caps the correlation work (multiply-adds) of the full
	// searches, so a download that won't line up can't take the CPU for
	// long: about 10 s. The hardest of the real episodes checked needed a
	// fifth of it (1,084 full searches in 80 minutes); a 3-hour episode
	// searched in full at every window would need more.
	searchBudget = 1e10
)

// errSearchBudget means lining up took more work than searchBudget allows.
var errSearchBudget = fmt.Errorf("%w: the downloads' audio is too hard to line up", ErrImplausible)

// audioRun is a stretch of the home download found in the other: home
// steps [home, home+length) are other steps [home+offset, …).
type audioRun struct {
	home, length, offset int
	// bias is how much louder the home download is (log energy): encoders
	// differ in level.
	bias float64
}

func (r audioRun) end() int { return r.home + r.length }

// audioDiff compares two downloads by their loudness and cuts from home
// what other doesn't have.
func audioDiff(ctx context.Context, homeFile, otherFile mp3.File, homeLoudness, otherLoudness spectrum.Loudness, options Options) (Result, error) {
	home, other := quieted(homeLoudness.Envelope(envelopeStep)), quieted(otherLoudness.Envelope(envelopeStep))
	result := Result{HomeDuration: homeFile.Duration(), OtherDuration: otherFile.Duration(), ByAudio: true}
	runs, err := findRuns(ctx, home, other, options, searchBudget)
	if err != nil {
		return result, err
	}
	if len(runs) == 0 {
		return result, fmt.Errorf("%w: the downloads share no show audio, compared by audio too", ErrImplausible)
	}
	for i := range runs {
		runs[i] = refineRun(runs[i], home, other)
	}
	runs = extendRuns(runs, home, other)

	// What of home isn't matched is cut; what of other isn't is its ads.
	var cuts [][2]int
	previousEnd, previousOtherEnd := 0, 0
	for i, r := range runs {
		if r.home-previousEnd >= stepsOf(minAudioBreak) {
			cuts = append(cuts, [2]int{previousEnd, r.home})
		}
		otherStart := r.home + r.offset
		if gap := otherStart - previousOtherEnd; i > 0 && gap < -stepsOf(lineUpSlack) {
			return result, fmt.Errorf("%w: the downloads' audio doesn't line up in order", ErrImplausible)
		} else if gap >= stepsOf(minAudioBreak) {
			result.OtherExtra += time.Duration(gap) * envelopeStep
			result.OtherBreaks++
		}
		previousEnd, previousOtherEnd = max(previousEnd, r.end()), r.end()+r.offset
	}
	if len(home)-previousEnd >= stepsOf(minAudioBreak) {
		cuts = append(cuts, [2]int{previousEnd, len(home)})
	}
	if gap := len(other) - previousOtherEnd; gap >= stepsOf(minAudioBreak) {
		result.OtherExtra += time.Duration(gap) * envelopeStep
		result.OtherBreaks++
	}

	result.Kept, result.Removed = frameSegments(homeFile, cuts)
	for _, segment := range result.Kept {
		result.Duration += segment.Duration
	}
	if len(result.Removed) == 0 && result.OtherBreaks == 0 {
		return result, ErrIdentical
	}
	if err := check(result, options); err != nil {
		return result, err
	}
	if len(result.Removed) == 0 {
		// The home download has no ads the other lacks: keep it whole.
		result.Output = homeFile.Data
		return result, nil
	}
	var carried time.Duration
	result.Output, carried = write(homeFile, result.Kept)
	result.Duration += carried
	return result, nil
}

func stepsOf(duration time.Duration) int { return int(duration / envelopeStep) }

// findRuns looks for each window of the home download in the other and
// groups the windows found at one offset into runs, in order in both. It
// fails with errSearchBudget once the full searches have done budget
// multiply-adds, and with ctx's error when ctx is done.
func findRuns(ctx context.Context, home, other []float64, options Options, budget float64) ([]audioRun, error) {
	homeCoarse, otherCoarse := coarsen(home), coarsen(other)
	if len(homeCoarse) < windowSteps || len(otherCoarse) < windowSteps {
		return nil, nil
	}
	search := newWindowSearch(otherCoarse)
	coarseSecond := int(time.Second / envelopeStep / coarseFactor)
	share := options.MaxRemovedShare
	if share <= 0 || share > 1 {
		share = 1
	}
	homeAds := int(float64(len(homeCoarse))*share) + int(searchMargin.Seconds())*coarseSecond
	minOffset := -homeAds
	maxOffset := len(otherCoarse) - len(homeCoarse) + homeAds

	// Windows in silence or steady noise match anywhere: leave out those
	// much flatter than the episode's typical window.
	type window struct {
		start  int
		values []float64 // zero mean, unit norm
		spread float64
	}
	var windows []window
	var spreads []float64
	for start := 0; start+windowSteps <= len(homeCoarse); start += windowHop {
		values, spread := normalise(homeCoarse[start : start+windowSteps])
		windows = append(windows, window{start: start, values: values, spread: spread})
		spreads = append(spreads, spread)
	}
	slices.Sort(spreads)
	flat := spreads[len(spreads)/2] / 4

	type match struct{ start, offset int }
	var matches []match
	previous, havePrevious := 0, false
	work := 0.0
	for _, w := range windows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if w.spread < flat || w.values == nil {
			continue
		}
		if havePrevious {
			best, offset := search.best(w.values, w.start, previous-neighbourhood, previous+neighbourhood)
			if best >= followCorrelation {
				matches = append(matches, match{w.start, offset})
				previous = offset
				continue
			}
		}
		if work += float64(maxOffset-minOffset+1) * windowSteps; work > budget {
			return nil, errSearchBudget
		}
		best, offset, second := search.bestUnique(w.values, w.start, minOffset, maxOffset)
		if best >= acceptCorrelation && best-second >= uniqueMargin {
			matches = append(matches, match{w.start, offset})
			previous, havePrevious = offset, true
		}
	}

	// Consecutive windows at (nearly) one offset are one run; windows left
	// out in between (pauses) don't break it.
	type group struct {
		first, last int
		offsets     []int
	}
	var groups []group
	for _, m := range matches {
		if n := len(groups); n > 0 && abs(m.offset-groups[n-1].offsets[len(groups[n-1].offsets)-1]) <= 1 {
			groups[n-1].last = m.start
			groups[n-1].offsets = append(groups[n-1].offsets, m.offset)
			continue
		}
		groups = append(groups, group{first: m.start, last: m.start, offsets: []int{m.offset}})
	}
	var runs []audioRun
	minimum := int(minAudioRun / envelopeStep / coarseFactor)
	for _, g := range groups {
		length := g.last + windowSteps - g.first
		if length < minimum {
			continue
		}
		slices.Sort(g.offsets)
		runs = append(runs, audioRun{
			home:   g.first * coarseFactor,
			length: length * coarseFactor,
			offset: g.offsets[len(g.offsets)/2] * coarseFactor,
		})
	}
	return inOrderRuns(runs), nil
}

// inOrderRuns keeps the runs that are in order in both downloads with the
// most audio between them: a run out of order is a false match. Runs may
// overlap by a window at their edges, where a break falls inside one.
func inOrderRuns(runs []audioRun) []audioRun {
	if len(runs) == 0 {
		return nil
	}
	sort.Slice(runs, func(a, b int) bool { return runs[a].home < runs[b].home })
	slack := windowSteps * coarseFactor
	follows := func(before, after audioRun) bool {
		return after.home >= before.end()-slack && after.home+after.offset >= before.end()+before.offset-slack
	}
	best, previous := make([]int, len(runs)), make([]int, len(runs))
	for i := range runs {
		best[i], previous[i] = runs[i].length, -1
		for j := range i {
			if follows(runs[j], runs[i]) && best[j]+runs[i].length > best[i] {
				best[i], previous[i] = best[j]+runs[i].length, j
			}
		}
	}
	last := 0
	for i := range runs {
		if best[i] > best[last] {
			last = i
		}
	}
	var chain []audioRun
	for i := last; i >= 0; i = previous[i] {
		chain = append(chain, runs[i])
	}
	slices.Reverse(chain)

	// Neighbours at one offset are one run that a stray window split.
	merged := chain[:1]
	for _, r := range chain[1:] {
		if last := &merged[len(merged)-1]; abs(r.offset-last.offset) <= coarseFactor {
			last.length = max(last.end(), r.end()) - last.home
			continue
		}
		merged = append(merged, r)
	}
	return merged
}

// refineRun finds a run's offset to the fine step, and the level
// difference between the downloads over it.
func refineRun(r audioRun, home, other []float64) audioRun {
	// The middle of the run, up to a minute, is plenty to pin the offset.
	from, to := r.home, r.end()
	if span := stepsOf(time.Minute); to-from > span {
		from = r.home + (r.length-span)/2
		to = from + span
	}
	values, _ := normalise(home[from:to])
	if values == nil {
		return r
	}
	search := newWindowSearch(other)
	_, offset := search.best(values, from, r.offset-coarseFactor*2, r.offset+coarseFactor*2)
	r.offset = offset
	var differences []float64
	for p := r.home; p < r.end(); p++ {
		if q := p + r.offset; q >= 0 && q < len(other) {
			differences = append(differences, home[p]-other[q])
		}
	}
	if len(differences) > 0 {
		slices.Sort(differences)
		r.bias = differences[len(differences)/2]
	}
	return r
}

// extendRuns follows each run out to where its audio stops matching,
// bounded by its neighbours, so a cut falls at the break rather than at a
// window's edge.
func extendRuns(runs []audioRun, home, other []float64) []audioRun {
	for i := range runs {
		r := runs[i]
		mismatch := func(p int) float64 {
			q := p + r.offset
			if p < 0 || p >= len(home) || q < 0 || q >= len(other) {
				return math.Inf(1)
			}
			return math.Abs(home[p] - other[q] - r.bias)
		}
		// Typical error inside the run sets what "stops matching" means.
		var inside []float64
		for p := r.home; p < r.end(); p++ {
			inside = append(inside, mismatch(p))
		}
		slices.Sort(inside)
		limit := 4*inside[len(inside)/2] + 0.2
		// mean error over the edgeSmoothing steps from p
		matches := func(p int) bool {
			total := 0.0
			for q := p; q < p+edgeSmoothing; q++ {
				total += mismatch(q)
			}
			return total/edgeSmoothing < limit
		}

		// Back to the earliest point that still matches, but not past the
		// previous run's start; then forward over any of the window's
		// edge that overshot the break.
		lower := 0
		if i > 0 {
			lower = runs[i-1].home
		}
		start := r.home
		for start > lower && matches(start-1) {
			start--
		}
		for start < r.end() && !matches(start) {
			start++
		}
		// Likewise at the end, not past the next run's end.
		upper := len(home)
		if i+1 < len(runs) {
			upper = runs[i+1].end()
		}
		end := r.end()
		for end < upper && matches(end+1-edgeSmoothing) {
			end++
		}
		for end > start && !matches(end-edgeSmoothing) {
			end--
		}
		runs[i].home, runs[i].length = start, max(end-start, 0)
	}
	return runs
}

// frameSegments turns the cut stretches (envelope steps) into kept and
// removed frames of the home file: a frame goes with the stretch its middle
// falls in.
func frameSegments(file mp3.File, cuts [][2]int) (kept, removed []Segment) {
	cutAt := func(position time.Duration) bool {
		step := int(position / envelopeStep)
		for _, c := range cuts {
			if step >= c[0] && step < c[1] {
				return true
			}
		}
		return false
	}
	var at time.Duration
	var current *Segment
	var currentCut bool
	for i, frame := range file.Frames {
		duration := frame.Header.Duration()
		isCut := cutAt(at + duration/2)
		if current == nil || isCut != currentCut {
			if current != nil {
				if currentCut {
					removed = append(removed, *current)
				} else {
					kept = append(kept, *current)
				}
			}
			current, currentCut = &Segment{Start: i, End: i}, isCut
		}
		current.End = i + 1
		current.Duration += duration
		at += duration
	}
	if current != nil {
		if currentCut {
			removed = append(removed, *current)
		} else {
			kept = append(kept, *current)
		}
	}
	return kept, removed
}

// quieted raises everything more than quietBelow under the envelope's
// median to that level.
func quieted(envelope []float64) []float64 {
	if len(envelope) == 0 {
		return envelope
	}
	sorted := slices.Clone(envelope)
	slices.Sort(sorted)
	floor := sorted[len(sorted)/2] - quietBelow
	for i, value := range envelope {
		envelope[i] = max(value, floor)
	}
	return envelope
}

// coarsen averages the envelope over coarseFactor steps.
func coarsen(values []float64) []float64 {
	coarse := make([]float64, len(values)/coarseFactor)
	for i := range coarse {
		total := 0.0
		for _, v := range values[i*coarseFactor : (i+1)*coarseFactor] {
			total += v
		}
		coarse[i] = total / coarseFactor
	}
	return coarse
}

// normalise returns values minus their mean, scaled to unit length, and
// their standard deviation; nil when they are all equal.
func normalise(values []float64) ([]float64, float64) {
	mean := 0.0
	for _, v := range values {
		mean += v
	}
	mean /= float64(len(values))
	result := make([]float64, len(values))
	norm := 0.0
	for i, v := range values {
		result[i] = v - mean
		norm += result[i] * result[i]
	}
	if norm == 0 {
		return nil, 0
	}
	scale := 1 / math.Sqrt(norm)
	for i := range result {
		result[i] *= scale
	}
	return result, math.Sqrt(norm / float64(len(values)))
}

// windowSearch correlates normalised windows with every stretch of a
// series, using prefix sums for each stretch's mean and spread.
type windowSearch struct {
	values      []float64
	sum, square []float64 // prefix sums
}

func newWindowSearch(values []float64) windowSearch {
	search := windowSearch{values: values, sum: make([]float64, len(values)+1), square: make([]float64, len(values)+1)}
	for i, v := range values {
		search.sum[i+1] = search.sum[i] + v
		search.square[i+1] = search.square[i] + v*v
	}
	return search
}

// correlation of the normalised window with values[at : at+len(window)],
// or -1 when that stretch is out of range or flat.
func (search windowSearch) correlation(window []float64, at int) float64 {
	n := len(window)
	if at < 0 || at+n > len(search.values) {
		return -1
	}
	sum := search.sum[at+n] - search.sum[at]
	variance := search.square[at+n] - search.square[at] - sum*sum/float64(n)
	if variance <= 1e-12 {
		return -1
	}
	dot := 0.0
	for i, w := range window {
		dot += w * search.values[at+i]
	}
	return dot / math.Sqrt(variance)
}

// best is the best correlation of a window starting at start, for offsets
// in [from, to], and that offset.
func (search windowSearch) best(window []float64, start, from, to int) (float64, int) {
	best, offset := -2.0, from
	for d := from; d <= to; d++ {
		if c := search.correlation(window, start+d); c > best {
			best, offset = c, d
		}
	}
	return best, offset
}

// bestUnique is best over [from, to], plus the best correlation more than a
// window away from it: how clearly the window fits one place only.
func (search windowSearch) bestUnique(window []float64, start, from, to int) (best float64, offset int, second float64) {
	if to < from {
		return -2, from, -2
	}
	scores := make([]float64, to-from+1)
	best, offset = -2, from
	for i := range scores {
		scores[i] = search.correlation(window, start+from+i)
		if scores[i] > best {
			best, offset = scores[i], from+i
		}
	}
	second = -2
	for i, score := range scores {
		if abs(from+i-offset) > len(window) && score > second {
			second = score
		}
	}
	return best, offset, second
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
