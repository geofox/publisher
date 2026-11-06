package store

import (
	"context"
	"fmt"
)

// migrations is the ordered, append-only list of schema steps. Open applies
// every entry newer than the database's recorded version, each in its own
// transaction, after writing a <db>.pre-v<N>.bak copy of an existing database.
//
// Rules:
//   - Never edit, renumber or reorder a released migration. Add a new one with
//     the next version.
//   - Versions are dense from 1; names are unique, short and snake_case.
//   - Prefer additive changes. Drops, renames and type changes use SQLite's
//     table-rebuild recipe (https://sqlite.org/lang_altertable.html#otheralter)
//     with noForeignKeys: true.
//   - Data backfills are Go up funcs, in the same migration as their DDL.
//   - After adding one, run `go test ./internal/store -run TestSchemaGolden
//     -update` and commit the golden diff with it.
var migrations = []migration{
	{version: 1, name: "baseline", up: migrateBaseline},
	{version: 2, name: "error_class", up: migrateErrorClass},
	{version: 3, name: "owners_accounts", up: migrateOwnersAccounts, noForeignKeys: true},
	{version: 4, name: "target_items", up: migrateTargetItems, noForeignKeys: true},
	{version: 5, name: "token_scopes_idempotency", up: migrateTokenScopesIdempotency},
	{version: 6, name: "events", up: migrateEvents},
	{version: 7, name: "media_objects", up: migrateMediaObjects},
	{version: 8, name: "drafts_v1", up: migrateDraftsV1},
	{version: 9, name: "draft_breaks", up: migrateDraftBreaks},
	{version: 10, name: "item_send_marks", up: migrateItemSendMarks},
}

// migrateItemSendMarks (v10) gives target_items two marks the engine's
// duplicate guard reads. doubt_reset_at is when the owner last said to send
// the item again (a retry by hand, or "not published"): attempts before it
// no longer count as maybe-posted. committing_at is set just before the
// request that publishes the item, so a restart can tell an item that never
// reached the network from one that may have. An item an older binary left
// sending has no mark but may have been posted: it is marked, so recovery
// leaves it "maybe posted" rather than sending it again.
//
// It also forgets the users' stored emails: before, the login kept an email
// the provider hadn't verified, and a session is re-checked against the
// email allowlist by the stored one. The next login stores it again, if
// verified.
func migrateItemSendMarks(ctx context.Context, tx execer) error {
	_, err := tx.ExecContext(ctx, `
ALTER TABLE target_items ADD COLUMN doubt_reset_at TIMESTAMP;
ALTER TABLE target_items ADD COLUMN committing_at TIMESTAMP;
UPDATE target_items SET committing_at = CURRENT_TIMESTAMP WHERE status = 'sending';
UPDATE users SET email = NULL;`)
	return err
}

// migrateDraftsV1 (v8) gives drafts the /v1 shape: body_json holds the
// draft as a POST /v1/posts body (media named by media object id), version
// counts its writes (its ETag). A draft from before has no body until the
// server converts it at start (api.ConvertOldDrafts): the conversion needs
// the accounts and destinations, which a migration doesn't have. Adopting
// those drafts' files as media objects looks them up by hash.
func migrateDraftsV1(ctx context.Context, tx execer) error {
	_, err := tx.ExecContext(ctx, `
ALTER TABLE drafts ADD COLUMN body_json TEXT;
ALTER TABLE drafts ADD COLUMN version INTEGER NOT NULL DEFAULT 1;
CREATE INDEX idx_drafts_owner_updated ON drafts(owner_id, updated_at, id);
CREATE INDEX idx_media_objects_sha ON media_objects(owner_id, sha256);`)
	return err
}

// migrateMediaObjects (v7) stores the media /v1 uploads, before any post
// uses them: an image is ready at once, a video once processed.
func migrateMediaObjects(ctx context.Context, tx execer) error {
	_, err := tx.ExecContext(ctx, `
CREATE TABLE media_objects (
  id            TEXT PRIMARY KEY,
  owner_id      TEXT NOT NULL REFERENCES owners(id),
  status        TEXT NOT NULL,
  url           TEXT NOT NULL DEFAULT '',
  sha256        TEXT NOT NULL DEFAULT '',
  mime          TEXT NOT NULL DEFAULT '',
  dim           TEXT NOT NULL DEFAULT '',
  blurhash      TEXT NOT NULL DEFAULT '',
  size_bytes    INTEGER NOT NULL DEFAULT 0,
  duration_secs INTEGER NOT NULL DEFAULT 0,
  poster_url    TEXT NOT NULL DEFAULT '',
  alt           TEXT NOT NULL DEFAULT '',
  error         TEXT NOT NULL DEFAULT '',
  job_id        TEXT NOT NULL DEFAULT '',
  created_at    TIMESTAMP NOT NULL,
  updated_at    TIMESTAMP NOT NULL
);
CREATE INDEX idx_media_objects_owner ON media_objects(owner_id, created_at);`)
	return err
}

// migrateEvents (v6) adds the event log behind GET /v1/events: one row per
// state change, written in the transaction that makes the change, so a
// client resuming with Last-Event-ID misses nothing.
func migrateEvents(ctx context.Context, tx execer) error {
	_, err := tx.ExecContext(ctx, `
CREATE TABLE events (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  owner_id   TEXT NOT NULL,
  post_id    TEXT,
  type       TEXT NOT NULL,
  data       TEXT NOT NULL,
  created_at TIMESTAMP NOT NULL
);
CREATE INDEX idx_events_owner ON events(owner_id, id);
CREATE INDEX idx_events_post ON events(post_id, id);
CREATE INDEX idx_events_created ON events(created_at);
ALTER TABLE posts ADD COLUMN canceled_at TIMESTAMP;`)
	return err
}

// migrateTokenScopesIdempotency (v5) gives API tokens scopes and an optional
// expiry, and adds the store behind the /v1 Idempotency-Key header. Existing
// tokens keep what /publish and /upload-media let them do.
func migrateTokenScopesIdempotency(ctx context.Context, tx execer) error {
	_, err := tx.ExecContext(ctx, `
ALTER TABLE api_tokens ADD COLUMN scopes TEXT NOT NULL DEFAULT '';
UPDATE api_tokens SET scopes = 'posts:write media:write';
ALTER TABLE api_tokens ADD COLUMN expires_at TIMESTAMP;
CREATE TABLE idempotency_keys (
  principal        TEXT NOT NULL,
  key              TEXT NOT NULL,
  method           TEXT NOT NULL,
  path             TEXT NOT NULL,
  fingerprint      TEXT NOT NULL,
  state            TEXT NOT NULL,
  response_status  INTEGER,
  response_headers TEXT,
  response_body    BLOB,
  created_at       TIMESTAMP NOT NULL,
  expires_at       TIMESTAMP NOT NULL,
  PRIMARY KEY (principal, key)
);
CREATE INDEX idx_idempotency_keys_expires ON idempotency_keys(expires_at);`)
	return err
}

// migrateErrorClass records each failure's destination.ErrorClass, so the
// retrier can stop repeating attempts that can only fail the same way.
func migrateErrorClass(ctx context.Context, tx execer) error {
	_, err := tx.ExecContext(ctx, `
ALTER TABLE post_targets ADD COLUMN error_class TEXT;
ALTER TABLE target_attempts ADD COLUMN error_class TEXT;`)
	return err
}

// migrateBaseline is the schema as it stood when versioned migrations were
// introduced. Databases created before then are at any point of the column
// history below, so — in this migration only — each column is added only if
// missing. Every later migration is plain run-once DDL.
func migrateBaseline(ctx context.Context, tx execer) error {
	if _, err := tx.ExecContext(ctx, baselineSchema); err != nil {
		return err
	}
	for _, c := range baselineColumns {
		if err := addLegacyColumn(ctx, tx, c.table, c.column, c.typ); err != nil {
			return fmt.Errorf("add %s.%s: %w", c.table, c.column, err)
		}
	}
	return nil
}

// baselineColumns are the columns added to baselineSchema's tables over time,
// in the order they were introduced.
var baselineColumns = []struct{ table, column, typ string }{
	{"post_targets", "signed_event_json", "TEXT"},
	{"posts", "scheduled_at", "TIMESTAMP"},
	{"post_targets", "segments_json", "TEXT"},
	{"posts", "hidden", "INTEGER NOT NULL DEFAULT 0"},
	{"posts", "interaction_json", "TEXT"},
	{"post_targets", "gave_up_at", "TIMESTAMP"},
	{"target_relays", "gave_up_at", "TIMESTAMP"},
	{"target_relays", "retry_count", "INTEGER NOT NULL DEFAULT 0"},
	{"media", "duration_secs", "INTEGER"},
	{"draft_media", "duration_secs", "INTEGER"},
	{"media", "poster_url", "TEXT"},
	{"draft_media", "poster_url", "TEXT"},
	{"draft_media", "client_id", "TEXT"},
}

// addLegacyColumn adds a column unless it already exists (SQLite has no ADD
// COLUMN IF NOT EXISTS). Only the baseline migration may use it.
func addLegacyColumn(ctx context.Context, tx execer, table, column, typ string) error {
	rows, err := tx.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return rows.Err() // already present
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_ = rows.Close()
	_, err = tx.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN "+column+" "+typ)
	return err
}

const baselineSchema = `
CREATE TABLE IF NOT EXISTS posts (
  id            TEXT PRIMARY KEY,
  created_at    TIMESTAMP NOT NULL,
  master_text   TEXT,
  platforms     TEXT NOT NULL,
  delay_seconds INTEGER NOT NULL DEFAULT 0,
  source        TEXT NOT NULL,
  status        TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS post_targets (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  post_id         TEXT NOT NULL REFERENCES posts(id),
  platform        TEXT NOT NULL,
  final_text      TEXT,
  fields_json     TEXT,
  status          TEXT NOT NULL,
  remote_id       TEXT,
  remote_url      TEXT,
  latency_ms      INTEGER,
  attempt_count   INTEGER NOT NULL DEFAULT 0,
  last_attempt_at TIMESTAMP,
  UNIQUE(post_id, platform)
);
CREATE TABLE IF NOT EXISTS target_attempts (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  target_id     INTEGER NOT NULL REFERENCES post_targets(id),
  attempt_no    INTEGER NOT NULL,
  status        TEXT NOT NULL,
  error         TEXT,
  latency_ms    INTEGER,
  remote_id     TEXT,
  request_json  TEXT,
  response_json TEXT,
  attempted_at  TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS media (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  post_id     TEXT NOT NULL REFERENCES posts(id),
  ordinal     INTEGER NOT NULL,
  blossom_url TEXT NOT NULL,
  sha256      TEXT NOT NULL,
  mime        TEXT,
  dim         TEXT,
  blurhash    TEXT,
  size_bytes  INTEGER,
  alt         TEXT
);
CREATE TABLE IF NOT EXISTS target_relays (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  target_id    INTEGER NOT NULL REFERENCES post_targets(id),
  relay_url    TEXT NOT NULL,
  status       TEXT NOT NULL,
  message      TEXT,
  attempted_at TIMESTAMP NOT NULL,
  UNIQUE(target_id, relay_url)
);
CREATE TABLE IF NOT EXISTS sync_relays (
  url      TEXT PRIMARY KEY,
  added_at TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS threads_token (
  id           INTEGER PRIMARY KEY CHECK (id = 1),
  token        TEXT NOT NULL,
  expires_at   TIMESTAMP NOT NULL,
  seed_hash    TEXT NOT NULL,
  refreshed_at TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS drafts (
  id          TEXT PRIMARY KEY,
  created_at  TIMESTAMP NOT NULL,
  updated_at  TIMESTAMP NOT NULL,
  title       TEXT NOT NULL DEFAULT '',
  master_text TEXT NOT NULL DEFAULT '',
  tags_json   TEXT NOT NULL DEFAULT '[]',
  spec_json   TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS draft_media (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  draft_id    TEXT NOT NULL REFERENCES drafts(id) ON DELETE CASCADE,
  ordinal     INTEGER NOT NULL,
  blossom_url TEXT NOT NULL,
  sha256      TEXT NOT NULL,
  mime        TEXT,
  dim         TEXT,
  blurhash    TEXT,
  size_bytes  INTEGER,
  alt         TEXT
);
CREATE INDEX IF NOT EXISTS idx_posts_created ON posts(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_targets_post ON post_targets(post_id);
CREATE INDEX IF NOT EXISTS idx_target_relays_target ON target_relays(target_id);
CREATE INDEX IF NOT EXISTS idx_drafts_updated_at ON drafts(updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_draft_media_draft_id ON draft_media(draft_id);
CREATE TABLE IF NOT EXISTS users (
  id           TEXT PRIMARY KEY,
  subject      TEXT NOT NULL UNIQUE,
  email        TEXT,
  name         TEXT,
  created_at   TIMESTAMP NOT NULL,
  last_seen_at TIMESTAMP
);
CREATE TABLE IF NOT EXISTS sessions (
  id         TEXT PRIMARY KEY,
  user_id    TEXT NOT NULL REFERENCES users(id),
  created_at TIMESTAMP NOT NULL,
  expires_at TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS api_tokens (
  id           TEXT PRIMARY KEY,
  name         TEXT NOT NULL,
  token_hash   TEXT NOT NULL UNIQUE,
  created_at   TIMESTAMP NOT NULL,
  last_used_at TIMESTAMP,
  revoked_at   TIMESTAMP
);
CREATE TABLE IF NOT EXISTS nostr_fanout (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  post_id           TEXT,
  signed_event_json TEXT NOT NULL,
  relay_url         TEXT NOT NULL,
  status            TEXT NOT NULL DEFAULT 'pending',
  retry_count       INTEGER NOT NULL DEFAULT 0,
  next_attempt_at   TIMESTAMP NOT NULL,
  created_at        TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_nostr_fanout_due ON nostr_fanout(status, next_attempt_at);
`
