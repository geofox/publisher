package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// Relay-sync lists (v11): each Nostr account has its own. The global
// list from before (table sync_relays) is only read once, to give it to
// the account that owned it (AdoptLegacySyncRelays). Once an account has a
// list (nostr_sync_relay_lists), even one the owner emptied, nothing
// adopts or seeds one for it again.

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
	if err := markSyncRelayList(ctx, tx, accountID, now); err != nil {
		return err
	}
	for i, u := range urls {
		if _, err := tx.ExecContext(ctx, `INSERT INTO nostr_sync_relays (account_id, url, added_at) VALUES (?, ?, ?)
			ON CONFLICT (account_id, url) DO NOTHING`, accountID, u, now.Add(time.Duration(i)*time.Microsecond)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SeedAccountSyncRelaysOnce gives an account its starting list when it
// never had one, so the owner's removals are never brought back, not even
// the last one.
func (s *Store) SeedAccountSyncRelaysOnce(ctx context.Context, accountID string, urls []string) error {
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if has, err := hasSyncRelayList(ctx, tx, accountID); err != nil || has {
		return err
	}
	now := time.Now().UTC()
	if err := markSyncRelayList(ctx, tx, accountID, now); err != nil {
		return err
	}
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
// their own) to an account that never had a list, keeping its order. It is
// idempotent: an account with a list, even an emptied one, is left alone.
// It reports whether it copied anything; with nothing to copy the account
// still has no list, so its seed applies.
func (s *Store) AdoptLegacySyncRelays(ctx context.Context, accountID string) (bool, error) {
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if has, err := hasSyncRelayList(ctx, tx, accountID); err != nil || has {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO nostr_sync_relays (account_id, url, added_at)
		SELECT ?, url, added_at FROM sync_relays`, accountID)
	if err != nil {
		return false, err
	}
	if copied, _ := res.RowsAffected(); copied == 0 {
		return false, nil
	}
	if err := markSyncRelayList(ctx, tx, accountID, time.Now().UTC()); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// hasSyncRelayList reports whether the account has had a list.
func hasSyncRelayList(ctx context.Context, tx *sql.Tx, accountID string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM nostr_sync_relay_lists WHERE account_id = ?)
		+ (SELECT COUNT(*) FROM nostr_sync_relays WHERE account_id = ?)`, accountID, accountID).Scan(&n)
	return n > 0, err
}

// markSyncRelayList records that the account has a list from now on.
func markSyncRelayList(ctx context.Context, tx *sql.Tx, accountID string, at time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO nostr_sync_relay_lists (account_id, created_at) VALUES (?, ?)
		ON CONFLICT (account_id) DO NOTHING`, accountID, at)
	return err
}
