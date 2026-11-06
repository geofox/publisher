package store

import (
	"context"
	"encoding/json"
	"time"
)

// SyncRelays returns the user-managed secondary relay list (URLs), oldest first.
func (s *Store) SyncRelays() ([]string, error) {
	rows, err := s.sql.Query(`SELECT url FROM sync_relays ORDER BY added_at, url`)
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

// AddSyncRelay adds a relay to the secondary list (idempotent).
func (s *Store) AddSyncRelay(url string) error {
	_, err := s.sql.Exec(
		`INSERT INTO sync_relays(url, added_at) VALUES(?, ?) ON CONFLICT(url) DO NOTHING`,
		url, time.Now().UTC(),
	)
	return err
}

// RemoveSyncRelay removes a relay from the secondary list.
func (s *Store) RemoveSyncRelay(url string) error {
	_, err := s.sql.Exec(`DELETE FROM sync_relays WHERE url=?`, url)
	return err
}

// SeedSyncRelaysIfEmpty inserts defaults only when the table is empty, so user
// removals are never resurrected.
func (s *Store) SeedSyncRelaysIfEmpty(defaults []string) error {
	var n int
	if err := s.sql.QueryRow(`SELECT COUNT(*) FROM sync_relays`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	now := time.Now().UTC()
	for _, u := range defaults {
		if u == "" {
			continue
		}
		if _, err := s.sql.Exec(`INSERT INTO sync_relays(url, added_at) VALUES(?, ?) ON CONFLICT(url) DO NOTHING`, u, now); err != nil {
			return err
		}
	}
	return nil
}

// SetSyncRelays replaces the secondary relay list with urls, in that order.
// Relays already listed keep their place in the order; new ones follow.
func (s *Store) SetSyncRelays(urls []string) error {
	tx, err := s.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	keep := map[string]bool{}
	for _, u := range urls {
		keep[u] = true
	}
	rows, err := tx.Query(`SELECT url FROM sync_relays`)
	if err != nil {
		return err
	}
	var drop []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			rows.Close()
			return err
		}
		if !keep[u] {
			drop = append(drop, u)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, u := range drop {
		if _, err := tx.Exec(`DELETE FROM sync_relays WHERE url=?`, u); err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	for i, u := range urls {
		if _, err := tx.Exec(`INSERT INTO sync_relays(url, added_at) VALUES(?, ?) ON CONFLICT(url) DO NOTHING`,
			u, now.Add(time.Duration(i)*time.Microsecond)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Per-account relay-sync lists (v11). Each Nostr account has its own list;
// the global functions above serve the env account until the API works by
// account.

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

// AddAccountSyncRelay adds a relay to an account's list (idempotent).
func (s *Store) AddAccountSyncRelay(ctx context.Context, accountID, url string) error {
	_, err := s.sql.ExecContext(ctx, `INSERT INTO nostr_sync_relays (account_id, url, added_at) VALUES (?, ?, ?)
		ON CONFLICT (account_id, url) DO NOTHING`, accountID, url, time.Now().UTC())
	return err
}

// RemoveAccountSyncRelay removes a relay from an account's list.
func (s *Store) RemoveAccountSyncRelay(ctx context.Context, accountID, url string) error {
	_, err := s.sql.ExecContext(ctx, `DELETE FROM nostr_sync_relays WHERE account_id = ? AND url = ?`, accountID, url)
	return err
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
