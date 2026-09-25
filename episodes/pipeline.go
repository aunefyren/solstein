// Package episodes prepares and serves episode audio. The pipeline downloads
// new episodes of cache-mode feeds into the cache in the background, with
// retries, so clients never wait on a download; for feeds an episode
// processor handles, it runs the processor instead.
package episodes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	"aunefyren/solstein/database"
	"aunefyren/solstein/feeds"
	"aunefyren/solstein/logger"
	"aunefyren/solstein/models"
	"aunefyren/solstein/outbound"

	"github.com/google/uuid"
)

const (
	// maxEpisodeBytes caps a download; the longest podcast episodes are a
	// few hundred megabytes.
	maxEpisodeBytes = 2 << 30
	// defaultIdleTimeout abandons a download that stops sending data.
	defaultIdleTimeout = 2 * time.Minute
	// downloadTimeout bounds a whole download, however slowly it trickles.
	downloadTimeout = time.Hour
	// checkInterval is how often idle workers look for work they weren't
	// woken for, such as retries coming due.
	checkInterval = 30 * time.Second
)

// retryDelays is the wait after each failed attempt. After the last, the
// episode is marked failed: it is then published anyway and served by
// streaming from the source, so one bad download can't hold a feed back.
var retryDelays = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 3 * time.Hour}

// ErrPermanent marks failures a retry won't fix. Processors wrap it too.
var ErrPermanent = errors.New("permanent failure")

// Options configures a Pipeline.
type Options struct {
	DefaultDeliveryMode string
	Workers             int
	// Processor, when set, prepares the episodes of the feeds it handles
	// instead of a plain download.
	Processor Processor
	// IdleTimeout abandons a download that sends nothing for this long;
	// zero means two minutes.
	IdleTimeout time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Pipeline downloads waiting episodes into the cache.
type Pipeline struct {
	store   *database.Store
	exits   *outbound.Manager
	cache   Cache
	options Options
	wake    chan struct{}
}

// NewPipeline builds a Pipeline.
func NewPipeline(store *database.Store, exits *outbound.Manager, cache Cache, options Options) *Pipeline {
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Workers < 1 {
		options.Workers = 1
	}
	if options.IdleTimeout <= 0 {
		options.IdleTimeout = defaultIdleTimeout
	}
	return &Pipeline{store: store, exits: exits, cache: cache, options: options, wake: make(chan struct{}, 1)}
}

// Wake tells an idle worker to look for work now, e.g. after a poll found new
// episodes. It never blocks.
func (pipeline *Pipeline) Wake() {
	select {
	case pipeline.wake <- struct{}{}:
	default:
	}
}

// Recover undoes what a crash or restart left behind: episodes stuck
// mid-download go back to waiting, and their unfinished files are removed.
// Call it once before Run.
func (pipeline *Pipeline) Recover(ctx context.Context) error {
	reset, err := pipeline.store.ResetInterruptedEpisodes(ctx)
	if err != nil {
		return err
	}
	removed, err := pipeline.cache.RemovePartFiles()
	if err != nil {
		return err
	}
	if reset > 0 || removed > 0 {
		logger.Log.Info(fmt.Sprintf("Resumed %d interrupted episode downloads and removed %d unfinished files.", reset, removed))
	}
	return nil
}

// Run runs the workers until ctx is cancelled. In-flight downloads are
// abandoned on shutdown and picked up again by Recover on the next start.
func (pipeline *Pipeline) Run(ctx context.Context) {
	var wait sync.WaitGroup
	for range pipeline.options.Workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			pipeline.work(ctx)
		}()
	}
	pipeline.Wake()
	wait.Wait()
}

func (pipeline *Pipeline) work(ctx context.Context) {
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()
	for {
		for {
			worked, err := pipeline.ProcessNext(ctx)
			if err != nil && ctx.Err() == nil {
				logger.Log.Error("Episode pipeline error. Error: " + err.Error())
			}
			if !worked || ctx.Err() != nil {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-pipeline.wake:
		case <-ticker.C:
		}
	}
}

// ProcessNext claims and prepares one waiting episode: downloads it, or has
// the processor make it, into the cache. It reports whether there was one.
// Failures are recorded on the episode, not returned; the error is for
// database problems.
func (pipeline *Pipeline) ProcessNext(ctx context.Context) (bool, error) {
	processedFeeds, err := pipeline.processedFeeds(ctx)
	if err != nil {
		return false, err
	}
	episode, err := pipeline.store.ClaimNextEpisode(ctx, pipeline.options.Now().UTC(), pipeline.options.DefaultDeliveryMode, processedFeeds)
	if errors.Is(err, database.ErrNoWork) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// Other idle workers can take the next one meanwhile.
	pipeline.Wake()

	feed, err := pipeline.store.GetFeed(ctx, episode.FeedID)
	if err != nil {
		return true, err
	}

	processor := pipeline.options.Processor
	processing := processor != nil && processor.Handles(feed)
	var prepared preparedEpisode
	if processing {
		prepared, err = pipeline.process(ctx, feed, episode)
	} else {
		prepared, err = pipeline.download(ctx, feed, episode)
	}
	if ctx.Err() != nil {
		// Shutting down: leave the episode acquiring for Recover.
		return true, nil
	}
	now := pipeline.options.Now().UTC()
	episode.Attempts++
	if err == nil {
		episode.State = models.EpisodeReady
		episode.CacheFile, episode.CacheSize, episode.CacheSeconds, episode.CachedAt = prepared.cacheFile, prepared.size, prepared.seconds, &now
		episode.LastError, episode.NextAttemptAt, episode.Withheld, episode.ProcessNote = "", nil, false, prepared.note
		message := fmt.Sprintf("Cached episode '%s' of '%s' (%.1f MB)", episode.Title, feed.Title, float64(prepared.size)/(1<<20))
		if prepared.note != "" {
			message += "; " + processor.Name() + ": " + prepared.note
		}
		logger.Log.Info(message + ".")
		return true, pipeline.store.UpdateEpisode(ctx, &episode)
	}

	episode.LastError = err.Error()
	action := "download"
	if processing {
		action = "process"
	}
	if errors.Is(err, ErrPermanent) || episode.Attempts > len(retryDelays) {
		episode.State, episode.NextAttemptAt = models.EpisodeFailed, nil
		fallback := "it will be streamed from the source instead"
		if processing {
			episode.Withheld = processor.HideOnFailure(feed)
			fallback = "it is published unprocessed"
			if episode.Withheld {
				fallback = "it is kept out of the feed"
			}
		}
		logger.Log.Warn(fmt.Sprintf("Gave up trying to %s episode '%s' of '%s' after %d attempts; %s. Error: %s", action, episode.Title, feed.Title, episode.Attempts, fallback, err))
	} else {
		next := now.Add(retryDelays[episode.Attempts-1])
		episode.State, episode.NextAttemptAt = models.EpisodeDiscovered, &next
		logger.Log.Warn(fmt.Sprintf("Failed to %s episode '%s' of '%s' (attempt %d), retrying at %s. Error: %s", action, episode.Title, feed.Title, episode.Attempts, next.Local().Format("15:04"), err))
	}
	return true, pipeline.store.UpdateEpisode(ctx, &episode)
}

// processedFeeds lists the feeds the processor handles, whose episodes are
// prepared whatever their delivery mode.
func (pipeline *Pipeline) processedFeeds(ctx context.Context) ([]uuid.UUID, error) {
	if pipeline.options.Processor == nil {
		return nil, nil
	}
	feeds, err := pipeline.store.ListFeeds(ctx)
	if err != nil {
		return nil, err
	}
	var handled []uuid.UUID
	for _, feed := range feeds {
		if pipeline.options.Processor.Handles(feed) {
			handled = append(handled, feed.ID)
		}
	}
	return handled, nil
}

// preparedEpisode is an episode's file, stored in the cache.
type preparedEpisode struct {
	cacheFile string
	size      int64
	seconds   int // the processed duration; zero when unchanged
	note      string
}

// download fetches an episode's audio through its feed's exit into the cache.
func (pipeline *Pipeline) download(ctx context.Context, feed models.Feed, episode models.Episode) (preparedEpisode, error) {
	var commit func() (string, error)
	var discard func()
	var size int64
	err := pipeline.fetch(ctx, feed.Exit, episode.SourceURL, maxEpisodeBytes, func(contentType string, body io.Reader) (int64, error) {
		file, commitFile, discardFile, err := pipeline.cache.create(feed.ID, episode.ID, feeds.AudioExtension(episode.SourceURL, contentType))
		if err != nil {
			return 0, err
		}
		commit, discard = commitFile, discardFile
		size, err = io.Copy(file, body)
		return size, err
	})
	if err != nil {
		if discard != nil {
			discard()
		}
		return preparedEpisode{}, err
	}
	cacheFile, err := commit()
	if err != nil {
		return preparedEpisode{}, err
	}
	return preparedEpisode{cacheFile: cacheFile, size: size}, nil
}

// process runs the processor on an episode and caches the file it makes.
func (pipeline *Pipeline) process(ctx context.Context, feed models.Feed, episode models.Episode) (preparedEpisode, error) {
	job := Job{
		Feed:             feed,
		Episode:          episode,
		ExpectedDuration: time.Duration(episode.SourceSeconds) * time.Second,
		Fetch:            pipeline.fetchForJob(episode.SourceURL),
	}
	processed, err := pipeline.options.Processor.Process(ctx, job)
	if err != nil {
		return preparedEpisode{}, err
	}
	if len(processed.Audio) == 0 {
		return preparedEpisode{}, errors.New("the processor returned no audio")
	}

	file, commit, discard, err := pipeline.cache.create(feed.ID, episode.ID, feeds.AudioExtension(episode.SourceURL, processed.ContentType))
	if err != nil {
		return preparedEpisode{}, err
	}
	if _, err := file.Write(processed.Audio); err != nil {
		discard()
		return preparedEpisode{}, fmt.Errorf("write processed episode: %w", err)
	}
	cacheFile, err := commit()
	if err != nil {
		return preparedEpisode{}, err
	}
	return preparedEpisode{
		cacheFile: cacheFile,
		size:      int64(len(processed.Audio)),
		seconds:   int(processed.Duration.Round(time.Second) / time.Second),
		note:      processed.Note,
	}, nil
}

// fetch downloads an episode's source through an exit. Once the response
// has passed checkResponse, write receives its body (at most limit+1 bytes)
// and returns how much it took; the download then fails if that was nothing,
// more than limit, or less than the source announced. write's own errors are
// returned as they are.
func (pipeline *Pipeline) fetch(ctx context.Context, exit, sourceURL string, limit int64, write func(contentType string, body io.Reader) (int64, error)) error {
	client, err := pipeline.exits.Client(exit)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrPermanent, err)
	}

	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrPermanent, err)
	}
	request.Header.Set("Accept", "*/*")

	response, err := client.Do(request)
	if err != nil {
		if errors.Is(err, outbound.ErrDestinationBlocked) {
			return fmt.Errorf("%w: %w", ErrPermanent, err)
		}
		return err
	}
	defer response.Body.Close()

	if err := checkResponse(response); err != nil {
		return err
	}

	body := newIdleReader(response.Body, pipeline.options.IdleTimeout, cancel)
	defer body.stop()
	size, err := write(response.Header.Get("Content-Type"), io.LimitReader(body, limit+1))
	switch {
	case err != nil && body.timedOut():
		return fmt.Errorf("no data for %s", pipeline.options.IdleTimeout)
	case err != nil:
		return err
	case size > limit:
		return fmt.Errorf("%w: episode is larger than %d MB", ErrPermanent, limit>>20)
	case size == 0:
		return errors.New("source sent no data")
	case response.ContentLength > 0 && size != response.ContentLength:
		return fmt.Errorf("download ended after %d of %d bytes", size, response.ContentLength)
	}
	return nil
}

// checkResponse refuses error statuses and responses that aren't audio. A
// host that answers with an HTML error page and status 200 would otherwise
// get that page cached and served as the episode.
func checkResponse(response *http.Response) error {
	status := response.StatusCode
	if status < 200 || status > 299 {
		err := fmt.Errorf("source answered %s", response.Status)
		if status >= 400 && status < 500 && status != http.StatusRequestTimeout && status != http.StatusTooManyRequests {
			return fmt.Errorf("%w: %w", ErrPermanent, err)
		}
		return err
	}
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if strings.HasPrefix(mediaType, "text/") || strings.Contains(mediaType, "html") ||
		strings.Contains(mediaType, "xml") || strings.Contains(mediaType, "json") {
		return fmt.Errorf("%w: source sent %s, not audio", ErrPermanent, mediaType)
	}
	return nil
}

// idleReader cancels a download when no data arrives for a while. The
// overall timeout alone would let a stalled download hang for an hour.
type idleReader struct {
	reader  io.Reader
	timeout time.Duration
	timer   *time.Timer
	mutex   sync.Mutex
	expired bool
}

func newIdleReader(reader io.Reader, timeout time.Duration, cancel context.CancelFunc) *idleReader {
	idle := &idleReader{reader: reader, timeout: timeout}
	idle.timer = time.AfterFunc(timeout, func() {
		idle.mutex.Lock()
		idle.expired = true
		idle.mutex.Unlock()
		cancel()
	})
	return idle
}

func (idle *idleReader) Read(buffer []byte) (int, error) {
	count, err := idle.reader.Read(buffer)
	if count > 0 {
		idle.timer.Reset(idle.timeout)
	}
	return count, err
}

func (idle *idleReader) stop() {
	idle.timer.Stop()
}

func (idle *idleReader) timedOut() bool {
	idle.mutex.Lock()
	defer idle.mutex.Unlock()
	return idle.expired
}
