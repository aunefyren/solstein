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
	"aunefyren/solstein/models"

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
// than the retention period, over a size cap, already fully served (both
// optional), and files no episode refers to.
type Housekeeper struct {
	store           *database.Store
	cache           Cache
	retention       time.Duration
	maxSize         int64 // bytes; 0 means no cap (cache_max_size_mb)
	evictAfterServe bool
	now             func() time.Time
}

// NewHousekeeper builds a Housekeeper. now may be nil for time.Now.
func NewHousekeeper(store *database.Store, cache Cache, retention time.Duration, maxSize int64, evictAfterServe bool, now func() time.Time) *Housekeeper {
	if now == nil {
		now = time.Now
	}
	return &Housekeeper{store: store, cache: cache, retention: retention, maxSize: maxSize, evictAfterServe: evictAfterServe, now: now}
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
	served, err := housekeeper.evictServed(ctx)
	if err != nil {
		return err
	}
	overCap, err := housekeeper.enforceSizeCap(ctx)
	if err != nil {
		return err
	}
	orphans, freed, err := housekeeper.removeOrphans(ctx)
	if err != nil {
		return err
	}
	if expired > 0 || served > 0 || overCap > 0 || orphans > 0 {
		logger.Log.Info(fmt.Sprintf("Cache clean-up removed %d expired, %d already-served and %d over the size cap, and %d stray files (%.1f MB).", expired, served, overCap, orphans, float64(freed)/(1<<20)))
	}
	return nil
}

// removeCached deletes one episode's cache file and clears its cache
// fields. It reports false, without error, when the file couldn't be
// deleted (being served, on Windows): the entry is left for the next sweep
// rather than forgetting a file that still exists.
func (housekeeper *Housekeeper) removeCached(ctx context.Context, episode models.Episode) (bool, error) {
	fullPath, err := housekeeper.cache.Path(episode.CacheFile)
	if err != nil {
		return false, err
	}
	if err := os.Remove(fullPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		logger.Log.Warn("Failed to delete cache file for episode '" + episode.Title + "'; will retry. Error: " + err.Error())
		return false, nil
	}
	episode.ForgetCache()
	if err := housekeeper.store.UpdateEpisode(ctx, &episode); err != nil && !errors.Is(err, database.ErrEpisodeNotFound) {
		return false, err
	}
	return true, nil
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
	return housekeeper.removeEach(ctx, episodes)
}

// evictServed deletes cache copies of episodes a client has already
// downloaded in full, when cache_evict_after_serve is on: not needed for
// that any more, though a second client (or the same one asking again) then
// costs a fresh download, or, for a processed feed, reprocessing.
func (housekeeper *Housekeeper) evictServed(ctx context.Context) (int, error) {
	if !housekeeper.evictAfterServe {
		return 0, nil
	}
	episodes, err := housekeeper.store.ListFullyServed(ctx)
	if err != nil {
		return 0, err
	}
	return housekeeper.removeEach(ctx, episodes)
}

// enforceSizeCap deletes cache copies, oldest cached first, until the total
// is back under cache_max_size_mb (0 means no cap). It runs after expire
// and evictServed, so it only has to look at what they left.
func (housekeeper *Housekeeper) enforceSizeCap(ctx context.Context) (int, error) {
	if housekeeper.maxSize <= 0 {
		return 0, nil
	}
	episodes, err := housekeeper.store.ListCachedByAge(ctx)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, episode := range episodes {
		total += episode.CacheSize
	}
	removed := 0
	for _, episode := range episodes {
		if total <= housekeeper.maxSize {
			break
		}
		ok, err := housekeeper.removeCached(ctx, episode)
		if err != nil {
			return removed, err
		}
		if ok {
			total -= episode.CacheSize
			removed++
		}
	}
	return removed, nil
}

// removeEach removes a list of episodes' cache copies, counting only the
// ones actually removed.
func (housekeeper *Housekeeper) removeEach(ctx context.Context, episodes []models.Episode) (int, error) {
	removed := 0
	for _, episode := range episodes {
		ok, err := housekeeper.removeCached(ctx, episode)
		if err != nil {
			return removed, err
		}
		if ok {
			removed++
		}
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
