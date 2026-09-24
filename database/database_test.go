package database

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"aunefyren/solstein/models"
)

// openTestStore opens a fresh database in a temp directory. A file rather
// than :memory:, because every pooled connection to :memory: would get its
// own empty database.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func createTestFeed(t *testing.T, store *Store, sourceURL string) models.Feed {
	t.Helper()
	feed := models.Feed{SourceURL: sourceURL, Title: "Test Show"}
	if err := store.CreateFeed(context.Background(), &feed); err != nil {
		t.Fatalf("CreateFeed: %v", err)
	}
	return feed
}

func TestOpenCreatesFileAndPragmas(t *testing.T) {
	configDir := t.TempDir()
	store, err := Open(configDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	if _, err := os.Stat(filepath.Join(configDir, fileName)); err != nil {
		t.Fatalf("database file not created: %v", err)
	}

	var foreignKeys int
	if err := store.db.Raw("PRAGMA foreign_keys").Scan(&foreignKeys).Error; err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 {
		t.Errorf("foreign_keys = %d, want 1", foreignKeys)
	}

	var journalMode string
	if err := store.db.Raw("PRAGMA journal_mode").Scan(&journalMode).Error; err != nil {
		t.Fatal(err)
	}
	if journalMode != "wal" {
		t.Errorf("journal_mode = %q, want wal", journalMode)
	}
}

func TestOpenReopensExistingDatabase(t *testing.T) {
	configDir := t.TempDir()
	store, err := Open(configDir)
	if err != nil {
		t.Fatal(err)
	}
	feed := createTestFeed(t, store, "https://example.com/feed")
	store.Close()

	store, err = Open(configDir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer store.Close()
	if _, err := store.GetFeed(context.Background(), feed.ID); err != nil {
		t.Errorf("feed lost after reopen: %v", err)
	}
}

func TestOpenFailsOnMissingDirectory(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("expected an error for a missing config directory")
	}
}
