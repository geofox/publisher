package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

// migration is one forward-only schema step. Released migrations are never
// edited or reordered; a change is always a new migration with the next
// version. See migrations.go for the list and the rules.
type migration struct {
	version int
	name    string
	// up runs inside the migration's own transaction (SQLite DDL is
	// transactional, so a failure leaves no half-applied schema). It must not
	// BEGIN, COMMIT or ROLLBACK itself.
	up func(ctx context.Context, tx execer) error
	// noForeignKeys runs the migration with PRAGMA foreign_keys=OFF, as
	// SQLite's table-rebuild recipe requires. PRAGMA foreign_key_check must
	// come back empty before the transaction commits.
	noForeignKeys bool
}

// sqlMigration is a migration whose body is plain SQL.
func sqlMigration(version int, name, stmts string) migration {
	return migration{version: version, name: name, up: func(ctx context.Context, tx execer) error {
		_, err := tx.ExecContext(ctx, stmts)
		return err
	}}
}

// ErrSchemaTooNew is returned by Open when the database was migrated by a newer
// build than this one. Starting anyway would write rows a newer schema does not
// expect, so the store refuses.
var ErrSchemaTooNew = errors.New("database schema is newer than this build supports")

// LatestSchemaVersion is the schema version this build migrates databases to.
func LatestSchemaVersion() int { return latestVersion(migrations) }

func latestVersion(ms []migration) int {
	if len(ms) == 0 {
		return 0
	}
	return ms[len(ms)-1].version
}

const createHistoryTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
  version    INTEGER PRIMARY KEY,
  name       TEXT NOT NULL,
  applied_at TIMESTAMP NOT NULL
)`

// runMigrations brings the database at dbPath up to the newest migration in ms.
// dbPath is used only to place the pre-migration backup ("" or ":memory:"
// skips it).
func runMigrations(ctx context.Context, db *sql.DB, dbPath string, ms []migration) error {
	if err := validateMigrations(ms); err != nil {
		return err
	}
	// One dedicated connection: PRAGMAs are per connection, and the pool must not
	// hand a different connection to the middle of a migration.
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, createHistoryTable); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	// Another process may migrate the same file concurrently; if it moves the
	// version while we take the backup, start over from the new version.
	for attempt := 0; ; attempt++ {
		err := migrateOnce(ctx, conn, dbPath, ms)
		// Each retry means another process applied at least one migration,
		// so there can be at most len(ms) of them.
		if errors.Is(err, errVersionMoved) && attempt < len(ms) {
			continue
		}
		return err
	}
}

// errVersionMoved means the schema version changed under us (another process
// migrated the database) while the pre-migration backup was being written.
var errVersionMoved = errors.New("schema version changed during backup")

func migrateOnce(ctx context.Context, conn *sql.Conn, dbPath string, ms []migration) error {
	current, err := schemaVersion(ctx, conn)
	if err != nil {
		return err
	}
	latest := latestVersion(ms)
	if current > latest {
		return fmt.Errorf("%w: database is at schema v%d, this build knows up to v%d — run a newer image or restore a backup", ErrSchemaTooNew, current, latest)
	}
	var pending []migration
	for _, m := range ms {
		if m.version > current {
			pending = append(pending, m)
		}
	}
	if len(pending) == 0 {
		return nil
	}

	hasData, err := holdsData(ctx, conn, current)
	if err != nil {
		return err
	}
	if hasData {
		if err := backupBeforeMigrate(ctx, conn, dbPath, current); err != nil {
			return err
		}
	}
	// A migration that records the database's birth (v12) learns it here:
	// created now (latest), or the version it had (0 for a database from
	// before versioned migrations).
	born := current
	if !hasData {
		born = latest
	}
	ctx = context.WithValue(ctx, bornKey{}, born)
	for _, m := range pending {
		if err := applyMigration(ctx, conn, m); err != nil {
			return fmt.Errorf("migration v%d %s: %w", m.version, m.name, err)
		}
	}
	return nil
}

func validateMigrations(ms []migration) error {
	seen := map[string]bool{}
	for i, m := range ms {
		if m.version != i+1 {
			return fmt.Errorf("migrations: entry %d has version %d, want %d (versions must be dense from 1, in order)", i, m.version, i+1)
		}
		if m.name == "" || seen[m.name] {
			return fmt.Errorf("migrations: v%d has an empty or duplicate name %q", m.version, m.name)
		}
		if m.up == nil {
			return fmt.Errorf("migrations: v%d %s has no up func", m.version, m.name)
		}
		seen[m.name] = true
	}
	return nil
}

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// execer is what a migration's up func gets: the migration connection, already
// inside the migration's transaction.
type execer interface {
	queryer
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func schemaVersion(ctx context.Context, q queryer) (int, error) {
	var v sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return int(v.Int64), nil
}

// holdsData reports whether the database already has content worth backing up:
// a recorded schema version, or tables created before versioned migrations
// existed.
func holdsData(ctx context.Context, q queryer, current int) (bool, error) {
	if current > 0 {
		return true, nil
	}
	var n int
	err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name != 'schema_migrations'`).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("inspect schema: %w", err)
	}
	return n > 0, nil
}

// backupBeforeMigrate writes a consistent copy of the database next to it
// (<db>.pre-v<N+1>.bak) before any pending migration runs. A failed backup
// aborts the upgrade: a disk that cannot hold the copy likely cannot hold the
// migration either, and an upgrade without a way back is the riskier outcome.
func backupBeforeMigrate(ctx context.Context, conn *sql.Conn, dbPath string, current int) error {
	if dbPath == "" || dbPath == ":memory:" || strings.HasPrefix(dbPath, "file::memory:") {
		return nil
	}
	dst := fmt.Sprintf("%s.pre-v%d.bak", dbPath, current+1)
	// Write to a private temp name and rename into place, so a concurrent
	// starter never sees (or clobbers) a half-written backup.
	tmp := fmt.Sprintf("%s.tmp-%d-%d", dst, os.Getpid(), time.Now().UnixNano())
	start := time.Now()
	if _, err := conn.ExecContext(ctx, `VACUUM INTO ?`, tmp); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("pre-migration backup to %s: %w", dst, err)
	}
	// VACUUM INTO reads one consistent snapshot. If the version is unchanged
	// now, that snapshot predates any concurrent migration and is a true
	// pre-migration copy; otherwise discard it and let the caller start over.
	now, err := schemaVersion(ctx, conn)
	if err != nil || now != current {
		_ = os.Remove(tmp)
		if err != nil {
			return err
		}
		return errVersionMoved
	}
	// The copy holds what the database holds, credentials included.
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("pre-migration backup to %s: %w", dst, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("pre-migration backup to %s: %w", dst, err)
	}
	slog.Info("database backed up before migrating", "path", dst, "from_version", current,
		"duration_ms", time.Since(start).Milliseconds())
	return nil
}

func applyMigration(ctx context.Context, conn *sql.Conn, m migration) (err error) {
	if m.noForeignKeys {
		// Must be set outside a transaction (it is a no-op inside one).
		if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
			return err
		}
		defer func() {
			if _, ferr := conn.ExecContext(context.Background(), `PRAGMA foreign_keys = ON`); ferr != nil && err == nil {
				err = fmt.Errorf("re-enable foreign keys: %w", ferr)
			}
		}()
	}
	start := time.Now()
	// BEGIN IMMEDIATE takes SQLite's write lock up front, so two processes
	// starting on the same file serialize here instead of racing. database/sql's
	// BeginTx cannot express IMMEDIATE, so the transaction is driven by hand on
	// the dedicated connection and wrapped for the up func below.
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()

	// Another process may have applied this migration while we waited for the lock.
	var done int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, m.version).Scan(&done); err != nil {
		return err
	}
	if done > 0 {
		_, err := conn.ExecContext(ctx, `COMMIT`)
		committed = err == nil
		return err
	}

	var fkBefore map[string]int
	if m.noForeignKeys {
		if fkBefore, err = foreignKeyViolations(ctx, conn); err != nil {
			return err
		}
	}
	if err := m.up(ctx, conn); err != nil {
		return err
	}
	if m.noForeignKeys {
		if err := checkForeignKeys(ctx, conn, fkBefore); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
		m.version, m.name, time.Now().UTC()); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	slog.Info("applied database migration", "version", m.version, "name", m.name,
		"duration_ms", time.Since(start).Milliseconds())
	return nil
}

// foreignKeyViolations counts the database's foreign key violations by
// child and parent table.
func foreignKeyViolations(ctx context.Context, conn *sql.Conn) (map[string]int, error) {
	rows, err := conn.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var table, parent string
		var rowid sql.NullInt64
		var fkid int
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return nil, fmt.Errorf("foreign key check: %w", err)
		}
		out[table+" references missing "+parent]++
	}
	return out, rows.Err()
}

// checkForeignKeys fails a migration that left a foreign key violation it
// did not find. Violations already in the database (rows an older version
// orphaned) are logged and left: failing on them would stop every start
// over rows the migration never touched.
func checkForeignKeys(ctx context.Context, conn *sql.Conn, before map[string]int) error {
	after, err := foreignKeyViolations(ctx, conn)
	if err != nil {
		return err
	}
	for k, n := range after {
		if n > before[k] {
			return fmt.Errorf("foreign key violation: %s", k)
		}
		slog.Warn("database has orphaned rows from before this upgrade", "violation", k, "rows", n)
	}
	return nil
}
