package models

import (
	"time"

	"github.com/google/uuid"
)

// EpisodeState is where an episode is in the pipeline; see docs/episodes.md.
type EpisodeState string

const (
	EpisodeDiscovered EpisodeState = "discovered"
	EpisodeAcquiring  EpisodeState = "acquiring"
	EpisodeProcessing EpisodeState = "processing"
	EpisodeReady      EpisodeState = "ready"
	EpisodeFailed     EpisodeState = "failed"
)

// Episode is one item of a feed.
type Episode struct {
	Base
	FeedID uuid.UUID `json:"feed_id" gorm:"type:varchar(36);not null;uniqueIndex:idx_episode_feed_guid"`
	// GUID is the source item's guid, or its enclosure URL when the item has
	// none. It is never rewritten in the served feed, because clients (ABS)
	// match episodes they already have by it.
	GUID        string     `json:"guid" gorm:"not null;uniqueIndex:idx_episode_feed_guid"`
	SourceURL   string     `json:"source_url" gorm:"not null"`
	Title       string     `json:"title"`
	PublishedAt *time.Time `json:"published_at" gorm:"index"`
	// SourceSeconds is the source's stated duration (itunes:duration), zero
	// if it states none.
	SourceSeconds int `json:"source_seconds"`
	// Backlog marks episodes that already existed when the feed was added.
	// They are fetched on demand instead of at poll time.
	Backlog bool `json:"backlog"`
	// ReleasedAt is when the episode first appeared in a feed Solstein
	// served. The served pubDate is never earlier than this; see
	// feeds.Service.Render for why.
	ReleasedAt *time.Time `json:"released_at"`

	State    EpisodeState `json:"state" gorm:"not null;index"`
	Attempts int          `json:"attempts"`
	// FailedAttempts counts failures since the last success. It limits how
	// often an episode already published is retried on request, and makes
	// retries ask the source for a fresh copy.
	FailedAttempts int `json:"failed_attempts"`
	// NextAttemptAt is when a waiting episode is tried again. On a failed
	// or published episode, it means the episode is queued for a
	// background attempt: a withheld episode's slow retries, a manual
	// retry, or preparing ahead (see episodes.Pipeline.Queue).
	NextAttemptAt *time.Time `json:"next_attempt_at"`
	// LateRetries counts the background attempts made since the episode
	// failed; it limits a withheld episode's slow retries.
	LateRetries int    `json:"late_retries"`
	LastError   string `json:"last_error"`
	// Withheld marks a failed episode the processor's failure policy keeps
	// out of the feed, rather than publishing it unprocessed.
	Withheld bool `json:"withheld"`
	// Hidden marks an episode one of the feed's rules leaves out of the
	// feed. It is never prepared, and doesn't hold newer episodes back. Only
	// the rules set it (database.ReplaceFeedRules, and when the episode is
	// stored), never UpdateEpisode.
	Hidden bool `json:"hidden" gorm:"not null;default:false"`
	// DroppedAt is when the source feed stopped listing the episode; nil
	// while it lists it. A dropped episode is served only where
	// serve_dropped_episodes (or the feed's, or the episode's own Serve)
	// says so (see feeds.Service.ServesDropped). Only a poll sets it
	// (database.SyncEpisodes), never UpdateEpisode.
	DroppedAt *time.Time `json:"dropped_at"`
	// Serve overrides, for this episode once dropped, whether it is still
	// served: "on", "off", or empty to follow the feed. Set by hand, or to
	// "off" when the source answers that the audio is gone (ServeWarning
	// says so). Never written by UpdateEpisode.
	Serve string `json:"serve" gorm:"not null;default:''"`
	// ServeWarning says why Serve was switched off by Solstein itself; empty
	// otherwise. Never written by UpdateEpisode.
	ServeWarning string `json:"serve_warning" gorm:"not null;default:''"`
	// SourceItem is the episode's <item> as the source last listed it, so
	// it can still be served once dropped. Empty for episodes stored before
	// it was kept. Only polls write it, never UpdateEpisode.
	SourceItem []byte `json:"-"`
	// ProcessNote says what a processor did, e.g. how much it removed.
	ProcessNote string `json:"process_note"`
	// PreparedWith describes the settings the episode's file was made with
	// (or, for a failed episode, those it failed under), so a change of
	// settings can be told apart; see episodes.Pipeline.Reconcile. Empty
	// for episodes from before it was recorded.
	PreparedWith string `json:"prepared_with"`

	// CacheFile is relative to the cache directory; empty when not cached.
	CacheFile string `json:"-"`
	CacheSize int64  `json:"cache_size"`
	// CacheSeconds is the cached file's duration when a processor changed
	// it, served as itunes:duration; zero keeps the source's.
	CacheSeconds int        `json:"cache_seconds"`
	CachedAt     *time.Time `json:"cached_at"`
	// FullyServedAt is when a client first downloaded this cache copy in
	// full; nil until then. Used by cache_evict_after_serve, and cleared
	// along with the rest of the cache fields when the file is evicted or
	// found missing, so a fresh copy starts unserved again.
	FullyServedAt *time.Time `json:"fully_served_at"`
}

// ForgetCache clears the cache fields, after the cached file is deleted or
// found missing.
func (episode *Episode) ForgetCache() {
	episode.CacheFile, episode.CacheSize, episode.CacheSeconds, episode.CachedAt = "", 0, 0, nil
	episode.FullyServedAt = nil
}
