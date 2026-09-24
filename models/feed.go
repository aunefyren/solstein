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

	LastPolledAt  *time.Time `json:"last_polled_at"`
	LastSuccessAt *time.Time `json:"last_success_at"`
	LastError     string     `json:"last_error"`
	// ETag and LastModified from the last successful poll, for conditional
	// requests to the source.
	ETag         string `json:"-"`
	LastModified string `json:"-"`

	Episodes []Episode `json:"-" gorm:"constraint:OnDelete:CASCADE"`
}
