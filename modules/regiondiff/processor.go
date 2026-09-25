package regiondiff

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"aunefyren/solstein/episodes"
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
// would cut differently, so episodes cut before are cut again.
const algorithmVersion = 1

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
	home, other, err := fetchPair(ctx, job, pair)
	if err != nil {
		return episodes.Processed{}, err
	}

	options := processor.options.Diff
	options.ExpectedDuration = job.ExpectedDuration
	options.TrimBreakMarkers = processor.trimsMarkers(job.Feed)
	compared := pair[1]
	result, err := Diff(home.Data, other.Data, options)
	for _, exit := range fallbacks {
		if !errors.Is(err, ErrIdentical) {
			break
		}
		fallback, fetchErr := job.Fetch(ctx, exit)
		if fetchErr != nil {
			return episodes.Processed{}, fmt.Errorf("download through fallback exit %q: %w", exit, fetchErr)
		}
		compared = exit
		result, err = Diff(home.Data, fallback.Data, options)
	}

	switch {
	case errors.Is(err, ErrIdentical):
		return episodes.Processed{Audio: home.Data, ContentType: home.ContentType, Note: identicalNote(home.Data, options)}, nil
	case errors.Is(err, ErrUnsupported):
		return episodes.Processed{}, fmt.Errorf("%w: %w", episodes.ErrPermanent, err)
	case err != nil:
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
	return episodes.Processed{
		Audio:       result.Output,
		ContentType: home.ContentType,
		Duration:    result.Duration,
		Note:        note,
	}, nil
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

// fetchPair downloads through both exits at the same moment, so the only
// difference between the two requests is the region.
func fetchPair(ctx context.Context, job episodes.Job, exits [2]string) (home, other episodes.Download, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var downloads [2]episodes.Download
	var errs [2]error
	var wait sync.WaitGroup
	for i, exit := range exits {
		wait.Go(func() {
			downloads[i], errs[i] = job.Fetch(ctx, exit)
			if errs[i] != nil {
				errs[i] = fmt.Errorf("download through exit %q: %w", exit, errs[i])
				cancel() // no use finishing the other
			}
		})
	}
	wait.Wait()
	// The first real failure, not the other download's cancellation.
	for _, err := range errs {
		if err != nil && !errors.Is(err, context.Canceled) {
			return episodes.Download{}, episodes.Download{}, err
		}
	}
	if err := errors.Join(errs[0], errs[1]); err != nil {
		return episodes.Download{}, episodes.Download{}, err
	}
	return downloads[0], downloads[1], nil
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
