package database

import (
	"context"
	"fmt"
	"time"

	"aunefyren/solstein/models"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// CreateSubscription stores a new feed together with its first source
// document and episodes, all or nothing, so a half-created feed never
// exists. It returns ErrFeedExists if the source URL is already subscribed.
func (store *Store) CreateSubscription(ctx context.Context, feed *models.Feed, data []byte, fetchedAt time.Time, episodes []models.Episode) error {
	return store.withContext(ctx).Transaction(func(tx *gorm.DB) error {
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
		document := models.FeedDocument{FeedID: feed.ID, Data: data, FetchedAt: fetchedAt}
		if err := tx.Omit("Feed").Create(&document).Error; err != nil {
			return fmt.Errorf("create feed document: %w", err)
		}
		for i := range episodes {
			episodes[i].FeedID = feed.ID
		}
		if len(episodes) > 0 {
			if err := tx.CreateInBatches(episodes, 200).Error; err != nil {
				return fmt.Errorf("create episodes: %w", err)
			}
		}
		return nil
	})
}

// AddNewEpisodes stores those of episodes whose GUID the feed doesn't have
// yet, and returns the ones it stored. Existing episodes are left untouched,
// so their state and cache survive a poll.
func (store *Store) AddNewEpisodes(ctx context.Context, feedID uuid.UUID, episodes []models.Episode) ([]models.Episode, error) {
	var added []models.Episode
	err := store.withContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing []string
		if err := tx.Model(&models.Episode{}).Where("feed_id = ?", feedID).Pluck("guid", &existing).Error; err != nil {
			return fmt.Errorf("list existing episodes: %w", err)
		}
		known := make(map[string]bool, len(existing))
		for _, guid := range existing {
			known[guid] = true
		}
		for _, episode := range episodes {
			if known[episode.GUID] {
				continue
			}
			known[episode.GUID] = true
			episode.FeedID = feedID
			added = append(added, episode)
		}
		if len(added) > 0 {
			if err := tx.CreateInBatches(added, 200).Error; err != nil {
				return fmt.Errorf("create episodes: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return added, nil
}
