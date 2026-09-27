package database

import (
	"context"
	"fmt"
	"time"

	"aunefyren/solstein/models"
)

// ListCachedBefore returns episodes whose cache copy was made before cutoff.
func (store *Store) ListCachedBefore(ctx context.Context, cutoff time.Time) ([]models.Episode, error) {
	var episodes []models.Episode
	err := store.withContext(ctx).
		Where("cache_file <> '' AND cached_at < ?", cutoff).
		Find(&episodes).Error
	if err != nil {
		return nil, fmt.Errorf("list expired cache entries: %w", err)
	}
	return episodes, nil
}

// ListFullyServed returns cached episodes a client has downloaded in full,
// for cache_evict_after_serve.
func (store *Store) ListFullyServed(ctx context.Context) ([]models.Episode, error) {
	var episodes []models.Episode
	err := store.withContext(ctx).
		Where("cache_file <> '' AND fully_served_at IS NOT NULL").
		Find(&episodes).Error
	if err != nil {
		return nil, fmt.Errorf("list fully served episodes: %w", err)
	}
	return episodes, nil
}

// ListCachedByAge returns every cached episode, oldest cache copy first, for
// enforcing cache_max_size_mb: the oldest are evicted first when it's over.
func (store *Store) ListCachedByAge(ctx context.Context) ([]models.Episode, error) {
	var episodes []models.Episode
	err := store.withContext(ctx).
		Where("cache_file <> ''").
		Order("cached_at").
		Find(&episodes).Error
	if err != nil {
		return nil, fmt.Errorf("list cached episodes by age: %w", err)
	}
	return episodes, nil
}

// CacheFiles returns every cache file an episode refers to, as a set.
func (store *Store) CacheFiles(ctx context.Context) (map[string]bool, error) {
	var files []string
	if err := store.withContext(ctx).Model(&models.Episode{}).Where("cache_file <> ''").Pluck("cache_file", &files).Error; err != nil {
		return nil, fmt.Errorf("list cache files: %w", err)
	}
	set := make(map[string]bool, len(files))
	for _, file := range files {
		set[file] = true
	}
	return set, nil
}
