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

// SyncEpisodes stores the episodes of a poll: those whose GUID the feed
// doesn't have yet are added, with their hidden flag from the feed's rules,
// and returned. Existing episodes keep their state and cache; only their
// title and stated duration follow the source, and their hidden flag the
// rules, should either change (see applyHiding). The rules are read in the
// same transaction, which holds the write lock from its start, so a poll
// and a rule change never interleave: an episode is never stored with rules
// that were replaced meanwhile.
func (store *Store) SyncEpisodes(ctx context.Context, feedID uuid.UUID, episodes []models.Episode, hiding Hiding) ([]models.Episode, error) {
	var added []models.Episode
	err := store.withContext(ctx).Transaction(func(tx *gorm.DB) error {
		rules, err := listFeedRules(tx, feedID)
		if err != nil {
			return err
		}
		hide := hiding.hider(rules)
		var existing []models.Episode
		if err := tx.Where("feed_id = ?", feedID).Find(&existing).Error; err != nil {
			return fmt.Errorf("list existing episodes: %w", err)
		}
		known := make(map[string]*models.Episode, len(existing))
		for i := range existing {
			known[existing[i].GUID] = &existing[i]
		}
		seen := make(map[string]bool, len(episodes))
		var changed []models.Episode
		for _, episode := range episodes {
			if seen[episode.GUID] {
				continue
			}
			seen[episode.GUID] = true
			if stored, ok := known[episode.GUID]; ok {
				if stored.Title != episode.Title || stored.SourceSeconds != episode.SourceSeconds {
					stored.Title, stored.SourceSeconds = episode.Title, episode.SourceSeconds
					if err := tx.Model(&models.Episode{}).Where("id = ?", stored.ID).
						Updates(map[string]any{"title": stored.Title, "source_seconds": stored.SourceSeconds}).Error; err != nil {
						return fmt.Errorf("update episode details: %w", err)
					}
					changed = append(changed, *stored)
				}
				continue
			}
			episode.FeedID = feedID
			episode.Hidden = hide(episode)
			added = append(added, episode)
		}
		if _, err := applyHiding(tx, changed, hide, hiding); err != nil {
			return err
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
