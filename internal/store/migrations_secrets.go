package store

import "context"

// migrateSecrets (v13) adds the encrypted secret store and what accounts
// added in the app need: how they were connected (auth_method), and a
// soft delete (deleted_at). An owner can't have two live accounts of a
// type with one remote id: the partial unique index closes the race two
// "add account" requests for the same account would otherwise win both.
//
// secrets holds values sealed by internal/secrets ("v1.<key id>.…"); the
// plaintext never touches the database. key_id says which master key
// sealed each value, so a missing or wrong key is found without opening
// anything.
func migrateSecrets(ctx context.Context, tx execer) error {
	_, err := tx.ExecContext(ctx, `
CREATE TABLE secrets (
  owner_id   TEXT NOT NULL,
  scope      TEXT NOT NULL,
  ref        TEXT NOT NULL,
  name       TEXT NOT NULL,
  audience   TEXT NOT NULL DEFAULT '',
  sealed     TEXT NOT NULL,
  key_id     TEXT NOT NULL,
  created_at TIMESTAMP NOT NULL,
  updated_at TIMESTAMP NOT NULL,
  PRIMARY KEY (owner_id, scope, ref, name)
);
CREATE INDEX idx_secrets_key ON secrets(key_id);
ALTER TABLE accounts ADD COLUMN deleted_at TIMESTAMP;
ALTER TABLE accounts ADD COLUMN auth_method TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_accounts_remote ON accounts(owner_id, type, remote_id)
  WHERE deleted_at IS NULL AND remote_id IS NOT NULL;`)
	return err
}
