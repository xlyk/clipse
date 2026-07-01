package store

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite" // registers the "sqlite" driver (pure Go, no cgo)
)

// Store wraps a SQLite-backed kernel database: issues, runs, events.
type Store struct {
	db *sql.DB
}

// Open opens (creating if necessary) the SQLite database at path, enables
// WAL journaling and a busy timeout so concurrent dispatcher/TUI readers
// don't fail immediately on lock contention, and runs migrations. Migrating
// an empty database creates all tables; migrating an already-migrated
// database is a no-op (every migration statement is idempotent).
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite db %s: %w", path, err)
	}

	if _, err := db.Exec(`PRAGMA journal_mode = WAL`); err != nil {
		db.Close()
		return nil, fmt.Errorf("setting journal_mode=WAL: %w", err)
	}
	if _, err := db.Exec(`PRAGMA busy_timeout = 5000`); err != nil {
		db.Close()
		return nil, fmt.Errorf("setting busy_timeout: %w", err)
	}

	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating sqlite db %s: %w", path, err)
	}

	return &Store{db: db}, nil
}

// DB exposes the underlying *sql.DB for callers (tests, future packages)
// that need to run ad-hoc queries the Store doesn't otherwise expose.
func (s *Store) DB() *sql.DB {
	return s.db
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("closing sqlite db: %w", err)
	}
	return nil
}
