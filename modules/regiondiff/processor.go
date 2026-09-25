package regiondiff

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"aunefyren/solstein/episodes"
	"aunefyren/solstein/models"
	"aunefyren/solstein/mp3"
)

// ProcessorOptions configures the region-diff processor.
type ProcessorOptions struct {
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

// Handles reports whether region diff applies to a feed: every feed, for
// now. Per-feed switches come with the settings (see docs/design.md, Region
// diff build order).
func (processor *Processor) Handles(models.Feed) bool { return true }

func (processor *Processor) HideOnFailure(models.Feed) bool { return processor.options.HideOnFailure }

// Process downloads the episode through both exits at once and removes the
// audio they don't share. When both carry the same audio, the fallback
// exits are tried in turn; if every one agrees, the episode is kept as it
// is, since it most likely has no dynamic ads. Files that can't be diffed
// frame by frame fail for good; an implausible result is retried, as the
// next downloads may carry other ads.
func (processor *Processor) Process(ctx context.Context, job episodes.Job) (episodes.Processed, error) {
	home, other, err := fetchPair(ctx, job, processor.options.Exits)
	if err != nil {
		return episodes.Processed{}, err
	}

	options := processor.options.Diff
	options.ExpectedDuration = job.ExpectedDuration
	compared := processor.options.Exits[1]
	result, err := Diff(home.Data, other.Data, options)
	for _, exit := range processor.options.FallbackExits {
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
	breaks := fmt.Sprintf("%d breaks", len(result.Removed))
	if len(result.Removed) == 1 {
		breaks = "1 break"
	}
	return episodes.Processed{
		Audio:       result.Output,
		ContentType: home.ContentType,
		Duration:    result.Duration,
		Note: fmt.Sprintf("removed %s of ads in %s, comparing %s with %s",
			removed.Round(time.Second), breaks, processor.options.Exits[0], compared),
	}, nil
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
