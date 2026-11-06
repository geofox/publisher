package store

import "context"

// migrateAccountsByID (v11) prepares accounts for more than one per type.
//
//   - remote_id is the account's stable id on its network (a DID, an actor
//     id, a user id, a public key): it tells two accounts of a type apart
//     when handles change. default_selected says whether the composer
//     selects the account for a new post; public_feed whether its posts go
//     to the public feed.
//   - Before step 4, enabled = 0 on an env account only ever meant "its
//     variables are not set". The account registry now decides that at
//     start, and enabled becomes the owner's own switch, so every env
//     account starts enabled.
//   - The relay-sync list belongs to a Nostr account (nostr_sync_relays).
//     The registry adopts the old global list (sync_relays) for the env
//     Nostr account; the old table stays for older binaries and a
//     rollback.
//   - v4 backfilled nostr_fanout.item_id, but EnqueueFanout never wrote it,
//     so every job queued since lacks it; the same backfill runs again.
func migrateAccountsByID(ctx context.Context, tx execer) error {
	_, err := tx.ExecContext(ctx, `
ALTER TABLE accounts ADD COLUMN remote_id TEXT;
ALTER TABLE accounts ADD COLUMN default_selected INTEGER NOT NULL DEFAULT 1;
ALTER TABLE accounts ADD COLUMN public_feed INTEGER NOT NULL DEFAULT 1;
UPDATE accounts SET enabled = 1 WHERE source = 'env';
CREATE TABLE nostr_sync_relays (
  account_id TEXT NOT NULL REFERENCES accounts(id),
  url        TEXT NOT NULL,
  added_at   TIMESTAMP NOT NULL,
  PRIMARY KEY (account_id, url)
);
UPDATE nostr_fanout SET item_id = (
  SELECT ti.id FROM target_items ti JOIN post_targets pt ON pt.id = ti.target_id
   WHERE pt.post_id = nostr_fanout.post_id AND ti.ref_json = nostr_fanout.signed_event_json
   ORDER BY ti.id LIMIT 1)
 WHERE item_id IS NULL;`)
	return err
}
