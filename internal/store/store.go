package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

type Store struct{ sql *sql.DB }

func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}
	dsn := "file:" + path +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)" +
		"&_pragma=temp_store(MEMORY)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	s := &Store{sql: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.sql.Close() }

func (s *Store) migrate() error {
	if _, err := s.sql.Exec(schema); err != nil {
		return err
	}
	if err := s.addColumnIfMissing("post_targets", "signed_event_json", "TEXT"); err != nil {
		return err
	}
	if err := s.addColumnIfMissing("posts", "scheduled_at", "TIMESTAMP"); err != nil {
		return err
	}
	if err := s.addColumnIfMissing("post_targets", "segments_json", "TEXT"); err != nil {
		return err
	}
	if err := s.addColumnIfMissing("posts", "hidden", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.addColumnIfMissing("posts", "interaction_json", "TEXT"); err != nil {
		return err
	}
	if err := s.addColumnIfMissing("post_targets", "gave_up_at", "TIMESTAMP"); err != nil {
		return err
	}
	if err := s.addColumnIfMissing("target_relays", "gave_up_at", "TIMESTAMP"); err != nil {
		return err
	}
	return s.addColumnIfMissing("target_relays", "retry_count", "INTEGER NOT NULL DEFAULT 0")
}

// addColumnIfMissing performs an idempotent ALTER TABLE ADD COLUMN (SQLite has
// no ADD COLUMN IF NOT EXISTS; CREATE TABLE IF NOT EXISTS can't add columns to
// an existing table).
func (s *Store) addColumnIfMissing(table, column, typ string) error {
	rows, err := s.sql.Query("PRAGMA table_info(" + table + ")")
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
	_, err = s.sql.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + typ)
	return err
}

const schema = `
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
`
