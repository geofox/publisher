package store

import (
	"context"
	"strconv"
)

// bornKey carries, into a migration, the schema version the database had
// when this start began migrating it: latest for a database created now,
// 0 for one from before versioned migrations, else its version.
type bornKey struct{}

func bornVersion(ctx context.Context) (int, bool) {
	v, ok := ctx.Value(bornKey{}).(int)
	return v, ok
}

// migrateLocalAuth (v12) adds local sign-in (passkeys, an optional
// password, the first-run setup token, re-authentication nonces) and the
// instance's birth record.
//
// users.source tells a local owner ('local') from an OIDC identity
// ('oidc'): each mode only honours its own sessions. Sessions gain a
// public id (the API names a session by it, never by its hash), when they
// were last used, and how they were started. instance_meta records
// created_schema, the schema version the database had before this
// build first migrated it, which picks AUTH_MODE's default: a database
// created by this build defaults to local sign-in, an older one keeps
// running as before (proxy).
func migrateLocalAuth(ctx context.Context, tx execer) error {
	if _, err := tx.ExecContext(ctx, `
ALTER TABLE users ADD COLUMN source TEXT NOT NULL DEFAULT 'oidc';
ALTER TABLE sessions ADD COLUMN public_id TEXT;
ALTER TABLE sessions ADD COLUMN last_seen_at TIMESTAMP;
ALTER TABLE sessions ADD COLUMN method TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN user_agent TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN addr TEXT NOT NULL DEFAULT '';
UPDATE sessions SET public_id = lower(hex(randomblob(12))), method = 'oidc';
CREATE UNIQUE INDEX idx_sessions_public_id ON sessions(public_id);
CREATE INDEX idx_sessions_user ON sessions(user_id);
CREATE TABLE webauthn_credentials (
  id               TEXT PRIMARY KEY,
  user_id          TEXT NOT NULL REFERENCES users(id),
  rp_id            TEXT NOT NULL,
  credential_id    BLOB NOT NULL UNIQUE,
  public_key       BLOB NOT NULL,
  sign_count       INTEGER NOT NULL DEFAULT 0,
  flags            INTEGER NOT NULL DEFAULT 0,
  transports       TEXT NOT NULL DEFAULT '',
  aaguid           BLOB,
  attestation_type TEXT NOT NULL DEFAULT '',
  name             TEXT NOT NULL,
  created_at       TIMESTAMP NOT NULL,
  last_used_at     TIMESTAMP
);
CREATE INDEX idx_webauthn_credentials_user ON webauthn_credentials(user_id);
CREATE TABLE password_credentials (
  user_id    TEXT PRIMARY KEY REFERENCES users(id),
  phc        TEXT NOT NULL,
  updated_at TIMESTAMP NOT NULL
);
CREATE TABLE login_failures (
  addr TEXT NOT NULL,
  at   TIMESTAMP NOT NULL
);
CREATE INDEX idx_login_failures_addr_at ON login_failures(addr, at);
CREATE INDEX idx_login_failures_at ON login_failures(at);
CREATE TABLE reauth_nonces (
  nonce_hash TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  operation  TEXT NOT NULL,
  target     TEXT NOT NULL DEFAULT '',
  expires_at TIMESTAMP NOT NULL
);
CREATE INDEX idx_reauth_nonces_session ON reauth_nonces(session_id);
CREATE TABLE setup_tokens (
  token_hash TEXT PRIMARY KEY,
  expires_at TIMESTAMP NOT NULL,
  used_at    TIMESTAMP
);
CREATE TABLE instance_meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);`); err != nil {
		return err
	}
	born, ok := bornVersion(ctx)
	if !ok {
		born = 0 // unknown: treated as an existing database
	}
	// A database at an older version that was never used is a new install
	// whose first start stopped part-way (killed, or two processes
	// migrating at once): it gets the new default too. Used means any
	// user, session, token, account, post, draft, event or upload.
	if born < localAuthVersion {
		var used bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM users) OR EXISTS (SELECT 1 FROM sessions)
			OR EXISTS (SELECT 1 FROM api_tokens) OR EXISTS (SELECT 1 FROM accounts) OR EXISTS (SELECT 1 FROM posts)
			OR EXISTS (SELECT 1 FROM target_items) OR EXISTS (SELECT 1 FROM drafts) OR EXISTS (SELECT 1 FROM events)
			OR EXISTS (SELECT 1 FROM media_objects) OR EXISTS (SELECT 1 FROM threads_token)`).Scan(&used); err != nil {
			return err
		}
		if !used {
			born = localAuthVersion
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO instance_meta (key, value) VALUES ('created_schema', ?)`, strconv.Itoa(born))
	return err
}

// localAuthVersion is the migration that brings local sign-in (v12).
const localAuthVersion = 12
