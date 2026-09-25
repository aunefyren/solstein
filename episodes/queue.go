package episodes

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"aunefyren/solstein/logger"
	"aunefyren/solstein/models"

	"github.com/google/uuid"
)

// ErrNotPrepared means a feed's episodes aren't prepared by the pipeline
// (stream or original mode, not processed), so there is nothing to queue.
var ErrNotPrepared = errors.New("the feed's episodes are not prepared")

// RetryFailed queues every failed episode of a feed, withheld or published
// unprocessed, for another attempt in the background, e.g. after a fix. A
// withheld episode that fails again gets the slow retries afresh. It
// returns how many were queued.
func (pipeline *Pipeline) RetryFailed(ctx context.Context, feedID uuid.UUID) (int, error) {
	return pipeline.queueEpisodes(ctx, feedID, 0, "tried again", pipeline.store.QueueFailedEpisodes)
}

// Queue queues a feed's newest episodes (all of them when newest is zero or
// less) that are published without their file — backlog, or expired from
// the cache — to be prepared in the background, so no client has to wait
// for them. The workers take them after new episodes, two at a time, which
// also keeps a large backlog from bursting downloads at the host. It
// returns how many were queued.
func (pipeline *Pipeline) Queue(ctx context.Context, feedID uuid.UUID, newest int) (int, error) {
	return pipeline.queueEpisodes(ctx, feedID, newest, "prepared ahead", pipeline.store.QueueUncachedEpisodes)
}

// queueEpisodes queues a feed's newest episodes through queue, which picks
// those that qualify, and wakes the workers. Episodes being prepared
// are left alone. purpose completes "to be …" in the log.
func (pipeline *Pipeline) queueEpisodes(ctx context.Context, feedID uuid.UUID, newest int, purpose string, queue func(context.Context, []uuid.UUID, time.Time) (int64, error)) (int, error) {
	feed, err := pipeline.store.GetFeed(ctx, feedID)
	if err != nil {
		return 0, err
	}
	if !pipeline.prepares(feed) {
		return 0, ErrNotPrepared
	}
	list, err := pipeline.store.ListEpisodes(ctx, feedID)
	if err != nil {
		return 0, err
	}
	newestFirst(list)
	if newest > 0 && newest < len(list) {
		list = list[:newest]
	}
	var ids []uuid.UUID
	for _, episode := range list {
		if !pipeline.preparing(episode.ID) {
			ids = append(ids, episode.ID)
		}
	}

	queued, err := queue(ctx, ids, pipeline.options.Now().UTC())
	if err != nil {
		return 0, err
	}
	if queued > 0 {
		logger.Log.Info(fmt.Sprintf("Queued %d episodes of '%s' to be %s in the background.", queued, feed.Title, purpose))
		pipeline.Wake()
	}
	return int(queued), nil
}

// newestFirst sorts episodes by publish date, newest first; undated ones
// count as oldest.
func newestFirst(list []models.Episode) {
	slices.SortStableFunc(list, func(a, b models.Episode) int {
		switch {
		case a.PublishedAt == nil && b.PublishedAt == nil:
			return 0
		case a.PublishedAt == nil:
			return 1
		case b.PublishedAt == nil:
			return -1
		}
		return b.PublishedAt.Compare(*a.PublishedAt)
	})
}
