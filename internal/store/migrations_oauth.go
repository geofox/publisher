package store

import "context"

// migrateOAuth (v15) adds what connecting an account by signing in at its
// network needs (spec §6.1): the flows under way and the OAuth clients
// Publisher registered with servers (a Mastodon instance).
//
// A flow is found by the hash of its state (the state itself is the
// capability the network sends back, and is never stored). What the
// driver keeps between the start and the callback, and the PKCE
// verifier, are secrets: they live in the secret store (scope
// "oauth_flow", ref the flow id), so a key rotation reseals them like any
// other. A client's secret lives there too (scope "oauth_client").
//
// status is pending until the callback, then completed, failed (with
// error_code) or choose (the network offered several identities; the
// candidates are in the secret store until the owner picks one).
// used_at marks the callback that claimed the flow: a second one (a
// reload) shows the first outcome without asking the network again.
func migrateOAuth(ctx context.Context, tx execer) error {
	_, err := tx.ExecContext(ctx, `
CREATE TABLE oauth_flows (
  id                TEXT PRIMARY KEY,
  owner_id          TEXT NOT NULL REFERENCES owners(id),
  user_id           TEXT NOT NULL,
  type              TEXT NOT NULL,
  method            TEXT NOT NULL,
  account_id        TEXT,
  label             TEXT NOT NULL DEFAULT '',
  redirect_uri      TEXT NOT NULL,
  state_hash        TEXT NOT NULL UNIQUE,
  status            TEXT NOT NULL DEFAULT 'pending',
  result_account_id TEXT,
  error_code        TEXT NOT NULL DEFAULT '',
  created_at        TIMESTAMP NOT NULL,
  expires_at        TIMESTAMP NOT NULL,
  used_at           TIMESTAMP
);
CREATE INDEX idx_oauth_flows_expires ON oauth_flows(expires_at);
CREATE TABLE oauth_clients (
  id           TEXT PRIMARY KEY,
  owner_id     TEXT NOT NULL REFERENCES owners(id),
  type         TEXT NOT NULL,
  issuer       TEXT NOT NULL,
  redirect_uri TEXT NOT NULL,
  client_id    TEXT NOT NULL,
  scopes       TEXT NOT NULL,
  created_at   TIMESTAMP NOT NULL,
  UNIQUE (owner_id, type, issuer, redirect_uri)
);`)
	return err
}
