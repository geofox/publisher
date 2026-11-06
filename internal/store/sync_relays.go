package store

import (
	"context"
	"encoding/json"
	"time"
)

// Relay-sync lists (v11): each Nostr account has its own. The global
// list from before (table sync_relays) is only read once, to give it to
// the account that owned it (AdoptLegacySyncRelays).

// AccountSyncRelays returns an account's relay-sync list, oldest first.
func (s *Store) AccountSyncRelays(ctx context.Context, accountID string) ([]string, error) {
	rows, err := s.sql.QueryContext(ctx, `SELECT url FROM nostr_sync_relays WHERE account_id = ? ORDER BY added_at, url`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetAccountSyncRelays replaces an account's list with urls, in that order.
// Relays already listed keep their place; new ones follow.
func (s *Store) SetAccountSyncRelays(ctx context.Context, accountID string, urls []string) error {
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	keep, err := json.Marshal(urls)
	if err != nil {
		return err
	}
	if urls == nil {
		keep = []byte("[]")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM nostr_sync_relays WHERE account_id = ?
		AND url NOT IN (SELECT value FROM json_each(?))`, accountID, string(keep)); err != nil {
		return err
	}
	now := time.Now().UTC()
	for i, u := range urls {
		if _, err := tx.ExecContext(ctx, `INSERT INTO nostr_sync_relays (account_id, url, added_at) VALUES (?, ?, ?)
			ON CONFLICT (account_id, url) DO NOTHING`, accountID, u, now.Add(time.Duration(i)*time.Microsecond)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SeedAccountSyncRelaysIfEmpty gives an account its starting list only when
// it has none, so the owner's removals are never brought back while one
// relay is left.
func (s *Store) SeedAccountSyncRelaysIfEmpty(ctx context.Context, accountID string, urls []string) error {
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM nostr_sync_relays WHERE account_id = ?`, accountID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	now := time.Now().UTC()
	for i, u := range urls {
		if u == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO nostr_sync_relays (account_id, url, added_at) VALUES (?, ?, ?)
			ON CONFLICT (account_id, url) DO NOTHING`, accountID, u, now.Add(time.Duration(i)*time.Microsecond)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AdoptLegacySyncRelays copies the global list (from before accounts had
// their own) to an account that has none yet, keeping its order. It is
// idempotent: an account with a list is left alone. It reports whether it
// copied anything.
func (s *Store) AdoptLegacySyncRelays(ctx context.Context, accountID string) (bool, error) {
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM nostr_sync_relays WHERE account_id = ?`, accountID).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO nostr_sync_relays (account_id, url, added_at)
		SELECT ?, url, added_at FROM sync_relays`, accountID)
	if err != nil {
		return false, err
	}
	copied, _ := res.RowsAffected()
	return copied > 0, tx.Commit()
}
