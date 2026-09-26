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
	// maxRequestFailures is how many attempts in a row may fail for an
	// episode prepared on request (already published, so without a retry
	// schedule) before the failure policy applies. Without a limit, an
	// episode that fails the same way every time would never be served.
	maxRequestFailures = 3
	// fetchRetryDelay is the pause before retrying a request that got no
	// response at all.
	fetchRetryDelay = 500 * time.Millisecond
	// checkInterval is how often idle workers look for work they weren't
	// woken for, such as retries coming due.
	checkInterval = 30 * time.Second
)

// retryDelays is the wait after each failed attempt. After the last, the
// episode is marked failed: it is then published anyway and served by
// streaming from the source, so one bad download can't hold a feed back.
var retryDelays = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 3 * time.Hour}

// withheldRetryDelays is the wait before each slow retry of an episode its
// failure policy withholds: soon, for a passing problem at the host, then
// daily for about a week. Nothing else would try it again, and a backlog
// episode can be withheld after three failures within a minute. After the
// last, it stays withheld until a retry through the API or a change of
// settings.
//
// WithheldRetrySchedule describes it for the log; keep the two in step.
var withheldRetryDelays = []time.Duration{time.Hour, 6 * time.Hour, 24 * time.Hour, 24 * time.Hour, 24 * time.Hour, 24 * time.Hour, 24 * time.Hour, 24 * time.Hour}

// WithheldRetrySchedule says when a withheld episode is tried again.
const WithheldRetrySchedule = "after 1 hour, after 6 hours, then daily for about a week"

// queueLease is how long a queued episode stays claimed (see
// database.Store.ClaimQueuedEpisode). It outlasts any attempt, so it only
// matters when an attempt never records an outcome: then the episode comes
// due again after this long.
const queueLease = 3 * time.Hour

var (
	// ErrPermanent marks failures a retry won't fix. Processors wrap it too.
	ErrPermanent = errors.New("permanent failure")
	// ErrBusy means an episode can't be prepared on request right now: a
	// worker is just taking it, or the pipeline is shutting down.
	ErrBusy = errors.New("episode is being prepared elsewhere")
)

// Options configures a Pipeline.
type Options struct {
	DefaultDeliveryMode string
	Workers             int
	// RequestWorkers is how many episodes are prepared on request at once;
	// further requests wait their turn. Zero means two. Without a limit, a
	// client downloading a whole backlog at once would start a job for
	// every episode, and comparing by audio takes seconds of CPU and a few
	// hundred MB each.
	RequestWorkers int
	// Processor, when set, prepares the episodes of the feeds it handles
	// instead of a plain download.
	Processor Processor
	// SkipTrackers requests episodes from the audio host directly, skipping
	// the tracking redirects in front of their URLs (feeds.WithoutTrackers).
	// Off, a tracking redirect is only skipped when it fails.
	SkipTrackers bool
	// IdleTimeout abandons a download that sends nothing for this long;
	// zero means two minutes.
	IdleTimeout time.Duration
	// ProcessingWait is how long a client waits for an episode processed on
	// its request before it gets 503 and Retry-After; zero means 20 seconds.
	// Only the Server uses it.
	ProcessingWait time.Duration
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

	mutex sync.Mutex
	// inFlight holds the episodes being prepared, by a worker or on request;
	// each channel is closed when that work is done.
	inFlight map[uuid.UUID]chan struct{}
	// lifetime is Run's context, for work started on request.
	lifetime context.Context
	stopped  bool
	onDemand sync.WaitGroup
	// requestSlots holds one token per episode being prepared on request.
	requestSlots chan struct{}
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
	if options.RequestWorkers < 1 {
		options.RequestWorkers = 2
	}
	return &Pipeline{
		store: store, exits: exits, cache: cache, options: options,
		wake:         make(chan struct{}, 1),
		inFlight:     map[uuid.UUID]chan struct{}{},
		lifetime:     context.Background(),
		requestSlots: make(chan struct{}, options.RequestWorkers),
	}
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

// Run runs the workers until ctx is cancelled, and gives work started on
// request the same lifetime. In-flight downloads are abandoned on shutdown
// and picked up again by Recover on the next start; Run returns once all of
// them have stopped.
func (pipeline *Pipeline) Run(ctx context.Context) {
	pipeline.mutex.Lock()
	pipeline.lifetime = ctx
	pipeline.mutex.Unlock()
	defer func() {
		pipeline.mutex.Lock()
		pipeline.stopped = true
		pipeline.mutex.Unlock()
		pipeline.onDemand.Wait()
	}()

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
	now := pipeline.options.Now().UTC()
	// New episodes first: they hold their feeds back until prepared.
	episode, err := pipeline.store.ClaimNextEpisode(ctx, now, pipeline.options.DefaultDeliveryMode, processedFeeds)
	if errors.Is(err, database.ErrNoWork) {
		episode, err = pipeline.store.ClaimQueuedEpisode(ctx, now, now.Add(queueLease), pipeline.options.DefaultDeliveryMode, processedFeeds)
	}
	if errors.Is(err, database.ErrNoWork) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// Other idle workers can take the next one meanwhile.
	pipeline.Wake()

	done, ok := pipeline.startFlight(episode.ID)
	if !ok {
		// A queued episode a client asked for just before: that request's
		// job prepares it and records the outcome.
		return true, nil
	}
	defer pipeline.endFlight(episode.ID, done)

	feed, err := pipeline.store.GetFeed(ctx, episode.FeedID)
	if err != nil {
		return true, err
	}
	return true, pipeline.prepare(ctx, feed, episode)
}

// Prepare processes an episode now, because a client asked for it before
// the pipeline had: a backlog episode, one whose processed file has expired
// from the cache, or one still waiting for its turn or retry. The work runs
// in the background, for as long as the pipeline runs, and the returned
// channel is closed when it is done; the outcome is recorded on the episode
// as for any other. A request for an episode already being prepared joins
// that work. It returns ErrBusy when a worker is just taking the episode or
// the pipeline is stopping.
func (pipeline *Pipeline) Prepare(feed models.Feed, episode models.Episode) (<-chan struct{}, error) {
	pipeline.mutex.Lock()
	defer pipeline.mutex.Unlock()
	if done, ok := pipeline.inFlight[episode.ID]; ok {
		return done, nil
	}
	if pipeline.stopped || pipeline.lifetime.Err() != nil {
		return nil, ErrBusy
	}
	ctx := pipeline.lifetime

	switch episode.State {
	case models.EpisodeReady:
		// Published without its file: nothing else will prepare it.
	case models.EpisodeDiscovered:
		claimed, err := pipeline.store.ClaimEpisode(ctx, episode.ID, pipeline.options.Now().UTC())
		if errors.Is(err, database.ErrNoWork) {
			return nil, ErrBusy
		}
		if err != nil {
			return nil, err
		}
		episode = claimed
	default:
		// Acquiring by a worker that hasn't registered yet, or failed (the
		// caller serves those as they are).
		return nil, ErrBusy
	}

	done := make(chan struct{})
	pipeline.inFlight[episode.ID] = done
	pipeline.onDemand.Add(1)
	go func() {
		defer pipeline.onDemand.Done()
		defer pipeline.endFlight(episode.ID, done)
		// Wait for a slot. Meanwhile the episode is registered, so further
		// requests for it join this job instead of queueing another.
		select {
		case pipeline.requestSlots <- struct{}{}:
		default:
			logger.Log.Info(fmt.Sprintf("Episode '%s' of '%s' waits for one of the %d episodes being prepared on request.", episode.Title, feed.Title, cap(pipeline.requestSlots)))
			select {
			case pipeline.requestSlots <- struct{}{}:
			case <-ctx.Done():
				return // shutting down; a claimed episode is picked up by Recover
			}
		}
		defer func() { <-pipeline.requestSlots }()
		logger.Log.Info(fmt.Sprintf("Preparing episode '%s' of '%s' for a client that asked for it.", episode.Title, feed.Title))
		if err := pipeline.prepare(ctx, feed, episode); err != nil && ctx.Err() == nil {
			logger.Log.Error("Episode pipeline error. Error: " + err.Error())
		}
	}()
	return done, nil
}

// startFlight registers an episode as being prepared. It reports false when
// it already is.
func (pipeline *Pipeline) startFlight(episodeID uuid.UUID) (chan struct{}, bool) {
	pipeline.mutex.Lock()
	defer pipeline.mutex.Unlock()
	if _, ok := pipeline.inFlight[episodeID]; ok {
		return nil, false
	}
	done := make(chan struct{})
	pipeline.inFlight[episodeID] = done
	return done, true
}

// preparing reports whether an episode is being prepared right now.
func (pipeline *Pipeline) preparing(episodeID uuid.UUID) bool {
	pipeline.mutex.Lock()
	defer pipeline.mutex.Unlock()
	_, ok := pipeline.inFlight[episodeID]
	return ok
}

func (pipeline *Pipeline) endFlight(episodeID uuid.UUID, done chan struct{}) {
	pipeline.mutex.Lock()
	if pipeline.inFlight[episodeID] == done {
		delete(pipeline.inFlight, episodeID)
	}
	pipeline.mutex.Unlock()
	close(done)
}

// prepare downloads or processes an episode into the cache and records the
// outcome. The episode is one of:
//   - claimed (acquiring): a failure is retried on the retry schedule;
//   - ready but without its file, prepared on request or queued to be
//     prepared ahead: a failure is recorded without the retry schedule,
//     since the episode is already published and it is the next request
//     that tries again;
//   - failed, queued for a late retry: a failure leaves it failed, and a
//     withheld episode is tried again on the slow schedule.
//
// The error is for database problems.
func (pipeline *Pipeline) prepare(ctx context.Context, feed models.Feed, episode models.Episode) error {
	claimed := episode.State == models.EpisodeAcquiring
	late := episode.State == models.EpisodeFailed
	recipe, failedRecipe := pipeline.recipe(feed), pipeline.failedRecipe(feed)
	processor := pipeline.options.Processor
	processing := processor != nil && processor.Handles(feed)
	var prepared preparedEpisode
	var err error
	if processing {
		prepared, err = pipeline.process(ctx, feed, episode)
	} else {
		prepared, err = pipeline.download(ctx, feed, episode)
	}
	if ctx.Err() != nil {
		// Shutting down: leave a claimed episode acquiring for Recover.
		return nil
	}
	now := pipeline.options.Now().UTC()
	episode.Attempts++
	if err == nil {
		episode.State = models.EpisodeReady
		episode.CacheFile, episode.CacheSize, episode.CacheSeconds, episode.CachedAt = prepared.cacheFile, prepared.size, prepared.seconds, &now
		episode.LastError, episode.NextAttemptAt, episode.Withheld, episode.ProcessNote = "", nil, false, prepared.note
		episode.PreparedWith, episode.FailedAttempts, episode.LateRetries = recipe, 0, 0
		message := fmt.Sprintf("Cached episode '%s' of '%s' (%.1f MB)", episode.Title, feed.Title, float64(prepared.size)/(1<<20))
		if prepared.note != "" {
			message += "; " + processor.Name() + ": " + prepared.note
		}
		logger.Log.Info(message + ".")
		return pipeline.store.UpdateEpisode(ctx, &episode)
	}

	episode.LastError = err.Error()
	episode.FailedAttempts++
	action := "download"
	if processing {
		action = "process"
	}
	switch {
	case late:
		episode.LateRetries++
		episode.NextAttemptAt, episode.PreparedWith = nil, failedRecipe
		outcome := "it stays as it was"
		switch {
		case episode.Withheld && episode.LateRetries < len(withheldRetryDelays):
			next := now.Add(withheldRetryDelays[episode.LateRetries])
			episode.NextAttemptAt = &next
			outcome = "it stays out of the feed and is tried again " + formatAttemptTime(now, next)
		case episode.Withheld:
			outcome = "it stays out of the feed, and is not tried again on its own; " + retryHint(feed)
		}
		logger.Log.Warn(fmt.Sprintf("Failed again to %s episode '%s' of '%s' (late retry %d); %s. Error: %s", action, episode.Title, feed.Title, episode.LateRetries, outcome, err))
	case errors.Is(err, ErrPermanent) || (claimed && episode.Attempts > len(retryDelays)) || (!claimed && episode.FailedAttempts >= maxRequestFailures):
		episode.State, episode.NextAttemptAt, episode.PreparedWith = models.EpisodeFailed, nil, failedRecipe
		episode.LateRetries = 0
		fallback := "it will be streamed from the source instead"
		if processing {
			episode.Withheld = processor.HideOnFailure(feed)
			fallback = "it is published unprocessed"
			if episode.Withheld {
				next := now.Add(withheldRetryDelays[0])
				episode.NextAttemptAt = &next
				fallback = "it is kept out of the feed, and tried again " + formatAttemptTime(now, next) + ", then less often for about a week; " + retryHint(feed)
			}
		}
		logger.Log.Warn(fmt.Sprintf("Gave up trying to %s episode '%s' of '%s' after %d failed attempts; %s. Error: %s", action, episode.Title, feed.Title, episode.FailedAttempts, fallback, err))
	case claimed:
		next := now.Add(retryDelays[episode.Attempts-1])
		episode.State, episode.NextAttemptAt = models.EpisodeDiscovered, &next
		logger.Log.Warn(fmt.Sprintf("Failed to %s episode '%s' of '%s' (attempt %d), retrying at %s. Error: %s", action, episode.Title, feed.Title, episode.Attempts, next.Local().Format("15:04"), err))
	default:
		when := "on request"
		if episode.NextAttemptAt != nil {
			when = "ahead of a request" // queued; not any more
		}
		episode.NextAttemptAt = nil
		logger.Log.Warn(fmt.Sprintf("Failed to %s episode '%s' of '%s' %s (%d of %d attempts); the next request tries again. Error: %s", action, episode.Title, feed.Title, when, episode.FailedAttempts, maxRequestFailures, err))
	}
	return pipeline.store.UpdateEpisode(ctx, &episode)
}

// formatAttemptTime says when an attempt is due, for the log.
func formatAttemptTime(now, next time.Time) string {
	now, next = now.Local(), next.Local()
	if next.YearDay() == now.YearDay() && next.Year() == now.Year() {
		return "at " + next.Format("15:04")
	}
	return "on " + next.Format("Mon 2 Jan at 15:04")
}

// retryHint says how to try a feed's failed episodes again at once.
func retryHint(feed models.Feed) string {
	return "POST /api/v1/feeds/" + feed.ID.String() + "/retry tries it now"
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
	err := pipeline.fetch(ctx, feed.Exit, episode.SourceURL, episode.FailedAttempts > 0, maxEpisodeBytes, func(contentType string, body io.Reader) (int64, error) {
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
		Fresh:            episode.FailedAttempts > 0,
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
func (pipeline *Pipeline) fetch(ctx context.Context, exit, sourceURL string, fresh bool, limit int64, write func(contentType string, body io.Reader) (int64, error)) error {
	client, err := pipeline.exits.Client(exit)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrPermanent, err)
	}

	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	response, err := requestSource(ctx, client, http.MethodGet, sourceURL, pipeline.options.SkipTrackers, func(request *http.Request) {
		request.Header.Set("Accept", "*/*")
		if fresh {
			// After a failure: in case a CDN edge served a broken copy.
			request.Header.Set("Cache-Control", "no-cache")
			request.Header.Set("Pragma", "no-cache")
		}
	}, checkResponse)
	if err != nil {
		return err
	}
	defer response.Body.Close()

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
