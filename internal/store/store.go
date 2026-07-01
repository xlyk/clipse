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
	// journal_mode and busy_timeout are set via _pragma DSN params (rather
	// than a one-off db.Exec after Open) because database/sql pools multiple
	// underlying connections: an Exec'd PRAGMA only lands on whichever
	// connection happens to run it, not on connections the pool opens later
	// under load. _pragma is applied by the driver to every connection it
	// opens, so all of them get WAL + the busy timeout.
	//
	// _txlock=immediate makes every write transaction (db.BeginTx with a
	// non-read-only *sql.Tx, e.g. ClaimReady's CAS) issue "BEGIN IMMEDIATE"
	// instead of SQLite's default "BEGIN DEFERRED". Deferred transactions
	// don't take the reserved lock until their first write, so two
	// concurrent claimers could both pass a read-only check before either
	// writes; immediate transactions take the reserved lock up front and
	// serialize at that point, which is what makes the CAS claim race-free.
	dsn := path + "?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite db %s: %w", path, err)
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
