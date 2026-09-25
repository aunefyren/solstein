package regiondiff

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"aunefyren/solstein/episodes"
	"aunefyren/solstein/logger"
	"aunefyren/solstein/models"
	"aunefyren/solstein/mp3"
)

// ProcessorOptions configures the region-diff processor. A feed's own
// settings (models.Feed's RegionDiff fields) override Enabled, Exits and
// HideOnFailure.
type ProcessorOptions struct {
	// Enabled is whether feeds that don't say otherwise are processed.
	Enabled bool
	// Exits are the pair every episode is downloaded through: the home
	// region's exit first (its download is the one kept), then one in
	// another ad market.
	Exits [2]string
	// FallbackExits are tried in order when the pair's downloads carry the
	// same audio, in case the same ads run in both markets.
	FallbackExits []string
	Diff          Options
	// HideOnFailure keeps an episode out of the feed when processing fails
	// for good, instead of publishing it with its ads.
	HideOnFailure bool
	// Locator says which country each exit comes out in, so two exits in
	// the same country are never compared; nil skips the check.
	Locator Locator
	// FailureDir, when set, keeps the downloads of attempts whose diff
	// failed, for a look at what the host sent (see keepFailed).
	FailureDir string
	// SuccessDir, when set, keeps the downloads of diffs that worked too,
	// with where they cut (see keepSuccessful).
	SuccessDir string
}

// Locator says where exits come out; outbound.Manager is one. The direct
// exit's country is always unknown.
type Locator interface {
	ExitCountries(exit string) (countries []string, known bool)
	ExitCountry(exit string) (country string, known bool)
}

// Processor is region diff as an episode processor for the pipeline.
type Processor struct {
	options ProcessorOptions
}

// NewProcessor checks the options and builds a Processor. Every exit must be
// named, and none may appear twice: two downloads through one exit come from
// one region, so they could never differ.
func NewProcessor(options ProcessorOptions) (*Processor, error) {
	all := append([]string{options.Exits[0], options.Exits[1]}, options.FallbackExits...)
	for i, exit := range all {
		if exit == "" {
			return nil, errors.New("region diff: every exit must be named")
		}
		if slices.Contains(all[:i], exit) {
			return nil, fmt.Errorf("region diff: exit %q is listed more than once; each download must come from a different exit", exit)
		}
	}
	return &Processor{options: options}, nil
}

func (processor *Processor) Name() string { return "region diff" }

// Handles reports whether region diff applies to a feed: its own switch, or
// the global one.
func (processor *Processor) Handles(feed models.Feed) bool {
	switch feed.RegionDiff {
	case "on":
		return true
	case "off":
		return false
	}
	return processor.options.Enabled
}

// HideOnFailure is the feed's failure policy, or the global one.
func (processor *Processor) HideOnFailure(feed models.Feed) bool {
	if feed.RegionDiffOnFailure != "" {
		return feed.RegionDiffOnFailure == "hide"
	}
	return processor.options.HideOnFailure
}

// algorithmVersion is part of the recipe: bump it when a change to the diff
// would cut differently, so episodes cut before are cut again. 2: long
// shared runs are kept without a clean frame to start or end on (Dovetail).
// 3: a result longer than the stated duration is no longer implausible.
const algorithmVersion = 3

// Recipe describes the settings that shape a feed's cleaned episodes, for
// episodes.Processor. The failure policy is left out: it doesn't change a
// cleaned file.
func (processor *Processor) Recipe(feed models.Feed) string {
	pair, fallbacks := processor.exitsFor(feed)
	recipe := fmt.Sprintf("v%d %s→%s", algorithmVersion, pair[0], pair[1])
	if len(fallbacks) > 0 {
		recipe += ", fallback " + strings.Join(fallbacks, ", ")
	}
	options := processor.options.Diff
	recipe += fmt.Sprintf(", shared ≥%s, removed ≤%g%%", options.MinShared, options.MaxRemovedShare*100)
	if processor.trimsMarkers(feed) {
		return recipe + ", markers trimmed"
	}
	return recipe
}

// exitsFor is the feed's exit pair and the fallbacks to try with it. A
// fallback that is part of the feed's own pair is skipped.
func (processor *Processor) exitsFor(feed models.Feed) (pair [2]string, fallbacks []string) {
	pair = processor.options.Exits
	if len(feed.RegionDiffExits) == 2 {
		pair = [2]string{feed.RegionDiffExits[0], feed.RegionDiffExits[1]}
	}
	for _, exit := range processor.options.FallbackExits {
		if exit != pair[0] && exit != pair[1] {
			fallbacks = append(fallbacks, exit)
		}
	}
	return pair, fallbacks
}

// Process downloads the episode through both exits at once and removes the
// audio they don't share. When both carry the same audio, the fallback
// exits are tried in turn; if every one agrees, the episode is kept as it
// is, since it most likely has no dynamic ads. Files that can't be diffed
// frame by frame fail for good; an implausible result is retried, as the
// next downloads may carry other ads.
func (processor *Processor) Process(ctx context.Context, job episodes.Job) (episodes.Processed, error) {
	pair, fallbacks := processor.exitsFor(job.Feed)
	pair, fallbacks, err := processor.inDifferentCountries(pair, fallbacks)
	if err != nil {
		return episodes.Processed{}, err
	}
	home, other, compared, fallbacks, err := processor.fetchPair(ctx, job, pair, fallbacks)
	if err != nil {
		return episodes.Processed{}, err
	}
	home.duration, other.duration = audioDuration(home.Data), audioDuration(other.Data)
	home = processor.recheck(ctx, job, pair[0], home, other.duration)
	other = processor.recheck(ctx, job, compared, other, home.duration)

	options := processor.options.Diff
	options.ExpectedDuration = job.ExpectedDuration
	options.TrimBreakMarkers = processor.trimsMarkers(job.Feed)
	against := other
	result, err := Diff(home.Data, other.Data, options)
	for _, exit := range fallbacks {
		if !errors.Is(err, ErrIdentical) {
			break
		}
		fallback, fetchErr := job.Fetch(ctx, exit, job.Fresh)
		if fetchErr != nil {
			return episodes.Processed{}, fmt.Errorf("download through fallback exit %q: %w", exit, fetchErr)
		}
		fetched := checkedDownload{Download: fallback, duration: audioDuration(fallback.Data)}
		compared, against = exit, processor.recheck(ctx, job, exit, fetched, home.duration)
		result, err = Diff(home.Data, against.Data, options)
	}

	switch {
	case errors.Is(err, ErrIdentical):
		note := identicalNote(home.Data, options)
		processor.keepSuccessful(job, note, nil, map[string]checkedDownload{pair[0]: home, compared: against})
		return episodes.Processed{Audio: home.Data, ContentType: home.ContentType, Note: note}, nil
	case err != nil:
		// Say what the downloads were, so a bad download can be told from
		// a bad diff, and keep them if asked to.
		err = fmt.Errorf("%w (%s: %s; %s: %s)", err, pair[0], home.describe(), compared, against.describe())
		processor.keepFailed(job, err, map[string]checkedDownload{pair[0]: home, compared: against})
		if errors.Is(err, ErrUnsupported) {
			return episodes.Processed{}, fmt.Errorf("%w: %w", episodes.ErrPermanent, err)
		}
		return episodes.Processed{}, err
	}

	var removed time.Duration
	for _, segment := range result.Removed {
		removed += segment.Duration
	}
	note := fmt.Sprintf("removed %s of ads in %s, comparing %s with %s",
		removed.Round(time.Second), plural(len(result.Removed), "break"), pair[0], compared)
	if len(result.Markers) > 0 {
		var markers time.Duration
		for _, marker := range result.Markers {
			markers += marker.Duration
		}
		note += fmt.Sprintf(", including %s (%s)", plural(len(result.Markers), "break marker"), markers.Round(time.Second))
	}
	if excess := result.Duration - options.ExpectedDuration; options.ExpectedDuration > 0 && float64(excess) > float64(options.ExpectedDuration)*options.DurationTolerance {
		note += fmt.Sprintf("; still %s longer than stated: ads the same in every compared region may be left", excess.Round(time.Second))
	}
	processor.keepSuccessful(job, note, &result, map[string]checkedDownload{pair[0]: home, compared: against})
	return episodes.Processed{
		Audio:       result.Output,
		ContentType: home.ContentType,
		Duration:    result.Duration,
		Note:        note,
	}, nil
}

// checkedDownload is a download with its playing time, zero when it isn't
// MPEG audio.
type checkedDownload struct {
	episodes.Download
	duration time.Duration
}

func (download checkedDownload) describe() string {
	length := "not MP3"
	if download.duration > 0 {
		length = download.duration.Round(time.Second).String()
	}
	return fmt.Sprintf("%.1f MB, %s", float64(len(download.Data))/(1<<20), length)
}

func audioDuration(data []byte) time.Duration {
	file, err := mp3.Parse(data)
	if err != nil {
		return 0
	}
	return file.Duration()
}

// incompleteShare is how much shorter than expected a download may be before
// it is taken for a broken one. Stated durations leave out dynamic ads, so a
// whole download is never much shorter.
const incompleteShare = 0.8

// incomplete explains why a download looks like it isn't the whole
// episode, or returns "". expected is the feed's stated duration (zero if
// unknown), other the other download's.
func incomplete(duration, expected, other time.Duration) string {
	switch {
	case duration == 0 && other > 0:
		return "it isn't MP3 audio, but the other download is"
	case expected > 0 && float64(duration) < incompleteShare*float64(expected):
		return fmt.Sprintf("it plays %s, but the feed says %s", duration.Round(time.Second), expected.Round(time.Second))
	case expected == 0 && other > 0 && float64(duration) < incompleteShare*float64(other):
		return fmt.Sprintf("it plays %s, but the other download %s", duration.Round(time.Second), other.Round(time.Second))
	}
	return ""
}

// recheck downloads through an exit again, asking for a fresh copy, when
// the download looks incomplete: a host or its CDN serving a cut-off or
// wrong file would otherwise make the diff implausible. If the second
// download looks no better (or fails), the first is kept and the diff
// decides.
func (processor *Processor) recheck(ctx context.Context, job episodes.Job, exit string, download checkedDownload, other time.Duration) checkedDownload {
	reason := incomplete(download.duration, job.ExpectedDuration, other)
	if reason == "" {
		return download
	}
	logger.Log.Warn(fmt.Sprintf("Region diff: the download of '%s' through %s looks incomplete (%s); downloading it again.", job.Episode.Title, exit, reason))
	again, err := job.Fetch(ctx, exit, true)
	if err != nil {
		logger.Log.Warn(fmt.Sprintf("Region diff: downloading '%s' through %s again failed. Error: %s", job.Episode.Title, exit, err))
		return download
	}
	retried := checkedDownload{Download: again, duration: audioDuration(again.Data)}
	if incomplete(retried.duration, job.ExpectedDuration, other) != "" && retried.duration <= download.duration {
		return download
	}
	return retried
}

// plural is "1 break" or "4 breaks".
func plural(count int, noun string) string {
	if count == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", count, noun)
}

// trimsMarkers is the feed's trim_break_markers switch, or the global one.
func (processor *Processor) trimsMarkers(feed models.Feed) bool {
	switch feed.RegionDiffTrimBreakMarkers {
	case "on":
		return true
	case "off":
		return false
	}
	return processor.options.Diff.TrimBreakMarkers
}

// inDifferentCountries makes sure the exits compared with the home exit
// come out in another country than it does right now: two downloads from
// one country carry the same ads, so the diff would find nothing and the
// episode would be published with them. An exit that has fallen back to the
// home country is skipped; when the pair's other exit has, the first
// fallback that hasn't takes its place. With none left, it fails with a
// retryable error, since exits can move back. Unknown countries (direct,
// servers without a location) are trusted.
func (processor *Processor) inDifferentCountries(pair [2]string, fallbacks []string) ([2]string, []string, error) {
	locator := processor.options.Locator
	if locator == nil {
		return pair, fallbacks, nil
	}
	homeCountry, known := locator.ExitCountry(pair[0])
	if !known {
		return pair, fallbacks, nil
	}
	sameCountry := func(exit string) bool {
		country, known := locator.ExitCountry(exit)
		return known && country == homeCountry
	}
	var others []string
	for _, exit := range append([]string{pair[1]}, fallbacks...) {
		if !sameCountry(exit) {
			others = append(others, exit)
		}
	}
	if len(others) == 0 {
		return pair, nil, fmt.Errorf("the exits to compare all come out in %s right now, like %s; waiting for one to move", homeCountry, pair[0])
	}
	return [2]string{pair[0], others[0]}, others[1:], nil
}

// fetchPair downloads through the home exit and its partner at the same
// moment, so the only difference between the two requests is the region.
// When the partner's download fails (its exit is down, or the host doesn't
// answer through it), the fallback exits take its place in turn, while the
// home download carries on; it returns the partner used and the fallbacks
// left for the identical-audio case. When the home download fails there is
// nothing to compare with, and the attempt fails.
func (processor *Processor) fetchPair(ctx context.Context, job episodes.Job, pair [2]string, fallbacks []string) (home, other checkedDownload, partner string, left []string, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var homeErr error
	homeDone := make(chan struct{})
	go func() {
		defer close(homeDone)
		var download episodes.Download
		if download, homeErr = job.Fetch(ctx, pair[0], job.Fresh); homeErr != nil {
			homeErr = fmt.Errorf("download through exit %q: %w", pair[0], homeErr)
			cancel() // no use finishing the partner's
			return
		}
		home = checkedDownload{Download: download}
	}()

	candidates := append([]string{pair[1]}, fallbacks...)
	var partnerErr error
	for i, exit := range candidates {
		download, fetchErr := job.Fetch(ctx, exit, job.Fresh)
		if fetchErr == nil {
			other, partner, left = checkedDownload{Download: download}, exit, candidates[i+1:]
			break
		}
		if ctx.Err() != nil {
			break // the home download failed
		}
		partnerErr = errors.Join(partnerErr, fmt.Errorf("download through exit %q: %w", exit, fetchErr))
		if i+1 < len(candidates) {
			logger.Log.Warn(fmt.Sprintf("Region diff: downloading '%s' through %s failed; comparing with %s instead. Error: %s", job.Episode.Title, exit, candidates[i+1], fetchErr))
		}
	}
	<-homeDone

	switch {
	case homeErr != nil:
		return checkedDownload{}, checkedDownload{}, "", nil, homeErr
	case partner == "":
		return checkedDownload{}, checkedDownload{}, "", nil, partnerErr
	}
	return home, other, partner, left, nil
}

// identicalNote describes an episode every region gave the same audio. A
// file clearly longer than its stated duration hints that it has ads after
// all, the same ones everywhere.
func identicalNote(data []byte, options Options) string {
	note := "no dynamic ads found"
	if options.ExpectedDuration <= 0 {
		return note
	}
	file, err := mp3.Parse(data)
	if err != nil {
		return note
	}
	excess := file.Duration() - options.ExpectedDuration
	if float64(excess) > float64(options.ExpectedDuration)*options.DurationTolerance {
		note += fmt.Sprintf(", but the file is %s longer than stated: any ads may be the same in every region", excess.Round(time.Second))
	}
	return note
}
