package database

import (
	"context"
	"fmt"

	"aunefyren/solstein/models"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ListFeedRules returns a feed's rules in the order they are tried. A feed
// without rules, or one that doesn't exist, has none.
func (store *Store) ListFeedRules(ctx context.Context, feedID uuid.UUID) ([]models.FeedRule, error) {
	var rules []models.FeedRule
	if err := store.withContext(ctx).Where("feed_id = ?", feedID).Order("position").Find(&rules).Error; err != nil {
		return nil, fmt.Errorf("list feed rules: %w", err)
	}
	return rules, nil
}

// ReplaceFeedRules replaces a feed's rules with rules, in that order, and
// sets every episode's hidden flag from hide in the same transaction, so
// the episodes never disagree with the rules stored. It returns how many
// episodes it stopped hiding, and ErrFeedNotFound if the feed doesn't exist.
func (store *Store) ReplaceFeedRules(ctx context.Context, feedID uuid.UUID, rules []models.FeedRule, hide func(models.Episode) bool) (shown int, err error) {
	err = store.withContext(ctx).Transaction(func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&models.Feed{}).Where("id = ?", feedID).Count(&count).Error; err != nil {
			return fmt.Errorf("check feed: %w", err)
		}
		if count == 0 {
			return ErrFeedNotFound
		}
		if err := tx.Where("feed_id = ?", feedID).Delete(&models.FeedRule{}).Error; err != nil {
			return fmt.Errorf("delete feed rules: %w", err)
		}
		for i := range rules {
			rules[i].ID, rules[i].FeedID, rules[i].Position = uuid.Nil, feedID, i
		}
		if len(rules) > 0 {
			if err := tx.Omit("Feed").Create(&rules).Error; err != nil {
				return fmt.Errorf("create feed rules: %w", err)
			}
		}

		var episodes []models.Episode
		if err := tx.Where("feed_id = ?", feedID).Find(&episodes).Error; err != nil {
			return fmt.Errorf("list episodes: %w", err)
		}
		var hidden, visible []uuid.UUID
		for _, episode := range episodes {
			switch wanted := hide(episode); {
			case wanted && !episode.Hidden:
				hidden = append(hidden, episode.ID)
			case !wanted && episode.Hidden:
				visible = append(visible, episode.ID)
			}
		}
		for _, change := range []struct {
			ids    []uuid.UUID
			hidden bool
		}{{hidden, true}, {visible, false}} {
			if len(change.ids) == 0 {
				continue
			}
			if err := tx.Model(&models.Episode{}).Where("id IN ?", change.ids).Update("hidden", change.hidden).Error; err != nil {
				return fmt.Errorf("update hidden episodes: %w", err)
			}
		}
		shown = len(visible)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return shown, nil
}
