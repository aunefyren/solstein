package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"aunefyren/solstein/models"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrFeedDocumentNotFound = errors.New("feed document not found")

// SaveFeedDocument stores the latest source document of a feed, replacing
// the previous one.
func (store *Store) SaveFeedDocument(ctx context.Context, feedID uuid.UUID, data []byte, fetchedAt time.Time) error {
	document := models.FeedDocument{FeedID: feedID, Data: data, FetchedAt: fetchedAt}
	err := store.withContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "feed_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"data", "fetched_at"}),
	}).Omit("Feed").Create(&document).Error
	if err != nil {
		return fmt.Errorf("save feed document: %w", err)
	}
	return nil
}

// GetFeedDocument returns the stored source document of a feed, or
// ErrFeedDocumentNotFound.
func (store *Store) GetFeedDocument(ctx context.Context, feedID uuid.UUID) (models.FeedDocument, error) {
	var document models.FeedDocument
	err := store.withContext(ctx).Where("feed_id = ?", feedID).Take(&document).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return models.FeedDocument{}, ErrFeedDocumentNotFound
	}
	if err != nil {
		return models.FeedDocument{}, fmt.Errorf("get feed document: %w", err)
	}
	return document, nil
}
