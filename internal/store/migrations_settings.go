package store

import "context"

// migrateSettings (v14) adds the settings the owner (or, for the
// instance, an admin) changes in the app, per-account defaults, account
// sets, and a mark for a retry asked for by hand.
//
// settings holds plain values as JSON; a secret setting lives in secrets
// (scope "setting", ref the setting's key) and never here. owner_id is an
// owner, or "instance" for the server's own settings.
//
// manual_due marks an item the owner asked to send again: with automatic
// retries off, the engine claims only those (and locked items waiting for
// their account), so turning retries off holds what was waiting for an
// automatic retry without holding what the owner retries by hand. It is
// cleared when the item is claimed.
func migrateSettings(ctx context.Context, tx execer) error {
	_, err := tx.ExecContext(ctx, `
CREATE TABLE settings (
  owner_id   TEXT NOT NULL,
  key        TEXT NOT NULL,
  value_json TEXT NOT NULL,
  updated_at TIMESTAMP NOT NULL,
  updated_by TEXT,
  PRIMARY KEY (owner_id, key)
);
ALTER TABLE accounts ADD COLUMN defaults_json TEXT NOT NULL DEFAULT '{}';
CREATE TABLE account_sets (
  id         TEXT PRIMARY KEY,
  owner_id   TEXT NOT NULL REFERENCES owners(id),
  name       TEXT NOT NULL,
  accounts   TEXT NOT NULL,
  is_default INTEGER NOT NULL DEFAULT 0,
  created_at TIMESTAMP NOT NULL,
  updated_at TIMESTAMP NOT NULL,
  UNIQUE (owner_id, name)
);
CREATE UNIQUE INDEX idx_account_sets_default ON account_sets(owner_id) WHERE is_default = 1;
ALTER TABLE target_items ADD COLUMN manual_due INTEGER NOT NULL DEFAULT 0;`)
	return err
}
