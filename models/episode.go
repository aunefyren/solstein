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
}

// ForgetCache clears the cache fields, after the cached file is deleted or
// found missing.
func (episode *Episode) ForgetCache() {
	episode.CacheFile, episode.CacheSize, episode.CacheSeconds, episode.CachedAt = "", 0, 0, nil
}
