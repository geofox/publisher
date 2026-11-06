package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// v11 on a v10 database: the account columns arrive with their defaults,
// every env account is enabled (enabled = 0 only ever meant "not
// configured"), the per-account sync relay table exists, and fan-out jobs
// queued without an item id get it back.
func TestMigrationV11FromV10(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v10.db")
	db, err := openWith(t, path, migrations[:10])
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	mustExec(t, db, `INSERT INTO accounts (id, owner_id, type, label, source, env_key, enabled, created_at, updated_at) VALUES
		('acc_on', 'own_default', 'mastodon', 'Mastodon', 'env', 'default', 1, ?, ?),
		('acc_off', 'own_default', 'bluesky', 'Bluesky', 'env', 'default', 0, ?, ?)`, now, now, now, now)
	mustExec(t, db, `INSERT INTO posts (id, created_at, master_text, platforms, source, status) VALUES ('p1', ?, 'gm', '["nostr"]', 'ui', 'partial')`, now)
	mustExec(t, db, `INSERT INTO post_targets (id, post_id, account_id, platform, status) VALUES (1, 'p1', 'acc_on', 'nostr', 'partial')`)
	mustExec(t, db, `INSERT INTO target_items (id, target_id, ordinal, content_json, status, idem_key, ref_json) VALUES
		(7, 1, 0, '{}', 'published', 'k0', '{"id":"ev0"}'),
		(8, 1, 1, '{}', 'published', 'k1', '{"id":"ev1"}')`)
	mustExec(t, db, `INSERT INTO nostr_fanout (post_id, signed_event_json, relay_url, next_attempt_at, created_at, item_id) VALUES
		('p1', '{"id":"ev1"}', 'wss://a', ?, ?, NULL),
		('p1', '{"id":"ev0"}', 'wss://b', ?, ?, 7),
		('p1', '{"id":"other"}', 'wss://c', ?, ?, NULL)`, now, now, now, now, now, now)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	for _, id := range []string{"acc_on", "acc_off"} {
		var enabled, defSel, feed int
		var remote sql.NullString
		if err := s.sql.QueryRowContext(ctx, `SELECT enabled, default_selected, public_feed, remote_id FROM accounts WHERE id = ?`, id).
			Scan(&enabled, &defSel, &feed, &remote); err != nil {
			t.Fatal(err)
		}
		if enabled != 1 || defSel != 1 || feed != 1 || remote.Valid {
			t.Errorf("%s: enabled %d default_selected %d public_feed %d remote_id %v", id, enabled, defSel, feed, remote)
		}
	}
	got := map[string]sql.NullInt64{}
	rows, err := s.sql.QueryContext(ctx, `SELECT relay_url, item_id FROM nostr_fanout`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var relay string
		var item sql.NullInt64
		if err := rows.Scan(&relay, &item); err != nil {
			t.Fatal(err)
		}
		got[relay] = item
	}
	rows.Close()
	if got["wss://a"].Int64 != 8 || got["wss://b"].Int64 != 7 || got["wss://c"].Valid {
		t.Errorf("fan-out item ids = %v, want a→8, b kept 7, c none", got)
	}
	if _, err := s.sql.ExecContext(ctx, `INSERT INTO nostr_sync_relays (account_id, url, added_at) VALUES ('acc_on', 'wss://r', ?)`, now); err != nil {
		t.Errorf("nostr_sync_relays: %v", err)
	}
}
