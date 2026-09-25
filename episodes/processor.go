package episodes

import (
	"bytes"
	"context"
	"io"
	"time"

	"aunefyren/solstein/models"
)

// maxProcessBytes caps each download a processor asks for. Processors work
// on whole files in memory; a three-hour episode at 128 kbit/s is under
// 200 MB.
const maxProcessBytes = 512 << 20

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

// fetchForJob fetches into memory for a processor.
func (pipeline *Pipeline) fetchForJob(sourceURL string) func(ctx context.Context, exit string, fresh bool) (Download, error) {
	return func(ctx context.Context, exit string, fresh bool) (Download, error) {
		var buffer bytes.Buffer
		var download Download
		err := pipeline.fetch(ctx, exit, sourceURL, fresh, maxProcessBytes, func(contentType string, body io.Reader) (int64, error) {
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
