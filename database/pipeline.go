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
// An episode is waiting when it is discovered, its retry time has come, and its feed uses cache delivery — set on the feed, or inherited
// from defaultMode when the feed has none — or is one of processedFeeds,
// whose episodes a processor prepares whatever the delivery mode.
func (store *Store) ClaimNextEpisode(ctx context.Context, now time.Time, defaultMode string, processedFeeds []uuid.UUID) (models.Episode, error) {
	var episode models.Episode
	err := store.withContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := preparedFeeds(tx, defaultMode, processedFeeds).
			Where("episodes.state = ?", models.EpisodeDiscovered).
			Where("episodes.next_attempt_at IS NULL OR episodes.next_attempt_at <= ?", now)
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

// preparedFeeds starts an episode query limited to the feeds whose episodes
// the pipeline prepares: cache delivery, set on the feed or inherited from
// defaultMode, or one of processedFeeds.
func preparedFeeds(tx *gorm.DB, defaultMode string, processedFeeds []uuid.UUID) *gorm.DB {
	query := tx.Model(&models.Episode{}).Joins("JOIN feeds ON feeds.id = episodes.feed_id")
	cacheModes := []string{"cache"}
	if defaultMode == "cache" {
		cacheModes = append(cacheModes, "")
	}
	if len(processedFeeds) > 0 {
		return query.Where("(feeds.delivery_mode IN ? OR feeds.id IN ?)", cacheModes, processedFeeds)
	}
	return query.Where("feeds.delivery_mode IN ?", cacheModes)
}

// ClaimQueuedEpisode picks the episode queued longest for a background
// attempt — a failed episode due a retry, or a published one without its
// file queued to be prepared ahead — whose feed the pipeline prepares (as
// for ClaimNextEpisode). Among episodes queued at the same time, the newest
// goes first: they are published already, so there is no order to keep, and
// listeners start from the newest.
//
// Its state is left as it is, so it stays published or withheld meanwhile.
// It is claimed by moving its attempt time on to leaseUntil, in the same
// transaction, so another worker can't take it too; if the attempt never
// records an outcome (a crash), it comes due again then. It returns
// ErrNoWork when nothing is due.
func (store *Store) ClaimQueuedEpisode(ctx context.Context, now, leaseUntil time.Time, defaultMode string, processedFeeds []uuid.UUID) (models.Episode, error) {
	var episode models.Episode
	err := store.withContext(ctx).Transaction(func(tx *gorm.DB) error {
		err := preparedFeeds(tx, defaultMode, processedFeeds).
			Where("episodes.next_attempt_at IS NOT NULL AND episodes.next_attempt_at <= ?", now).
			Where("episodes.state = ? OR (episodes.state = ? AND episodes.cache_file = '')", models.EpisodeFailed, models.EpisodeReady).
			Order("episodes.next_attempt_at, episodes.published_at IS NULL, episodes.published_at DESC, episodes.created_at DESC").
			Select("episodes.*").
			Take(&episode).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNoWork
		}
		if err != nil {
			return fmt.Errorf("find queued episode: %w", err)
		}

		// Still due, so not claimed by someone else in between.
		result := tx.Model(&models.Episode{}).
			Where("id = ? AND next_attempt_at <= ?", episode.ID, now).
			Updates(map[string]any{"next_attempt_at": leaseUntil, "updated_at": now})
		if result.Error != nil {
			return fmt.Errorf("claim queued episode: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return ErrNoWork // claimed by someone else in between
		}
		episode.NextAttemptAt = &leaseUntil
		return nil
	})
	if err != nil {
		return models.Episode{}, err
	}
	return episode, nil
}

// ClaimEpisode marks one discovered episode as acquiring, whatever its retry
// time: a client has asked for it, so it is prepared now. It returns
// ErrNoWork when the episode isn't discovered (a worker has it, or it is
// done).
func (store *Store) ClaimEpisode(ctx context.Context, episodeID uuid.UUID, now time.Time) (models.Episode, error) {
	var episode models.Episode
	err := store.withContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&models.Episode{}).
			Where("id = ? AND state = ?", episodeID, models.EpisodeDiscovered).
			Updates(map[string]any{"state": models.EpisodeAcquiring, "updated_at": now})
		if result.Error != nil {
			return fmt.Errorf("claim episode: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return ErrNoWork
		}
		if err := tx.Where("id = ?", episodeID).Take(&episode).Error; err != nil {
			return fmt.Errorf("load claimed episode: %w", err)
		}
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

// QueueFailedEpisodes queues those of the given episodes that are failed
// for a background attempt at the given time, with their late retries
// counted afresh. It returns how many it queued.
func (store *Store) QueueFailedEpisodes(ctx context.Context, episodeIDs []uuid.UUID, at time.Time) (int64, error) {
	return store.queueEpisodes(ctx, episodeIDs, map[string]any{"next_attempt_at": at, "late_retries": 0, "updated_at": at},
		"state = ?", models.EpisodeFailed)
}

// QueueUncachedEpisodes queues those of the given episodes that are
// published without their file to be prepared at the given time. It
// returns how many it queued.
func (store *Store) QueueUncachedEpisodes(ctx context.Context, episodeIDs []uuid.UUID, at time.Time) (int64, error) {
	return store.queueEpisodes(ctx, episodeIDs, map[string]any{"next_attempt_at": at, "updated_at": at},
		"state = ? AND cache_file = ''", models.EpisodeReady)
}

// queueEpisodes updates only the queueing fields, and only of episodes that
// still match the condition, so an episode prepared in the meantime isn't
// overwritten with an older copy.
func (store *Store) queueEpisodes(ctx context.Context, episodeIDs []uuid.UUID, fields map[string]any, condition string, args ...any) (int64, error) {
	if len(episodeIDs) == 0 {
		return 0, nil
	}
	result := store.withContext(ctx).Model(&models.Episode{}).
		Where("id IN ?", episodeIDs).
		Where(condition, args...).
		Updates(fields)
	if result.Error != nil {
		return 0, fmt.Errorf("queue episodes: %w", result.Error)
	}
	return result.RowsAffected, nil
}
