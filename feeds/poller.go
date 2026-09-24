package feeds

import (
	"context"
	"fmt"
	"time"

	"aunefyren/solstein/logger"
	"aunefyren/solstein/models"
)

const (
	// pollCheckInterval is how often the poller looks for feeds that are due.
	pollCheckInterval = time.Minute
	// pollTimeout bounds one feed's poll.
	pollTimeout = time.Minute
)

// Poller refreshes feeds on their schedule.
type Poller struct {
	service         *Service
	defaultInterval time.Duration
	// onNewEpisodes is called after a poll stored new episodes, so the
	// episode pipeline can start on them at once.
	onNewEpisodes func()
}

// NewPoller builds a Poller. defaultInterval applies to feeds without their
// own; onNewEpisodes may be nil.
func NewPoller(service *Service, defaultInterval time.Duration, onNewEpisodes func()) *Poller {
	if onNewEpisodes == nil {
		onNewEpisodes = func() {}
	}
	return &Poller{service: service, defaultInterval: defaultInterval, onNewEpisodes: onNewEpisodes}
}

// Run polls due feeds straight away and then every minute until ctx is
// cancelled.
func (poller *Poller) Run(ctx context.Context) {
	ticker := time.NewTicker(pollCheckInterval)
	defer ticker.Stop()
	for {
		if err := poller.PollDue(ctx); err != nil && ctx.Err() == nil {
			logger.Log.Error("Failed to check for feeds to poll. Error: " + err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// PollDue refreshes every feed whose interval has passed since its last
// poll, one at a time: feeds are few, and polling them in parallel would
// only burst requests at the hosts.
func (poller *Poller) PollDue(ctx context.Context) error {
	list, err := poller.service.List(ctx)
	if err != nil {
		return err
	}
	now := poller.service.options.Now()
	for _, feed := range list {
		if ctx.Err() != nil {
			return nil
		}
		if !poller.due(feed, now) {
			continue
		}
		poller.poll(ctx, feed)
	}
	return nil
}

func (poller *Poller) due(feed models.Feed, now time.Time) bool {
	if feed.LastPolledAt == nil {
		return true
	}
	interval := poller.defaultInterval
	if feed.PollIntervalMinutes > 0 {
		interval = time.Duration(feed.PollIntervalMinutes) * time.Minute
	}
	return now.Sub(*feed.LastPolledAt) >= interval
}

func (poller *Poller) poll(ctx context.Context, feed models.Feed) {
	pollCtx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()

	added, err := poller.service.Refresh(pollCtx, &feed)
	if err != nil {
		if ctx.Err() == nil {
			logger.Log.Warn("Failed to poll feed '" + feed.Title + "'; still serving the last good copy. Error: " + err.Error())
		}
		return
	}
	if len(added) > 0 {
		logger.Log.Info(fmt.Sprintf("Found %d new episodes in '%s'.", len(added), feed.Title))
		poller.onNewEpisodes()
	}
}
