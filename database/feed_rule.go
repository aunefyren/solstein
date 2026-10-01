package database

import (
	"context"
	"fmt"
	"time"

	"aunefyren/solstein/models"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Hiding is how a feed's rules act on its episodes when they are stored or
// the rules change.
type Hiding struct {
	// HiderFor builds, from the feed's rules as stored, what hides an
	// episode. Nil hides nothing.
	HiderFor func(rules []models.FeedRule) func(models.Episode) bool
	// QueueShown queues an episode no longer hidden that has no file to be
	// prepared in the background at Now: a feed preparing ahead publishes
	// nothing without its file, so nothing would ask for it otherwise.
	QueueShown bool
	Now        time.Time
}

// ListFeedRules returns a feed's rules in the order they are tried. A feed
// without rules, or one that doesn't exist, has none.
func (store *Store) ListFeedRules(ctx context.Context, feedID uuid.UUID) ([]models.FeedRule, error) {
	return listFeedRules(store.withContext(ctx), feedID)
}

func listFeedRules(tx *gorm.DB, feedID uuid.UUID) ([]models.FeedRule, error) {
	var rules []models.FeedRule
	if err := tx.Where("feed_id = ?", feedID).Order("position").Find(&rules).Error; err != nil {
		return nil, fmt.Errorf("list feed rules: %w", err)
	}
	return rules, nil
}

// ReplaceFeedRules replaces a feed's rules with rules, in that order, and
// sets every episode's hidden flag from them in the same transaction, so
// the episodes never disagree with the rules stored (see applyHiding). It
// returns how many episodes it stopped hiding, and ErrFeedNotFound if the
// feed doesn't exist.
func (store *Store) ReplaceFeedRules(ctx context.Context, feedID uuid.UUID, rules []models.FeedRule, hiding Hiding) (shown int, err error) {
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
		shown, err = applyHiding(tx, episodes, hiding.hider(rules), hiding)
		return err
	})
	if err != nil {
		return 0, err
	}
	return shown, nil
}

func (hiding Hiding) hider(rules []models.FeedRule) func(models.Episode) bool {
	if hiding.HiderFor == nil {
		return func(models.Episode) bool { return false }
	}
	return hiding.HiderFor(rules)
}

// applyHiding brings stored episodes' hidden flags in line with hide, and
// returns how many it stopped hiding.
//
// An episode shown again that was never published (hidden from the moment
// it was found) becomes backlog, ready without its file: it keeps its own
// date and is prepared when a client asks, as if it had been there when the
// feed was added. Published now as new, it would be dated now
// (feeds.Service.Render), so ABS would auto-download a burst of old
// episodes, and in cache mode one still waiting would hold back every newer
// one until it was downloaded.
func applyHiding(tx *gorm.DB, episodes []models.Episode, hide func(models.Episode) bool, hiding Hiding) (int, error) {
	var hidden, visible, backlog []uuid.UUID
	for _, episode := range episodes {
		switch wanted := hide(episode); {
		case wanted && !episode.Hidden:
			hidden = append(hidden, episode.ID)
		case !wanted && episode.Hidden && episode.ReleasedAt == nil && !episode.Backlog &&
			(episode.State == models.EpisodeDiscovered || episode.State == models.EpisodeReady):
			backlog = append(backlog, episode.ID)
		case !wanted && episode.Hidden:
			visible = append(visible, episode.ID)
		}
	}
	if len(hidden) > 0 {
		if err := tx.Model(&models.Episode{}).Where("id IN ?", hidden).Update("hidden", true).Error; err != nil {
			return 0, fmt.Errorf("hide episodes: %w", err)
		}
	}
	if len(visible) > 0 {
		if err := tx.Model(&models.Episode{}).Where("id IN ?", visible).Update("hidden", false).Error; err != nil {
			return 0, fmt.Errorf("show episodes: %w", err)
		}
	}
	if len(backlog) > 0 {
		fields := map[string]any{"hidden": false, "backlog": true, "state": models.EpisodeReady, "next_attempt_at": nil}
		if hiding.QueueShown {
			fields["next_attempt_at"] = hiding.Now
		}
		if err := tx.Model(&models.Episode{}).Where("id IN ?", backlog).Updates(fields).Error; err != nil {
			return 0, fmt.Errorf("show episodes as backlog: %w", err)
		}
		if hiding.QueueShown {
			// Only those without their file are queued.
			if err := tx.Model(&models.Episode{}).Where("id IN ? AND cache_file <> ''", backlog).Update("next_attempt_at", nil).Error; err != nil {
				return 0, fmt.Errorf("show episodes as backlog: %w", err)
			}
		}
	}
	return len(visible) + len(backlog), nil
}
