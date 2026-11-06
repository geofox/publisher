package store

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// v10 on a database an older binary stopped mid-send: the item left sending
// is marked as maybe posted (recovery must not send it again), and stored
// emails, which may not have been verified, are forgotten.
func TestMigrationV10MarksLeftoverSendsAndForgetsEmails(t *testing.T) {
	db, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `
CREATE TABLE target_items (id INTEGER PRIMARY KEY, status TEXT);
CREATE TABLE users (id TEXT PRIMARY KEY, email TEXT);
INSERT INTO target_items VALUES (1, 'sending'), (2, 'published');
INSERT INTO users VALUES ('u1', 'someone@example.com');`); err != nil {
		t.Fatal(err)
	}
	if err := migrateItemSendMarks(ctx, db); err != nil {
		t.Fatal(err)
	}
	var sending, published sql.NullString
	_ = db.QueryRowContext(ctx, `SELECT committing_at FROM target_items WHERE id = 1`).Scan(&sending)
	_ = db.QueryRowContext(ctx, `SELECT committing_at FROM target_items WHERE id = 2`).Scan(&published)
	if !sending.Valid || published.Valid {
		t.Errorf("committing_at: sending=%v published=%v, want only the sending item marked", sending, published)
	}
	var email sql.NullString
	_ = db.QueryRowContext(ctx, `SELECT email FROM users WHERE id = 'u1'`).Scan(&email)
	if email.Valid {
		t.Errorf("email kept: %q", email.String)
	}
}
