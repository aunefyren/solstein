package models

import (
	"time"

	"github.com/google/uuid"
)

// EpisodeState is where an episode is in the pipeline; see docs/design.md.
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
	// Backlog marks episodes that already existed when the feed was added.
	// They are fetched on demand instead of at poll time.
	Backlog bool `json:"backlog"`

	State         EpisodeState `json:"state" gorm:"not null;index"`
	Attempts      int          `json:"attempts"`
	NextAttemptAt *time.Time   `json:"next_attempt_at"`
	LastError     string       `json:"last_error"`

	// CacheFile is relative to the cache directory; empty when not cached.
	CacheFile string     `json:"-"`
	CacheSize int64      `json:"cache_size"`
	CachedAt  *time.Time `json:"cached_at"`
}
