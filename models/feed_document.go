package models

import (
	"time"

	"github.com/google/uuid"
)

// FeedDocument is the last successfully fetched source feed, which the
// served feed is rendered from. It is kept apart from Feed so that listing
// feeds doesn't load megabytes of XML.
type FeedDocument struct {
	FeedID    uuid.UUID `gorm:"type:varchar(36);primaryKey"`
	Feed      Feed      `gorm:"constraint:OnDelete:CASCADE"`
	Data      []byte    `gorm:"not null"`
	FetchedAt time.Time `gorm:"not null"`
}
