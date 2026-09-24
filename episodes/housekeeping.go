package episodes

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"aunefyren/solstein/database"
	"aunefyren/solstein/logger"

	"github.com/google/uuid"
)

const (
	sweepInterval = time.Hour
	// minimumFileAge protects files the sweep might otherwise catch between
	// a download finishing and the database recording it, and .part files of
	// downloads still running (which end after downloadTimeout at most).
	minimumFileAge = downloadTimeout + 10*time.Minute
)

// Housekeeper keeps the cache in check: it deletes cached episodes older
// than the retention period and files no episode refers to.
type Housekeeper struct {
	store     *database.Store
	cache     Cache
	retention time.Duration
	now       func() time.Time
}

// NewHousekeeper builds a Housekeeper. now may be nil for time.Now.
func NewHousekeeper(store *database.Store, cache Cache, retention time.Duration, now func() time.Time) *Housekeeper {
	if now == nil {
		now = time.Now
	}
	return &Housekeeper{store: store, cache: cache, retention: retention, now: now}
}

// Run sweeps straight away and then hourly until ctx is cancelled.
func (housekeeper *Housekeeper) Run(ctx context.Context) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		if err := housekeeper.Sweep(ctx); err != nil && ctx.Err() == nil {
			logger.Log.Error("Cache clean-up failed. Error: " + err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Sweep does one round of clean-up.
func (housekeeper *Housekeeper) Sweep(ctx context.Context) error {
	expired, err := housekeeper.expire(ctx)
	if err != nil {
		return err
	}
	orphans, freed, err := housekeeper.removeOrphans(ctx)
	if err != nil {
		return err
	}
	if expired > 0 || orphans > 0 {
		logger.Log.Info(fmt.Sprintf("Cache clean-up removed %d expired episodes and %d stray files (%.1f MB).", expired, orphans, float64(freed)/(1<<20)))
	}
	return nil
}

// expire deletes cache copies older than the retention period. The episode
// stays published: a later play streams it from the source, and in cache
// mode caches it again.
func (housekeeper *Housekeeper) expire(ctx context.Context) (int, error) {
	cutoff := housekeeper.now().UTC().Add(-housekeeper.retention)
	episodes, err := housekeeper.store.ListCachedBefore(ctx, cutoff)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, episode := range episodes {
		fullPath, err := housekeeper.cache.Path(episode.CacheFile)
		if err != nil {
			return removed, err
		}
		// On Windows a file being served can't be deleted; leave the entry
		// for the next sweep rather than forgetting a file that still exists.
		if err := os.Remove(fullPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			logger.Log.Warn("Failed to delete expired cache file for episode '" + episode.Title + "'; will retry. Error: " + err.Error())
			continue
		}
		episode.CacheFile, episode.CacheSize, episode.CachedAt = "", 0, nil
		if err := housekeeper.store.UpdateEpisode(ctx, &episode); err != nil && !errors.Is(err, database.ErrEpisodeNotFound) {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// removeOrphans deletes files in the cache that no episode refers to — left
// by deleted feeds or interrupted downloads — and then empty feed
// directories.
func (housekeeper *Housekeeper) removeOrphans(ctx context.Context) (count int, freed int64, err error) {
	known, err := housekeeper.store.CacheFiles(ctx)
	if err != nil {
		return 0, 0, err
	}
	cutoff := housekeeper.now().Add(-minimumFileAge)
	var directories []string

	err = filepath.WalkDir(housekeeper.cache.directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != housekeeper.cache.directory {
				directories = append(directories, path)
			}
			return nil
		}
		relative, err := filepath.Rel(housekeeper.cache.directory, path)
		if err != nil {
			return err
		}
		if known[filepath.ToSlash(relative)] {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.ModTime().After(cutoff) {
			return nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		count++
		freed += info.Size()
		return nil
	})
	if err != nil {
		return count, freed, fmt.Errorf("remove stray cache files: %w", err)
	}

	// Deepest first; os.Remove only removes empty directories, which is
	// exactly the ones wanted.
	for i := len(directories) - 1; i >= 0; i-- {
		os.Remove(directories[i])
	}
	return count, freed, nil
}

// RemoveFeed deletes a feed's cache directory, e.g. when the feed is deleted.
func (cache Cache) RemoveFeed(feedID uuid.UUID) error {
	if err := os.RemoveAll(filepath.Join(cache.directory, feedID.String())); err != nil {
		return fmt.Errorf("remove cache of feed %s: %w", feedID, err)
	}
	return nil
}
