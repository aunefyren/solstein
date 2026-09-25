package models

import "time"

// Feed is a source podcast feed that Solstein proxies. Per-feed settings left
// at their zero value fall back to the global settings in config.json.
type Feed struct {
	Base
	// SourceURL is normalised before it is stored, so the same feed added
	// twice maps to one record.
	SourceURL string `json:"source_url" gorm:"not null;uniqueIndex"`
	Title     string `json:"title"`

	Exit                string `json:"exit"`
	DeliveryMode        string `json:"delivery_mode"`
	PollIntervalMinutes int    `json:"poll_interval_minutes"`
	// RegionDiff is "on" or "off", or empty to follow region_diff.enabled.
	RegionDiff string `json:"region_diff"`
	// RegionDiffExits overrides region_diff.exits for this feed: two exits,
	// the home region first. Empty uses the global pair.
	RegionDiffExits []string `json:"region_diff_exits" gorm:"serializer:json"`
	// RegionDiffOnFailure overrides region_diff.on_failure ("publish" or
	// "hide"); empty uses the global policy.
	RegionDiffOnFailure string `json:"region_diff_on_failure"`
	// RegionDiffTrimBreakMarkers is "on" or "off", or empty to follow
	// region_diff.trim_break_markers.
	RegionDiffTrimBreakMarkers string `json:"region_diff_trim_break_markers"`

	LastPolledAt  *time.Time `json:"last_polled_at"`
	LastSuccessAt *time.Time `json:"last_success_at"`
	LastError     string     `json:"last_error"`
	// ETag and LastModified from the last successful poll, for conditional
	// requests to the source.
	ETag         string `json:"-"`
	LastModified string `json:"-"`

	Episodes []Episode `json:"-" gorm:"constraint:OnDelete:CASCADE"`
}
