-- index idx_draft_media_draft_id
CREATE INDEX idx_draft_media_draft_id ON draft_media(draft_id);

-- index idx_drafts_updated_at
CREATE INDEX idx_drafts_updated_at ON drafts(updated_at DESC);

-- index idx_events_created
CREATE INDEX idx_events_created ON events(created_at);

-- index idx_events_owner
CREATE INDEX idx_events_owner ON events(owner_id, id);

-- index idx_events_post
CREATE INDEX idx_events_post ON events(post_id, id);

-- index idx_idempotency_keys_expires
CREATE INDEX idx_idempotency_keys_expires ON idempotency_keys(expires_at);

-- index idx_item_deliveries_item
CREATE INDEX idx_item_deliveries_item ON item_deliveries(item_id);

-- index idx_media_objects_owner
CREATE INDEX idx_media_objects_owner ON media_objects(owner_id, created_at);

-- index idx_nostr_fanout_due
CREATE INDEX idx_nostr_fanout_due ON nostr_fanout(status, next_attempt_at);

-- index idx_posts_created
CREATE INDEX idx_posts_created ON posts(created_at DESC);

-- index idx_target_attempts_target
CREATE INDEX idx_target_attempts_target ON target_attempts(target_id);

-- index idx_target_items_due
CREATE INDEX idx_target_items_due ON target_items(status, next_attempt_at);

-- index idx_targets_post
CREATE INDEX idx_targets_post ON post_targets(post_id);

-- table accounts
CREATE TABLE accounts (
  id          TEXT PRIMARY KEY,
  owner_id    TEXT NOT NULL REFERENCES owners(id),
  type        TEXT NOT NULL,
  label       TEXT NOT NULL,
  handle      TEXT,
  source      TEXT NOT NULL,
  env_key     TEXT,
  config_json TEXT NOT NULL DEFAULT '{}',
  enabled     INTEGER NOT NULL DEFAULT 1,
  created_at  TIMESTAMP NOT NULL,
  updated_at  TIMESTAMP NOT NULL,
  UNIQUE(owner_id, type, env_key)
);

-- table api_tokens
CREATE TABLE api_tokens (
  id           TEXT PRIMARY KEY,
  name         TEXT NOT NULL,
  token_hash   TEXT NOT NULL UNIQUE,
  created_at   TIMESTAMP NOT NULL,
  last_used_at TIMESTAMP,
  revoked_at   TIMESTAMP
, owner_id TEXT NOT NULL DEFAULT 'own_default' REFERENCES owners(id), scopes TEXT NOT NULL DEFAULT '', expires_at TIMESTAMP);

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
, owner_id TEXT NOT NULL DEFAULT 'own_default' REFERENCES owners(id));

-- table events
CREATE TABLE events (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  owner_id   TEXT NOT NULL,
  post_id    TEXT,
  type       TEXT NOT NULL,
  data       TEXT NOT NULL,
  created_at TIMESTAMP NOT NULL
);

-- table idempotency_keys
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

-- table item_deliveries
CREATE TABLE item_deliveries (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  item_id      INTEGER NOT NULL REFERENCES target_items(id),
  endpoint     TEXT NOT NULL,
  status       TEXT NOT NULL,
  message      TEXT,
  attempted_at TIMESTAMP NOT NULL,
  retry_count  INTEGER NOT NULL DEFAULT 0,
  gave_up_at   TIMESTAMP,
  UNIQUE(item_id, endpoint)
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

-- table media_objects
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
, item_id INTEGER REFERENCES target_items(id));

-- table owners
CREATE TABLE owners (
  id         TEXT PRIMARY KEY,
  created_at TIMESTAMP NOT NULL
);

-- table post_targets
CREATE TABLE "post_targets" (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  post_id         TEXT NOT NULL REFERENCES posts(id),
  account_id      TEXT NOT NULL REFERENCES accounts(id),
  platform        TEXT NOT NULL,
  final_text      TEXT,
  fields_json     TEXT,
  status          TEXT NOT NULL,
  error_class     TEXT,
  latency_ms      INTEGER,
  attempt_count   INTEGER NOT NULL DEFAULT 0,
  last_attempt_at TIMESTAMP,
  gave_up_at      TIMESTAMP,
  UNIQUE(post_id, account_id)
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
, scheduled_at TIMESTAMP, hidden INTEGER NOT NULL DEFAULT 0, interaction_json TEXT, owner_id TEXT NOT NULL DEFAULT 'own_default' REFERENCES owners(id), canceled_at TIMESTAMP);

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
, error_class TEXT, item_id INTEGER REFERENCES target_items(id));

-- table target_items
CREATE TABLE target_items (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  target_id       INTEGER NOT NULL REFERENCES post_targets(id),
  ordinal         INTEGER NOT NULL,
  content_json    TEXT NOT NULL,
  status          TEXT NOT NULL,
  idem_key        TEXT NOT NULL,
  remote_id       TEXT,
  remote_url      TEXT,
  ref_json        TEXT,
  error_class     TEXT,
  error           TEXT,
  attempt_count   INTEGER NOT NULL DEFAULT 0,
  next_attempt_at TIMESTAMP,
  published_at    TIMESTAMP,
  UNIQUE(target_id, ordinal)
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
, owner_id TEXT NOT NULL DEFAULT 'own_default' REFERENCES owners(id));

