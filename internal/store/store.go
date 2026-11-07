package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

type Store struct {
	sql *sql.DB
	ev  eventSignal
}

func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}
	// busy_timeout: SQLite's lock wait polls and isn't fair, so under a
	// burst of writers (engine workers, fan-out, video jobs, requests) one
	// can lose the lock for seconds while others get through. 15 s keeps a
	// result write from failing after its post went out.
	dsn := "file:" + path +
		"?_pragma=busy_timeout(15000)&_pragma=journal_mode(WAL)" +
		"&_pragma=temp_store(MEMORY)&_pragma=foreign_keys(ON)" +
		// Deleted rows (an account's secrets, a moved token) are zeroed
		// in the file, not left in free pages.
		"&_pragma=secure_delete(ON)" +
		// A transaction that reads and then writes must hold the write lock
		// from the start: under WAL a deferred one that upgrades fails with
		// SQLITE_BUSY at once instead of waiting out busy_timeout.
		"&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	s := &Store{sql: db}
	if err := runMigrations(context.Background(), db, path, migrations); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.sql.Close() }

// Ping checks that the database answers a query.
func (s *Store) Ping(ctx context.Context) error { return s.sql.PingContext(ctx) }

// SchemaVersion returns the newest migration recorded in the database.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	return schemaVersion(ctx, s.sql)
}
