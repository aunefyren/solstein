// Package feeds is the core of the proxy: subscribing to source feeds,
// refreshing them, and rendering the feed Solstein serves to clients.
package feeds

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"aunefyren/solstein/database"
	"aunefyren/solstein/logger"
	"aunefyren/solstein/models"
	"aunefyren/solstein/outbound"
	"aunefyren/solstein/rss"
	"aunefyren/solstein/settings"

	"github.com/google/uuid"
)

// maxFeedBytes caps a source feed. Long-running shows can have feeds of
// several megabytes; anything far beyond that is not a podcast feed.
const maxFeedBytes = 50 << 20

var (
	ErrInvalidSettings = errors.New("invalid feed settings")
	ErrFetchFailed     = errors.New("fetching the source feed failed")
)

// Settings are the per-feed overrides. Zero values mean "use the global
// setting".
type Settings struct {
	Exit                string   `json:"exit"`
	DeliveryMode        string   `json:"delivery_mode"`
	PollIntervalMinutes int      `json:"poll_interval_minutes"`
	RegionDiff          string   `json:"region_diff"`
	RegionDiffExits     []string `json:"region_diff_exits"`
	RegionDiffOnFailure string   `json:"region_diff_on_failure"`
	// RegionDiffTrimBreakMarkers is "on", "off" or empty.
	RegionDiffTrimBreakMarkers string `json:"region_diff_trim_break_markers"`
	// RegionDiffCompareByAudio is "on", "off" or empty.
	RegionDiffCompareByAudio string `json:"region_diff_compare_by_audio"`
}

// SettingsOf returns a feed's per-feed settings.
func SettingsOf(feed models.Feed) Settings {
	return Settings{
		Exit:                       feed.Exit,
		DeliveryMode:               feed.DeliveryMode,
		PollIntervalMinutes:        feed.PollIntervalMinutes,
		RegionDiff:                 feed.RegionDiff,
		RegionDiffExits:            feed.RegionDiffExits,
		RegionDiffOnFailure:        feed.RegionDiffOnFailure,
		RegionDiffTrimBreakMarkers: feed.RegionDiffTrimBreakMarkers,
		RegionDiffCompareByAudio:   feed.RegionDiffCompareByAudio,
	}
}

// regionDiffSwitches are the valid values for a feed's region_diff.
var regionDiffSwitches = []string{"", "on", "off"}

// Options configures a Service.
type Options struct {
	DefaultDeliveryMode string
	AllowedSourceHosts  []string
	// RegionDiffAvailable says whether the region-diff module is running, so
	// a feed can switch it on.
	RegionDiffAvailable bool
	// Processed reports whether an episode processor (such as region diff)
	// handles a feed's episodes. Their episodes are then prepared in the
	// background and published once processed, whatever the delivery mode.
	// Nil means no feed is processed.
	Processed func(models.Feed) bool
	// ProcessBacklog is how many of a processed feed's newest backlog
	// episodes are queued for processing as soon as it is subscribed to, so
	// the likeliest plays are ready at once. The rest are processed when
	// first requested.
	ProcessBacklog int
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Service subscribes to, refreshes and renders feeds.
type Service struct {
	store   *database.Store
	exits   *outbound.Manager
	options Options
}

// New builds a Service.
func New(store *database.Store, exits *outbound.Manager, options Options) *Service {
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Service{store: store, exits: exits, options: options}
}

// ValidateSettings checks per-feed settings: a known exit and delivery mode,
// a non-negative poll interval, and region-diff settings that can work: on
// only when the module runs, a pair of two different available exits, a
// known failure policy.
func (service *Service) ValidateSettings(feedSettings Settings) error {
	if feedSettings.Exit != "" {
		if _, err := service.exits.Client(feedSettings.Exit); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidSettings, err)
		}
	}
	if feedSettings.DeliveryMode != "" && !slices.Contains(settings.DeliveryModes, feedSettings.DeliveryMode) {
		return fmt.Errorf("%w: delivery mode %q is not one of %v", ErrInvalidSettings, feedSettings.DeliveryMode, settings.DeliveryModes)
	}
	if feedSettings.PollIntervalMinutes < 0 {
		return fmt.Errorf("%w: poll interval must not be negative", ErrInvalidSettings)
	}
	if !slices.Contains(regionDiffSwitches, feedSettings.RegionDiff) {
		return fmt.Errorf("%w: region_diff must be \"on\", \"off\" or empty (follow the global setting)", ErrInvalidSettings)
	}
	if !slices.Contains(regionDiffSwitches, feedSettings.RegionDiffTrimBreakMarkers) {
		return fmt.Errorf("%w: region_diff_trim_break_markers must be \"on\", \"off\" or empty (follow the global setting)", ErrInvalidSettings)
	}
	if !slices.Contains(regionDiffSwitches, feedSettings.RegionDiffCompareByAudio) {
		return fmt.Errorf("%w: region_diff_compare_by_audio must be \"on\", \"off\" or empty (follow the global setting)", ErrInvalidSettings)
	}
	if feedSettings.RegionDiff == "on" && !service.options.RegionDiffAvailable {
		return fmt.Errorf("%w: region diff isn't running; set up region_diff in config.json first", ErrInvalidSettings)
	}
	if feedSettings.RegionDiffOnFailure != "" && !slices.Contains(settings.RegionDiffFailurePolicies, feedSettings.RegionDiffOnFailure) {
		return fmt.Errorf("%w: region_diff_on_failure %q is not one of %v", ErrInvalidSettings, feedSettings.RegionDiffOnFailure, settings.RegionDiffFailurePolicies)
	}
	if exits := feedSettings.RegionDiffExits; len(exits) > 0 {
		if len(exits) != 2 || exits[0] == exits[1] {
			return fmt.Errorf("%w: region_diff_exits must be two different exits, the home region first", ErrInvalidSettings)
		}
		for _, exit := range exits {
			if _, err := service.exits.Client(exit); err != nil {
				return fmt.Errorf("%w: region_diff_exits: %w", ErrInvalidSettings, err)
			}
		}
	}
	return nil
}

// DeliveryMode is the mode a feed actually uses.
func (service *Service) DeliveryMode(feed models.Feed) string {
	if feed.DeliveryMode != "" {
		return feed.DeliveryMode
	}
	return service.options.DefaultDeliveryMode
}

// Processed reports whether an episode processor handles the feed.
func (service *Service) Processed(feed models.Feed) bool {
	return service.options.Processed != nil && service.options.Processed(feed)
}

// Subscribe returns the feed for a source URL, subscribing to it first if
// needed. A new subscription fetches the source right away and is only
// stored if it is a valid RSS feed, so a mistyped URL leaves nothing behind.
// Every episode already in the feed is stored as backlog: published at once
// and fetched on demand, since clients don't auto-download old episodes.
// created reports whether the feed is new.
func (service *Service) Subscribe(ctx context.Context, rawSourceURL string, feedSettings Settings) (feed models.Feed, created bool, err error) {
	sourceURL, err := NormaliseSourceURL(rawSourceURL)
	if err != nil {
		return models.Feed{}, false, err
	}
	if !hostAllowed(sourceURL, service.options.AllowedSourceHosts) {
		return models.Feed{}, false, ErrSourceNotAllowed
	}

	feed, err = service.store.GetFeedBySourceURL(ctx, sourceURL)
	if err == nil {
		return feed, false, nil
	}
	if !errors.Is(err, database.ErrFeedNotFound) {
		return models.Feed{}, false, err
	}

	if err := service.ValidateSettings(feedSettings); err != nil {
		return models.Feed{}, false, err
	}
	feed = models.Feed{
		SourceURL:                  sourceURL,
		Exit:                       feedSettings.Exit,
		DeliveryMode:               feedSettings.DeliveryMode,
		PollIntervalMinutes:        feedSettings.PollIntervalMinutes,
		RegionDiff:                 feedSettings.RegionDiff,
		RegionDiffExits:            feedSettings.RegionDiffExits,
		RegionDiffOnFailure:        feedSettings.RegionDiffOnFailure,
		RegionDiffTrimBreakMarkers: feedSettings.RegionDiffTrimBreakMarkers,
		RegionDiffCompareByAudio:   feedSettings.RegionDiffCompareByAudio,
	}

	result, err := service.fetch(ctx, feed)
	if err != nil {
		return models.Feed{}, false, err
	}
	parsed, err := rss.Parse(result.data)
	if err != nil {
		return models.Feed{}, false, fmt.Errorf("%w: %w", ErrFetchFailed, err)
	}

	now := service.options.Now().UTC()
	feed.Title = parsed.Title
	feed.ETag, feed.LastModified = result.etag, result.lastModified
	feed.LastPolledAt, feed.LastSuccessAt = &now, &now

	episodes := episodesFromItems(parsed.Items, models.EpisodeReady, true)
	if service.Processed(feed) {
		queueNewest(episodes, service.options.ProcessBacklog)
	}
	err = service.store.CreateSubscription(ctx, &feed, result.data, now, episodes)
	if errors.Is(err, database.ErrFeedExists) {
		// Another request subscribed to the same feed meanwhile.
		feed, err = service.store.GetFeedBySourceURL(ctx, sourceURL)
		return feed, false, err
	}
	if err != nil {
		return models.Feed{}, false, err
	}
	return feed, true, nil
}

// Refresh polls a feed's source. New episodes are stored for the episode
// pipeline (in cache mode, or when a processor handles the feed) or as ready
// (in stream and original mode, which need no preparation). A failed poll keeps the last good document, so
// clients keep being served, and is recorded on the feed.
func (service *Service) Refresh(ctx context.Context, feed *models.Feed) (added []models.Episode, err error) {
	now := service.options.Now().UTC()
	feed.LastPolledAt = &now
	defer func() {
		if err != nil {
			feed.LastError = err.Error()
		} else {
			feed.LastError = ""
			feed.LastSuccessAt = &now
		}
		if updateErr := service.store.UpdateFeed(ctx, feed); updateErr != nil && err == nil {
			err = updateErr
		}
	}()

	result, err := service.fetch(ctx, *feed)
	if err != nil {
		return nil, err
	}
	if result.notModified {
		return nil, nil
	}
	parsed, err := rss.Parse(result.data)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrFetchFailed, err)
	}
	if err := service.store.SaveFeedDocument(ctx, feed.ID, result.data, now); err != nil {
		return nil, err
	}
	feed.Title = parsed.Title
	feed.ETag, feed.LastModified = result.etag, result.lastModified

	state := models.EpisodeReady
	if service.DeliveryMode(*feed) == "cache" || service.Processed(*feed) {
		state = models.EpisodeDiscovered
	}
	return service.store.AddNewEpisodes(ctx, feed.ID, episodesFromItems(parsed.Items, state, false))
}

// Render builds the feed served to clients from the stored source document
// and the episodes' state.
func (service *Service) Render(ctx context.Context, feed models.Feed, urls URLs) ([]byte, error) {
	document, err := service.store.GetFeedDocument(ctx, feed.ID)
	if err != nil {
		return nil, err
	}
	episodes, err := service.store.ListEpisodes(ctx, feed.ID)
	if err != nil {
		return nil, err
	}

	mode := service.DeliveryMode(feed)
	published := publishedEpisodes(episodes, mode == "cache" || service.Processed(feed))

	// An episode's served pubDate is never earlier than the first time
	// Solstein served it. ABS only auto-downloads episodes dated after a
	// reference point: the newest episode it has, or — for a podcast with
	// none downloaded yet — the time of its previous check. An episode that
	// reaches the served feed late (held back until cached, or simply found by
	// Solstein's poll after the client last checked) would otherwise be dated
	// before that check and skipped for good. Backlog episodes keep their
	// dates; clients don't auto-download those anyway.
	now := service.options.Now().UTC().Truncate(time.Second)
	var newlyReleased []uuid.UUID
	servedDate := make(map[uuid.UUID]time.Time)
	for _, episode := range episodes {
		if !published[episode.ID] || episode.Backlog {
			continue
		}
		releasedAt := now
		if episode.ReleasedAt != nil {
			releasedAt = episode.ReleasedAt.UTC()
		} else {
			newlyReleased = append(newlyReleased, episode.ID)
		}
		if episode.PublishedAt == nil || releasedAt.After(*episode.PublishedAt) {
			servedDate[episode.ID] = releasedAt
		}
	}
	byGUID := make(map[string]models.Episode, len(episodes))
	for _, episode := range episodes {
		byGUID[episode.GUID] = episode
	}

	rewrite := rss.Rewrite{
		FeedURL: urls.Feed(feed.ID),
		Item: func(item rss.Item) rss.ItemChange {
			if item.Enclosure == nil {
				return rss.ItemChange{} // no audio: nothing to proxy
			}
			episode, ok := byGUID[item.Key]
			if !ok || !published[episode.ID] {
				return rss.ItemChange{Omit: true}
			}
			var change rss.ItemChange
			if date, ok := servedDate[episode.ID]; ok {
				change.PublishedAt = &date
			}
			if mode == "original" {
				return change
			}
			change.EnclosureURL = urls.Episode(feed.ID, episode.ID, AudioExtension(item.Enclosure.URL, item.Enclosure.Type))
			if episode.CacheSize > 0 {
				change.Length = episode.CacheSize
			}
			if episode.CacheSeconds > 0 {
				change.Duration = rss.FormatDuration(time.Duration(episode.CacheSeconds) * time.Second)
			}
			return change
		},
	}
	output, err := rewrite.Apply(document.Data)
	if err != nil {
		return nil, err
	}
	// Recorded after a successful render, so the dates clients saw are the
	// ones kept.
	if err := service.store.MarkReleased(ctx, newlyReleased, now); err != nil {
		return nil, err
	}
	return output, nil
}

// Feed returns a feed by ID.
func (service *Service) Feed(ctx context.Context, feedID uuid.UUID) (models.Feed, error) {
	return service.store.GetFeed(ctx, feedID)
}

// List returns every feed.
func (service *Service) List(ctx context.Context) ([]models.Feed, error) {
	return service.store.ListFeeds(ctx)
}

// Update saves a feed's settings after validating them.
func (service *Service) Update(ctx context.Context, feed *models.Feed) error {
	err := service.ValidateSettings(SettingsOf(*feed))
	if err != nil {
		return err
	}
	return service.store.UpdateFeed(ctx, feed)
}

// Delete removes a feed and everything stored for it.
func (service *Service) Delete(ctx context.Context, feedID uuid.UUID) error {
	return service.store.DeleteFeed(ctx, feedID)
}

func episodesFromItems(items []rss.Item, state models.EpisodeState, backlog bool) []models.Episode {
	var episodes []models.Episode
	for _, item := range items {
		if item.Enclosure == nil || item.Key == "" {
			continue
		}
		episode := models.Episode{
			GUID:        item.Key,
			SourceURL:   item.Enclosure.URL,
			Title:       item.Title,
			PublishedAt: item.PublishedAt,
			Backlog:     backlog,
			State:       state,
		}
		if duration, ok := rss.ParseDuration(item.Duration); ok {
			episode.SourceSeconds = int(duration.Round(time.Second) / time.Second)
		}
		episodes = append(episodes, episode)
	}
	return episodes
}

// queueNewest marks the count newest backlog episodes as waiting for the
// pipeline. Episodes without a date count as oldest.
func queueNewest(episodes []models.Episode, count int) {
	if count <= 0 {
		return
	}
	order := make([]int, len(episodes))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		first, second := episodes[a].PublishedAt, episodes[b].PublishedAt
		switch {
		case first == nil && second == nil:
			return 0
		case first == nil:
			return 1
		case second == nil:
			return -1
		}
		return second.Compare(*first)
	})
	for _, index := range order[:min(count, len(order))] {
		episodes[index].State = models.EpisodeDiscovered
	}
}

type fetchResult struct {
	data         []byte
	notModified  bool
	etag         string
	lastModified string
}

// fetch downloads a feed's source through its exit, conditionally when the
// feed has an ETag or Last-Modified from an earlier poll.
// requestFeed requests a feed, conditionally, and returns a 2xx or 304
// response. A feed URL can be a tracking prefix too (Podtrac's pdrl.fm in
// front of feeds.megaphone.fm): when the request fails at a tracker whose
// target is embedded in its URL — an error status, an HTML page, no
// response — it asks that URL directly, as episode downloads do.
func requestFeed(ctx context.Context, client *http.Client, feed models.Feed) (*http.Response, error) {
	target := feed.SourceURL
	for skipped := 0; ; skipped++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrFetchFailed, err)
		}
		request.Header.Set("Accept", "application/rss+xml, application/xml;q=0.9, text/xml;q=0.9, */*;q=0.8")
		if feed.ETag != "" {
			request.Header.Set("If-None-Match", feed.ETag)
		}
		if feed.LastModified != "" {
			request.Header.Set("If-Modified-Since", feed.LastModified)
		}

		failedAt := target
		response, err := client.Do(request)
		switch {
		case err != nil:
			err = fmt.Errorf("%w: %w", ErrFetchFailed, err)
			var urlErr *url.Error
			if errors.As(err, &urlErr) && urlErr.URL != "" {
				failedAt = urlErr.URL
			}
		case response.StatusCode == http.StatusNotModified || (response.StatusCode >= 200 && response.StatusCode <= 299 && !isHTML(response)):
			return response, nil
		case response.StatusCode >= 200 && response.StatusCode <= 299:
			// An HTML page is a tracker's parking page when there is a URL to
			// fall back to; otherwise it is the feed host's answer, and the
			// parser says it isn't RSS.
			if _, ok := EmbeddedURL(response.Request.URL.String()); !ok {
				return response, nil
			}
			failedAt = response.Request.URL.String()
			err = fmt.Errorf("%w: source sent an HTML page, not a feed", ErrFetchFailed)
			response.Body.Close()
		default:
			failedAt = response.Request.URL.String()
			err = fmt.Errorf("%w: source answered %s", ErrFetchFailed, response.Status)
			response.Body.Close()
		}
		if errors.Is(err, outbound.ErrDestinationBlocked) || ctx.Err() != nil {
			return nil, err
		}
		next, ok := EmbeddedURL(failedAt)
		if !ok || skipped >= maxTrackers {
			return nil, err
		}
		logger.Log.Info(fmt.Sprintf("Tracking redirect in front of feed '%s' failed at %s (%s); asking %s directly.", feed.Title, hostOf(failedAt), shortError(err), hostOf(next)))
		target = next
	}
}

func isHTML(response *http.Response) bool {
	return strings.Contains(response.Header.Get("Content-Type"), "html")
}

func hostOf(raw string) string {
	if parsed, err := url.Parse(raw); err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return "?"
}

// shortError drops the URL Go's HTTP errors quote: its query may carry an
// access token.
func shortError(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err.Error()
	}
	return err.Error()
}

func (service *Service) fetch(ctx context.Context, feed models.Feed) (fetchResult, error) {
	client, err := service.exits.Client(feed.Exit)
	if err != nil {
		return fetchResult{}, fmt.Errorf("%w: %w", ErrFetchFailed, err)
	}
	response, err := requestFeed(ctx, client, feed)
	if err != nil {
		return fetchResult{}, err
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusNotModified {
		return fetchResult{notModified: true, etag: feed.ETag, lastModified: feed.LastModified}, nil
	}

	data, err := io.ReadAll(io.LimitReader(response.Body, maxFeedBytes+1))
	if err != nil {
		return fetchResult{}, fmt.Errorf("%w: reading response: %w", ErrFetchFailed, err)
	}
	if len(data) > maxFeedBytes {
		return fetchResult{}, fmt.Errorf("%w: feed is larger than %d MB", ErrFetchFailed, maxFeedBytes>>20)
	}
	return fetchResult{
		data:         data,
		etag:         response.Header.Get("ETag"),
		lastModified: response.Header.Get("Last-Modified"),
	}, nil
}
