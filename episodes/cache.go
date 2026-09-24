package episodes

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// partSuffix marks downloads in progress. A file only gets its final name
// once complete, so a half-written file is never served.
const partSuffix = ".part"

// Cache is the directory episode audio is stored in, one subdirectory per
// feed: cache/{feedID}/{episodeID}.{ext}.
type Cache struct {
	directory string
}

// NewCache returns the cache in directory, creating it if needed.
func NewCache(directory string) (Cache, error) {
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return Cache{}, fmt.Errorf("create cache directory %s: %w", directory, err)
	}
	return Cache{directory: directory}, nil
}

// relativePath is what Episode.CacheFile stores: relative, so the config
// directory can be moved.
func relativePath(feedID, episodeID uuid.UUID, extension string) string {
	return filepath.ToSlash(filepath.Join(feedID.String(), episodeID.String()+"."+extension))
}

// Path turns an Episode.CacheFile into a full path. It refuses anything that
// would leave the cache directory, in case the database was tampered with.
func (cache Cache) Path(cacheFile string) (string, error) {
	cleaned := filepath.Clean(filepath.FromSlash(cacheFile))
	if cacheFile == "" || filepath.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid cache file %q", cacheFile)
	}
	return filepath.Join(cache.directory, cleaned), nil
}

// create opens a new part file for an episode. commit renames it to its
// final name; discard removes it.
func (cache Cache) create(feedID, episodeID uuid.UUID, extension string) (file *os.File, commit func() (string, error), discard func(), err error) {
	relative := relativePath(feedID, episodeID, extension)
	final, err := cache.Path(relative)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o750); err != nil {
		return nil, nil, nil, fmt.Errorf("create feed cache directory: %w", err)
	}
	part := final + partSuffix
	file, err = os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create %s: %w", part, err)
	}

	discard = func() {
		file.Close()
		os.Remove(part)
	}
	commit = func() (string, error) {
		if err := file.Sync(); err != nil {
			discard()
			return "", fmt.Errorf("sync %s: %w", part, err)
		}
		if err := file.Close(); err != nil {
			os.Remove(part)
			return "", fmt.Errorf("close %s: %w", part, err)
		}
		if err := os.Rename(part, final); err != nil {
			os.Remove(part)
			return "", fmt.Errorf("move %s into place: %w", part, err)
		}
		return relative, nil
	}
	return file, commit, discard, nil
}

// RemovePartFiles deletes downloads a crash or restart left unfinished.
func (cache Cache) RemovePartFiles() (int, error) {
	removed := 0
	err := filepath.WalkDir(cache.directory, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), partSuffix) {
			if err := os.Remove(path); err != nil {
				return err
			}
			removed++
		}
		return nil
	})
	if err != nil {
		return removed, fmt.Errorf("remove unfinished downloads: %w", err)
	}
	return removed, nil
}
