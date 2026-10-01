package models

import "github.com/google/uuid"

// FeedRule is one of a feed's rules: what to do with the episodes it
// matches. A feed's rules are tried in Position order and the first match
// wins (see docs/feeds.md, Rules). The API shows it as feeds.Rule.
type FeedRule struct {
	Base
	FeedID   uuid.UUID `json:"-" gorm:"type:varchar(36);not null;index"`
	Feed     Feed      `json:"-" gorm:"constraint:OnDelete:CASCADE"`
	Position int       `json:"-" gorm:"not null"`
	// TitleMatches is a regular expression (Go syntax, case-insensitive)
	// the episode's title must contain a match for; empty matches any.
	TitleMatches string `json:"title_matches"`
	// MinSeconds and MaxSeconds bound the source's stated duration; zero
	// leaves that side open. An episode without a stated duration matches
	// neither.
	MinSeconds int `json:"min_seconds"`
	MaxSeconds int `json:"max_seconds"`
	// Action is "hide" or "tag".
	Action string `json:"action"`
	// EpisodeType is what "tag" sets: "full", "bonus" or "trailer".
	EpisodeType string `json:"episode_type,omitempty"`
}
