// Package episodes prepares and serves episode audio. The pipeline downloads
// new episodes of cache-mode feeds into the cache in the background, with
// retries, so clients never wait on a download.
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

// errPermanent marks failures a retry won't fix.
var errPermanent = errors.New("permanent failure")

// Options configures a Pipeline.
type Options struct {
	DefaultDeliveryMode string
	Workers             int
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

// ProcessNext claims and downloads one waiting episode. It reports whether
// there was one. Download failures are recorded on the episode, not returned;
// the error is for database problems.
func (pipeline *Pipeline) ProcessNext(ctx context.Context) (bool, error) {
	episode, err := pipeline.store.ClaimNextEpisode(ctx, pipeline.options.Now().UTC(), pipeline.options.DefaultDeliveryMode)
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

	cacheFile, size, downloadErr := pipeline.download(ctx, feed, episode)
	if ctx.Err() != nil {
		// Shutting down: leave the episode acquiring for Recover.
		return true, nil
	}
	now := pipeline.options.Now().UTC()
	episode.Attempts++
	if downloadErr == nil {
		episode.State = models.EpisodeReady
		episode.CacheFile, episode.CacheSize, episode.CachedAt = cacheFile, size, &now
		episode.LastError, episode.NextAttemptAt = "", nil
		logger.Log.Info(fmt.Sprintf("Cached episode '%s' of '%s' (%.1f MB).", episode.Title, feed.Title, float64(size)/(1<<20)))
	} else {
		episode.LastError = downloadErr.Error()
		if errors.Is(downloadErr, errPermanent) || episode.Attempts > len(retryDelays) {
			episode.State, episode.NextAttemptAt = models.EpisodeFailed, nil
			logger.Log.Warn(fmt.Sprintf("Gave up downloading episode '%s' of '%s' after %d attempts; it will be streamed from the source instead. Error: %s", episode.Title, feed.Title, episode.Attempts, downloadErr))
		} else {
			next := now.Add(retryDelays[episode.Attempts-1])
			episode.State, episode.NextAttemptAt = models.EpisodeDiscovered, &next
			logger.Log.Warn(fmt.Sprintf("Failed to download episode '%s' of '%s' (attempt %d), retrying at %s. Error: %s", episode.Title, feed.Title, episode.Attempts, next.Local().Format("15:04"), downloadErr))
		}
	}
	return true, pipeline.store.UpdateEpisode(ctx, &episode)
}

// download fetches an episode's audio through its feed's exit into the cache.
func (pipeline *Pipeline) download(ctx context.Context, feed models.Feed, episode models.Episode) (string, int64, error) {
	client, err := pipeline.exits.Client(feed.Exit)
	if err != nil {
		return "", 0, fmt.Errorf("%w: %w", errPermanent, err)
	}

	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, episode.SourceURL, nil)
	if err != nil {
		return "", 0, fmt.Errorf("%w: %w", errPermanent, err)
	}
	request.Header.Set("Accept", "*/*")

	response, err := client.Do(request)
	if err != nil {
		if errors.Is(err, outbound.ErrDestinationBlocked) {
			return "", 0, fmt.Errorf("%w: %w", errPermanent, err)
		}
		return "", 0, err
	}
	defer response.Body.Close()

	if err := checkResponse(response); err != nil {
		return "", 0, err
	}

	extension := feeds.AudioExtension(episode.SourceURL, response.Header.Get("Content-Type"))
	file, commit, discard, err := pipeline.cache.create(feed.ID, episode.ID, extension)
	if err != nil {
		return "", 0, err
	}

	body := newIdleReader(response.Body, pipeline.options.IdleTimeout, cancel)
	defer body.stop()
	size, err := io.Copy(file, io.LimitReader(body, maxEpisodeBytes+1))
	switch {
	case err != nil && body.timedOut():
		err = fmt.Errorf("no data for %s", pipeline.options.IdleTimeout)
	case err == nil && size > maxEpisodeBytes:
		err = fmt.Errorf("%w: episode is larger than %d GB", errPermanent, maxEpisodeBytes>>30)
	case err == nil && size == 0:
		err = errors.New("source sent no data")
	case err == nil && response.ContentLength > 0 && size != response.ContentLength:
		err = fmt.Errorf("download ended after %d of %d bytes", size, response.ContentLength)
	}
	if err != nil {
		discard()
		return "", 0, err
	}

	cacheFile, err := commit()
	if err != nil {
		return "", 0, err
	}
	return cacheFile, size, nil
}

// checkResponse refuses error statuses and responses that aren't audio. A
// host that answers with an HTML error page and status 200 would otherwise
// get that page cached and served as the episode.
func checkResponse(response *http.Response) error {
	status := response.StatusCode
	if status < 200 || status > 299 {
		err := fmt.Errorf("source answered %s", response.Status)
		if status >= 400 && status < 500 && status != http.StatusRequestTimeout && status != http.StatusTooManyRequests {
			return fmt.Errorf("%w: %w", errPermanent, err)
		}
		return err
	}
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if strings.HasPrefix(mediaType, "text/") || strings.Contains(mediaType, "html") ||
		strings.Contains(mediaType, "xml") || strings.Contains(mediaType, "json") {
		return fmt.Errorf("%w: source sent %s, not audio", errPermanent, mediaType)
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
