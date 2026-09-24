// Package database is Solstein's persistence layer: a SQLite database in the
// config directory, accessed through GORM. Everything else goes through the
// named functions on Store; no other package imports gorm.io/*.
package database

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"

	"aunefyren/solstein/models"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormLogger "gorm.io/gorm/logger"

	// CGO-free SQLite driver, registered as "sqlite". The release binary is
	// built with CGO_ENABLED=0, so the GORM driver's default (mattn) can't be
	// used; the connection is opened here and handed to GORM instead.
	_ "modernc.org/sqlite"
)

const fileName = "solstein.db"

// Pragmas are set per connection through the DSN, so every pooled connection
// gets them. WAL lets polls, downloads and client requests read while one of
// them writes; busy_timeout makes a writer wait for the lock instead of
// failing; foreign_keys is off by default in SQLite and is needed for the
// feed → episode cascade.
const pragmas = "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"

// Store is an open database. It is safe for concurrent use.
type Store struct {
	db *gorm.DB
}

// Open opens (creating if needed) the database in configDir and migrates the
// schema.
func Open(configDir string) (*Store, error) {
	path := filepath.Join(configDir, fileName)

	sqlDB, err := sql.Open("sqlite", "file:"+path+pragmas)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	db, err := gorm.Open(sqlite.Dialector{Conn: sqlDB}, &gorm.Config{
		// Errors are returned to callers and logged there; GORM's own logger
		// would print to stdout outside logrus.
		Logger: gormLogger.Discard,
	})
	if err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	store := &Store{db: db}
	if err := store.migrate(); err != nil {
		store.Close()
		return nil, err
	}
	return store, nil
}

func (store *Store) migrate() error {
	if err := store.db.AutoMigrate(&models.Feed{}, &models.Episode{}, &models.FeedDocument{}); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	return nil
}

// Close closes the database.
func (store *Store) Close() error {
	sqlDB, err := store.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// withContext scopes a query to ctx, so a cancelled request or shutdown
// abandons it.
func (store *Store) withContext(ctx context.Context) *gorm.DB {
	return store.db.WithContext(ctx)
}
