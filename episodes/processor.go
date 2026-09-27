package episodes

import (
	"bytes"
	"context"
	"io"
	"time"

	"aunefyren/solstein/models"
)

// defaultMaxProcessBytes is Options.MaxProcessBytes when unset: processors
// work on whole files in memory, and a three-hour episode at 128 kbit/s is
// under 200 MB — too small for some real shows (docs/wip.md), which is why
// it's a setting (settings.RegionDiff.MaxEpisodeMB) rather than fixed.
const defaultMaxProcessBytes = 512 << 20

// Processor turns an episode into the file served in its place — region diff
// removing dynamic ads, for one. Processors are modules: main.go hands one to
// the pipeline, which runs it in the background for the feeds it handles and
// caches what it returns. Episodes of those feeds are published once
// processed (see feeds.Options.Processed).
//
// Process's errors are retried with the pipeline's back-off unless they wrap
// ErrPermanent. Once it gives up, the episode is failed: published
// unprocessed, or withheld from the feed when HideOnFailure says so.
type Processor interface {
	Name() string
	// Handles reports whether the processor applies to a feed's episodes.
	Handles(feed models.Feed) bool
	HideOnFailure(feed models.Feed) bool
	// Recipe describes the settings that shape the processor's output for
	// a feed. When it changes, episodes processed before are processed
	// again. Include a version to bump when the processing itself changes.
	Recipe(feed models.Feed) string
	Process(ctx context.Context, job Job) (Processed, error)
}

// BatchProcessor is implemented by a Processor that can take several of a
// feed's queued episodes together, when that amortises a cost that falls
// per switch between exits rather than per episode (region diff, on a key
// short of a tunnel per exit). The pipeline uses it instead of Process only
// for queued (background) work, never for an episode a client is waiting
// on, and only when more than one of a feed's episodes are queued at
// once — otherwise episodes are prepared one at a time as always.
type BatchProcessor interface {
	Processor
	// BatchSize reports how many of a feed's queued episodes to take
	// together right now, and whether it's worth it at all; 0 or false
	// means one at a time as usual.
	BatchSize(feed models.Feed) (size int, ok bool)
	// ProcessBatch is Process for several episodes of the same feed at
	// once. It always returns one outcome per job, in the same order.
	ProcessBatch(ctx context.Context, jobs []Job) []BatchOutcome
}

// BatchOutcome is one job's result from ProcessBatch: Processed is valid
// only when Err is nil, exactly as Process's own return values are.
type BatchOutcome struct {
	Processed Processed
	Err       error
}

// Job is one episode for a processor.
type Job struct {
	Feed    models.Feed
	Episode models.Episode
	// ExpectedDuration is the source's stated duration, zero if unknown.
	ExpectedDuration time.Duration
	// Fresh is set when the last attempt failed: fetches should then ask
	// for a fresh copy, in case a cache on the way served a bad one.
	Fresh bool
	// Fetch downloads the episode's source through an exit, with the checks
	// the pipeline's own downloads get: audio only, complete, and at most
	// 512 MB. fresh asks caches on the way (the host's CDN) not to answer
	// from a stored copy.
	Fetch func(ctx context.Context, exit string, fresh bool) (Download, error)
	// CloseIdle drops an exit's pooled keep-alive connections. A finished
	// download's connection stays open for reuse until the client's idle
	// timeout, which outlasts a tunnel pool short on tunnels: switching to
	// a different exit right after would find the previous one still
	// "in use" by that idle connection and wait out the tunnel limit for
	// nothing (docs/exits.md). Call it on an exit a processor is done with,
	// right before fetching through a different one.
	CloseIdle func(exit string)
}

// Download is a fetched copy of an episode's source.
type Download struct {
	Data        []byte
	ContentType string
}

// Processed is what a processor made of an episode.
type Processed struct {
	Audio       []byte
	ContentType string
	// Duration is the audio's playing time when the processor changed it,
	// served as itunes:duration; zero keeps the source's.
	Duration time.Duration
	// Note says what was done, for the episode's status and the log.
	Note string
}

// closeIdle drops an exit's pooled idle connections; see Job.CloseIdle. An
// unknown exit has no client to close anything on, so there is nothing to do.
func (pipeline *Pipeline) closeIdle(exit string) {
	if client, err := pipeline.exits.Client(exit); err == nil {
		client.CloseIdleConnections()
	}
}

// fetchForJob fetches into memory for a processor.
func (pipeline *Pipeline) fetchForJob(sourceURL string) func(ctx context.Context, exit string, fresh bool) (Download, error) {
	return func(ctx context.Context, exit string, fresh bool) (Download, error) {
		var buffer bytes.Buffer
		var download Download
		err := pipeline.fetch(ctx, exit, sourceURL, fresh, pipeline.options.MaxProcessBytes, func(contentType string, body io.Reader) (int64, error) {
			download.ContentType = contentType
			return buffer.ReadFrom(body)
		})
		if err != nil {
			return Download{}, err
		}
		download.Data = buffer.Bytes()
		return download, nil
	}
}
