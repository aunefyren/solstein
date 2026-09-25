package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"aunefyren/solstein/models"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ErrNoWork means there is no episode waiting to be downloaded right now.
var ErrNoWork = errors.New("no episode waiting")

// ClaimNextEpisode picks the oldest episode that is waiting to be downloaded
// into the cache and marks it as acquiring, all in one transaction, so two
// workers can never claim the same episode. Oldest first, because episodes
// are published strictly in order and a pending old one holds back newer
// ones.
//
// An episode is waiting when it is discovered (not backlog), its retry time
// has come, and its feed uses cache delivery — set on the feed, or inherited
// from defaultMode when the feed has none — or is one of processedFeeds,
// whose episodes a processor prepares whatever the delivery mode.
func (store *Store) ClaimNextEpisode(ctx context.Context, now time.Time, defaultMode string, processedFeeds []uuid.UUID) (models.Episode, error) {
	var episode models.Episode
	err := store.withContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx.Model(&models.Episode{}).
			Joins("JOIN feeds ON feeds.id = episodes.feed_id").
			Where("episodes.state = ? AND episodes.backlog = ?", models.EpisodeDiscovered, false).
			Where("episodes.next_attempt_at IS NULL OR episodes.next_attempt_at <= ?", now)
		cacheModes := []string{"cache"}
		if defaultMode == "cache" {
			cacheModes = append(cacheModes, "")
		}
		if len(processedFeeds) > 0 {
			query = query.Where("(feeds.delivery_mode IN ? OR feeds.id IN ?)", cacheModes, processedFeeds)
		} else {
			query = query.Where("feeds.delivery_mode IN ?", cacheModes)
		}
		err := query.Order("episodes.published_at IS NULL, episodes.published_at, episodes.created_at").
			Select("episodes.*").
			Take(&episode).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNoWork
		}
		if err != nil {
			return fmt.Errorf("find waiting episode: %w", err)
		}

		result := tx.Model(&models.Episode{}).
			Where("id = ? AND state = ?", episode.ID, models.EpisodeDiscovered).
			Updates(map[string]any{"state": models.EpisodeAcquiring, "updated_at": now})
		if result.Error != nil {
			return fmt.Errorf("claim episode: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return ErrNoWork // claimed by someone else in between
		}
		episode.State = models.EpisodeAcquiring
		return nil
	})
	if err != nil {
		return models.Episode{}, err
	}
	return episode, nil
}

// ResetInterruptedEpisodes puts episodes left acquiring by a crash or restart
// back to discovered, so they are downloaded again. It returns how many.
func (store *Store) ResetInterruptedEpisodes(ctx context.Context) (int64, error) {
	result := store.withContext(ctx).Model(&models.Episode{}).
		Where("state = ?", models.EpisodeAcquiring).
		Update("state", models.EpisodeDiscovered)
	if result.Error != nil {
		return 0, fmt.Errorf("reset interrupted episodes: %w", result.Error)
	}
	return result.RowsAffected, nil
}
