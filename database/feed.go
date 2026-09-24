package database

import (
	"context"
	"errors"
	"fmt"

	"aunefyren/solstein/models"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

var (
	ErrFeedNotFound = errors.New("feed not found")
	ErrFeedExists   = errors.New("feed with this source URL already exists")
)

// CreateFeed stores a new feed and sets its ID. It returns ErrFeedExists if a
// feed with the same source URL is already stored.
func (store *Store) CreateFeed(ctx context.Context, feed *models.Feed) error {
	return store.withContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Checked up front rather than by parsing the unique-index error,
		// whose text depends on the SQLite driver. The index stays as a
		// backstop.
		var count int64
		if err := tx.Model(&models.Feed{}).Where("source_url = ?", feed.SourceURL).Count(&count).Error; err != nil {
			return fmt.Errorf("check for existing feed: %w", err)
		}
		if count > 0 {
			return ErrFeedExists
		}
		if err := tx.Create(feed).Error; err != nil {
			return fmt.Errorf("create feed: %w", err)
		}
		return nil
	})
}

// GetFeed returns the feed with the given ID, or ErrFeedNotFound.
func (store *Store) GetFeed(ctx context.Context, feedID uuid.UUID) (models.Feed, error) {
	return store.findFeed(ctx, "id = ?", feedID)
}

// GetFeedBySourceURL returns the feed with the given (normalised) source URL,
// or ErrFeedNotFound.
func (store *Store) GetFeedBySourceURL(ctx context.Context, sourceURL string) (models.Feed, error) {
	return store.findFeed(ctx, "source_url = ?", sourceURL)
}

func (store *Store) findFeed(ctx context.Context, query string, arg any) (models.Feed, error) {
	var feed models.Feed
	err := store.withContext(ctx).Where(query, arg).Take(&feed).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return models.Feed{}, ErrFeedNotFound
	}
	if err != nil {
		return models.Feed{}, fmt.Errorf("get feed: %w", err)
	}
	return feed, nil
}

// ListFeeds returns all feeds, oldest first.
func (store *Store) ListFeeds(ctx context.Context) ([]models.Feed, error) {
	var feeds []models.Feed
	if err := store.withContext(ctx).Order("created_at").Find(&feeds).Error; err != nil {
		return nil, fmt.Errorf("list feeds: %w", err)
	}
	return feeds, nil
}

// UpdateFeed saves every field of an existing feed. It returns
// ErrFeedNotFound if the feed doesn't exist, rather than creating it.
func (store *Store) UpdateFeed(ctx context.Context, feed *models.Feed) error {
	if feed.ID == uuid.Nil {
		return ErrFeedNotFound
	}
	result := store.withContext(ctx).Model(feed).Select("*").Omit("id", "created_at").Updates(feed)
	if result.Error != nil {
		return fmt.Errorf("update feed: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return ErrFeedNotFound
	}
	return nil
}

// DeleteFeed removes a feed and, through the foreign key, its episodes. It
// returns ErrFeedNotFound if there was nothing to delete.
func (store *Store) DeleteFeed(ctx context.Context, feedID uuid.UUID) error {
	result := store.withContext(ctx).Delete(&models.Feed{}, "id = ?", feedID)
	if result.Error != nil {
		return fmt.Errorf("delete feed: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return ErrFeedNotFound
	}
	return nil
}
