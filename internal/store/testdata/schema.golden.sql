-- index idx_draft_media_draft_id
CREATE INDEX idx_draft_media_draft_id ON draft_media(draft_id);

-- index idx_drafts_updated_at
CREATE INDEX idx_drafts_updated_at ON drafts(updated_at DESC);

-- index idx_nostr_fanout_due
CREATE INDEX idx_nostr_fanout_due ON nostr_fanout(status, next_attempt_at);

-- index idx_posts_created
CREATE INDEX idx_posts_created ON posts(created_at DESC);

-- index idx_target_relays_target
CREATE INDEX idx_target_relays_target ON target_relays(target_id);

-- index idx_targets_post
CREATE INDEX idx_targets_post ON post_targets(post_id);

-- table api_tokens
CREATE TABLE api_tokens (
  id           TEXT PRIMARY KEY,
  name         TEXT NOT NULL,
  token_hash   TEXT NOT NULL UNIQUE,
  created_at   TIMESTAMP NOT NULL,
  last_used_at TIMESTAMP,
  revoked_at   TIMESTAMP
);

-- table draft_media
CREATE TABLE draft_media (
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
, duration_secs INTEGER, poster_url TEXT, client_id TEXT);

-- table drafts
CREATE TABLE drafts (
  id          TEXT PRIMARY KEY,
  created_at  TIMESTAMP NOT NULL,
  updated_at  TIMESTAMP NOT NULL,
  title       TEXT NOT NULL DEFAULT '',
  master_text TEXT NOT NULL DEFAULT '',
  tags_json   TEXT NOT NULL DEFAULT '[]',
  spec_json   TEXT NOT NULL
);

-- table media
CREATE TABLE media (
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
, duration_secs INTEGER, poster_url TEXT);

-- table nostr_fanout
CREATE TABLE nostr_fanout (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  post_id           TEXT,
  signed_event_json TEXT NOT NULL,
  relay_url         TEXT NOT NULL,
  status            TEXT NOT NULL DEFAULT 'pending',
  retry_count       INTEGER NOT NULL DEFAULT 0,
  next_attempt_at   TIMESTAMP NOT NULL,
  created_at        TIMESTAMP NOT NULL
);

-- table post_targets
CREATE TABLE post_targets (
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
  last_attempt_at TIMESTAMP, signed_event_json TEXT, segments_json TEXT, gave_up_at TIMESTAMP,
  UNIQUE(post_id, platform)
);

-- table posts
CREATE TABLE posts (
  id            TEXT PRIMARY KEY,
  created_at    TIMESTAMP NOT NULL,
  master_text   TEXT,
  platforms     TEXT NOT NULL,
  delay_seconds INTEGER NOT NULL DEFAULT 0,
  source        TEXT NOT NULL,
  status        TEXT NOT NULL
, scheduled_at TIMESTAMP, hidden INTEGER NOT NULL DEFAULT 0, interaction_json TEXT);

-- table schema_migrations
CREATE TABLE schema_migrations (
  version    INTEGER PRIMARY KEY,
  name       TEXT NOT NULL,
  applied_at TIMESTAMP NOT NULL
);

-- table sessions
CREATE TABLE sessions (
  id         TEXT PRIMARY KEY,
  user_id    TEXT NOT NULL REFERENCES users(id),
  created_at TIMESTAMP NOT NULL,
  expires_at TIMESTAMP NOT NULL
);

-- table sync_relays
CREATE TABLE sync_relays (
  url      TEXT PRIMARY KEY,
  added_at TIMESTAMP NOT NULL
);

-- table target_attempts
CREATE TABLE target_attempts (
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

-- table target_relays
CREATE TABLE target_relays (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  target_id    INTEGER NOT NULL REFERENCES post_targets(id),
  relay_url    TEXT NOT NULL,
  status       TEXT NOT NULL,
  message      TEXT,
  attempted_at TIMESTAMP NOT NULL, gave_up_at TIMESTAMP, retry_count INTEGER NOT NULL DEFAULT 0,
  UNIQUE(target_id, relay_url)
);

-- table threads_token
CREATE TABLE threads_token (
  id           INTEGER PRIMARY KEY CHECK (id = 1),
  token        TEXT NOT NULL,
  expires_at   TIMESTAMP NOT NULL,
  seed_hash    TEXT NOT NULL,
  refreshed_at TIMESTAMP NOT NULL
);

-- table users
CREATE TABLE users (
  id           TEXT PRIMARY KEY,
  subject      TEXT NOT NULL UNIQUE,
  email        TEXT,
  name         TEXT,
  created_at   TIMESTAMP NOT NULL,
  last_seen_at TIMESTAMP
);

