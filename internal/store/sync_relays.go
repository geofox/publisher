package store

import "time"

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
