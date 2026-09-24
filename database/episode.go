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

var (
	ErrEpisodeNotFound = errors.New("episode not found")
	ErrEpisodeExists   = errors.New("episode with this GUID already exists in the feed")
)

// CreateEpisode stores a new episode and sets its ID. It returns
// ErrEpisodeExists if the feed already has an episode with the same GUID, and
// ErrFeedNotFound if the feed doesn't exist.
func (store *Store) CreateEpisode(ctx context.Context, episode *models.Episode) error {
	return store.withContext(ctx).Transaction(func(tx *gorm.DB) error {
		var feedCount int64
		if err := tx.Model(&models.Feed{}).Where("id = ?", episode.FeedID).Count(&feedCount).Error; err != nil {
			return fmt.Errorf("check feed: %w", err)
		}
		if feedCount == 0 {
			return ErrFeedNotFound
		}

		var count int64
		if err := tx.Model(&models.Episode{}).Where("feed_id = ? AND guid = ?", episode.FeedID, episode.GUID).Count(&count).Error; err != nil {
			return fmt.Errorf("check for existing episode: %w", err)
		}
		if count > 0 {
			return ErrEpisodeExists
		}

		if err := tx.Create(episode).Error; err != nil {
			return fmt.Errorf("create episode: %w", err)
		}
		return nil
	})
}

// GetEpisode returns an episode of the given feed, or ErrEpisodeNotFound.
// Scoping by feed means an episode ID can't be served under another feed's
// (signed) URL.
func (store *Store) GetEpisode(ctx context.Context, feedID, episodeID uuid.UUID) (models.Episode, error) {
	var episode models.Episode
	err := store.withContext(ctx).Where("id = ? AND feed_id = ?", episodeID, feedID).Take(&episode).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return models.Episode{}, ErrEpisodeNotFound
	}
	if err != nil {
		return models.Episode{}, fmt.Errorf("get episode: %w", err)
	}
	return episode, nil
}

// ListEpisodes returns a feed's episodes, oldest publish date first; episodes
// without a publish date come last. That is the order they must be published
// in (see docs/design.md, Publish policy).
func (store *Store) ListEpisodes(ctx context.Context, feedID uuid.UUID) ([]models.Episode, error) {
	var episodes []models.Episode
	err := store.withContext(ctx).
		Where("feed_id = ?", feedID).
		Order("published_at IS NULL, published_at, created_at").
		Find(&episodes).Error
	if err != nil {
		return nil, fmt.Errorf("list episodes: %w", err)
	}
	return episodes, nil
}

// MarkReleased records when episodes first appeared in a served feed. An
// episode already marked keeps its first time, so concurrent renders agree.
func (store *Store) MarkReleased(ctx context.Context, episodeIDs []uuid.UUID, at time.Time) error {
	if len(episodeIDs) == 0 {
		return nil
	}
	err := store.withContext(ctx).Model(&models.Episode{}).
		Where("id IN ? AND released_at IS NULL", episodeIDs).
		Update("released_at", at).Error
	if err != nil {
		return fmt.Errorf("mark episodes released: %w", err)
	}
	return nil
}

// UpdateEpisode saves every field of an existing episode except
// released_at, which only MarkReleased sets: a download or stream that loaded
// the episode before it was released would otherwise clear it again when it
// saves. It returns ErrEpisodeNotFound if the episode doesn't exist, rather
// than creating it.
func (store *Store) UpdateEpisode(ctx context.Context, episode *models.Episode) error {
	if episode.ID == uuid.Nil {
		return ErrEpisodeNotFound
	}
	result := store.withContext(ctx).Model(episode).Select("*").Omit("id", "created_at", "released_at").Updates(episode)
	if result.Error != nil {
		return fmt.Errorf("update episode: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return ErrEpisodeNotFound
	}
	return nil
}
