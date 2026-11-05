package store

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var updateGolden = flag.Bool("update", false, "rewrite testdata golden files")

// rawDB opens a database file without running migrations, with the same
// pragmas the store uses.
func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// dumpSchema returns the database's schema (sqlite_master) in a stable order,
// excluding SQLite's internal tables.
func dumpSchema(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.Query(`SELECT type, name, COALESCE(sql, '') FROM sqlite_master
		WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var typ, name, stmt string
		if err := rows.Scan(&typ, &name, &stmt); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "-- %s %s\n%s;\n\n", typ, name, stmt)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func columns(t *testing.T, db *sql.DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		out[name] = true
	}
	return out
}

func appliedVersions(t *testing.T, db *sql.DB) []int {
	t.Helper()
	rows, err := db.Query(`SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var vs []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		vs = append(vs, v)
	}
	return vs
}

func TestMigrationsListIsWellFormed(t *testing.T) {
	if err := validateMigrations(migrations); err != nil {
		t.Fatal(err)
	}
	if LatestSchemaVersion() != len(migrations) {
		t.Fatalf("LatestSchemaVersion = %d, want %d", LatestSchemaVersion(), len(migrations))
	}
}

func TestValidateMigrationsRejectsBadLists(t *testing.T) {
	noop := func(context.Context, execer) error { return nil }
	cases := map[string][]migration{
		"gap":       {{version: 1, name: "a", up: noop}, {version: 3, name: "b", up: noop}},
		"not from1": {{version: 2, name: "a", up: noop}},
		"dup name":  {{version: 1, name: "a", up: noop}, {version: 2, name: "a", up: noop}},
		"no name":   {{version: 1, up: noop}},
		"no up":     {{version: 1, name: "a"}},
	}
	for name, ms := range cases {
		if err := validateMigrations(ms); err == nil {
			t.Errorf("%s: validateMigrations accepted an invalid list", name)
		}
	}
}

// TestSchemaGolden pins the schema a fresh install gets. Run with -update after
// adding a migration, and review the golden diff.
func TestSchemaGolden(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got := dumpSchema(t, s.sql)
	_ = s.Close()

	golden := filepath.Join("testdata", "schema.golden.sql")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run with -update to create it): %v", err)
	}
	if got != string(want) {
		t.Fatalf("schema differs from %s; if intended, rerun with -update and review the diff.\n--- got ---\n%s", golden, got)
	}
}

func TestOpenFreshDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	v, err := s.SchemaVersion(context.Background())
	if err != nil || v != LatestSchemaVersion() {
		t.Fatalf("SchemaVersion = %d, %v; want %d", v, err, LatestSchemaVersion())
	}
	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if _, err := os.Stat(path + ".pre-v1.bak"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fresh database must not be backed up; stat err = %v", err)
	}
}

// legacyDB builds a database the way builds before versioned migrations left it:
// the baseline CREATE blob plus only the first `upTo` historical columns, with a
// post, a target and a draft in it.
func legacyDB(t *testing.T, path string, upTo int) {
	t.Helper()
	db := rawDB(t, path)
	if _, err := db.Exec(baselineSchema); err != nil {
		t.Fatal(err)
	}
	for _, c := range baselineColumns[:upTo] {
		if _, err := db.Exec("ALTER TABLE " + c.table + " ADD COLUMN " + c.column + " " + c.typ); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	mustExec(t, db, `INSERT INTO posts (id, created_at, master_text, platforms, source, status)
		VALUES ('p1', ?, 'hello legacy', '["nostr"]', 'ui', 'success')`, now)
	mustExec(t, db, `INSERT INTO post_targets (post_id, platform, final_text, status)
		VALUES ('p1', 'nostr', 'hello legacy', 'success')`)
	mustExec(t, db, `INSERT INTO drafts (id, created_at, updated_at, spec_json)
		VALUES ('d1', ?, ?, '{}')`, now, now)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// movedByV4 are baseline columns migration 4 moved into target_items and
// item_deliveries.
var movedByV4 = map[string]bool{
	"post_targets.signed_event_json": true, "post_targets.segments_json": true,
	"target_relays.gave_up_at": true, "target_relays.retry_count": true,
}

func TestOpenUpgradesLegacyDatabase(t *testing.T) {
	for _, upTo := range []int{0, 5, len(baselineColumns)} {
		t.Run(fmt.Sprintf("columns=%d", upTo), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "t.db")
			legacyDB(t, path, upTo)

			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()

			for _, c := range baselineColumns {
				if movedByV4[c.table+"."+c.column] {
					continue
				}
				if !columns(t, s.sql, c.table)[c.column] {
					t.Errorf("column %s.%s missing after upgrade", c.table, c.column)
				}
			}
			var text string
			var hidden int
			if err := s.sql.QueryRow(`SELECT master_text, hidden FROM posts WHERE id = 'p1'`).Scan(&text, &hidden); err != nil {
				t.Fatal(err)
			}
			if text != "hello legacy" || hidden != 0 {
				t.Errorf("legacy row = (%q, %d), want (\"hello legacy\", 0)", text, hidden)
			}
			if got := appliedVersions(t, s.sql); len(got) != LatestSchemaVersion() {
				t.Errorf("applied versions = %v", got)
			}

			// The upgraded schema must be exactly what a fresh install gets.
			freshPath := filepath.Join(t.TempDir(), "fresh.db")
			fresh, err := Open(freshPath)
			if err != nil {
				t.Fatal(err)
			}
			defer fresh.Close()
			if a, b := dumpSchema(t, s.sql), dumpSchema(t, fresh.sql); upTo == len(baselineColumns) && a != b {
				t.Errorf("upgraded schema differs from fresh schema:\n%s\n---\n%s", a, b)
			}

			// A backup of the pre-migration database sits next to it and is usable.
			bak := rawDB(t, path+".pre-v1.bak")
			var n int
			if err := bak.QueryRow(`SELECT COUNT(*) FROM posts`).Scan(&n); err != nil || n != 1 {
				t.Fatalf("backup posts = %d, %v; want 1", n, err)
			}
			if cols := columns(t, bak, "posts"); cols["hidden"] != (upTo > 3) {
				t.Errorf("backup should hold the pre-migration schema; posts.hidden present = %v", cols["hidden"])
			}
		})
	}
}

func TestReopenIsNoOp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := appliedVersions(t, s.sql); len(got) != LatestSchemaVersion() {
		t.Fatalf("applied versions after reopen = %v", got)
	}
	matches, _ := filepath.Glob(path + ".pre-v*.bak")
	if len(matches) != 0 {
		t.Fatalf("reopen of an up-to-date database wrote backups: %v", matches)
	}
}

func TestOpenRefusesNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, s.sql, `INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, 'from_the_future', ?)`,
		LatestSchemaVersion()+1, time.Now().UTC())
	_ = s.Close()

	_, err = Open(path)
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("Open = %v, want ErrSchemaTooNew", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("v%d", LatestSchemaVersion()+1)) {
		t.Errorf("error should name the database version: %v", err)
	}
}

// openWith runs an arbitrary migration list against a raw database.
func openWith(t *testing.T, path string, ms []migration) (*sql.DB, error) {
	t.Helper()
	db := rawDB(t, path)
	return db, runMigrations(context.Background(), db, path, ms)
}

func TestFailedMigrationRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	ms := []migration{
		sqlMigration(1, "one", `CREATE TABLE a (id INTEGER PRIMARY KEY)`),
		{version: 2, name: "two_breaks", up: func(ctx context.Context, tx execer) error {
			if _, err := tx.ExecContext(ctx, `CREATE TABLE b (id INTEGER PRIMARY KEY)`); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO nope VALUES (1)`)
			return err
		}},
	}
	db, err := openWith(t, path, ms)
	if err == nil || !strings.Contains(err.Error(), "v2 two_breaks") {
		t.Fatalf("err = %v, want it to name v2 two_breaks", err)
	}
	if got := appliedVersions(t, db); len(got) != 1 || got[0] != 1 {
		t.Fatalf("applied = %v, want [1]", got)
	}
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'b'`).Scan(&n)
	if n != 0 {
		t.Fatal("table b from the failed migration survived the rollback")
	}

	// Fixing the migration and reopening applies only v2 (with a v2 backup).
	ms[1] = sqlMigration(2, "two_breaks", `CREATE TABLE b (id INTEGER PRIMARY KEY)`)
	if err := runMigrations(context.Background(), db, path, ms); err != nil {
		t.Fatal(err)
	}
	if got := appliedVersions(t, db); len(got) != 2 {
		t.Fatalf("applied = %v, want [1 2]", got)
	}
	if _, err := os.Stat(path + ".pre-v2.bak"); err != nil {
		t.Fatalf("expected pre-v2 backup: %v", err)
	}
}

func TestTableRebuildMigration(t *testing.T) {
	base := []migration{
		sqlMigration(1, "tables", `
			CREATE TABLE parent (id INTEGER PRIMARY KEY, label TEXT);
			CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER NOT NULL REFERENCES parent(id));
			INSERT INTO parent VALUES (1, 'x');
			INSERT INTO child VALUES (10, 1);`),
	}
	rebuild := func(dropParentRow bool) migration {
		extra := ""
		if dropParentRow {
			extra = "DELETE FROM parent_new WHERE id = 1;"
		}
		return migration{version: 2, name: "rebuild_parent", noForeignKeys: true,
			up: func(ctx context.Context, tx execer) error {
				_, err := tx.ExecContext(ctx, `
					CREATE TABLE parent_new (id INTEGER PRIMARY KEY, label TEXT NOT NULL DEFAULT '');
					INSERT INTO parent_new (id, label) SELECT id, COALESCE(label, '') FROM parent;
					`+extra+`
					DROP TABLE parent;
					ALTER TABLE parent_new RENAME TO parent;`)
				return err
			}}
	}

	t.Run("ok", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "t.db")
		db, err := openWith(t, path, append(base, rebuild(false)))
		if err != nil {
			t.Fatal(err)
		}
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM child JOIN parent ON parent.id = child.parent_id`).Scan(&n); err != nil || n != 1 {
			t.Fatalf("join after rebuild = %d, %v", n, err)
		}
		// Foreign keys are back on for the connection pool afterwards.
		if _, err := db.Exec(`INSERT INTO child VALUES (11, 999)`); err == nil {
			t.Fatal("foreign keys not enforced after the migration")
		}
	})
	t.Run("violation rolls back", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "t.db")
		db, err := openWith(t, path, append(base, rebuild(true)))
		if err == nil || !strings.Contains(err.Error(), "foreign key violation") {
			t.Fatalf("err = %v, want a foreign key violation", err)
		}
		if got := appliedVersions(t, db); len(got) != 1 {
			t.Fatalf("applied = %v, want [1]", got)
		}
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM parent`).Scan(&n)
		if n != 1 {
			t.Fatalf("parent rows = %d after rollback, want 1", n)
		}
	})
}

func TestConcurrentOpenAppliesOnce(t *testing.T) {
	// Two processes racing on one database file (e.g. an overlapping container
	// restart). The file already exists in WAL mode, as any install's does:
	// switching a brand-new file into WAL is a connection-setup race inside
	// SQLite itself, outside what the migration runner controls.
	path := filepath.Join(t.TempDir(), "t.db")
	legacyDB(t, path, len(baselineColumns))
	var wg sync.WaitGroup
	errs := make([]error, 4)
	stores := make([]*Store, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			stores[i], errs[i] = Open(path)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Open #%d: %v", i, err)
		}
		defer stores[i].Close()
	}
	if got := appliedVersions(t, stores[0].sql); len(got) != LatestSchemaVersion() {
		t.Fatalf("applied = %v", got)
	}
}
