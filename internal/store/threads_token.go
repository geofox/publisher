package store

import (
	"database/sql"
	"errors"
	"time"
)

// ThreadsToken is the publisher's persisted Threads long-lived token state
// (singleton row id=1). Token carries json:"-" so it can never leak through any
// API response, mirroring Target.SignedEventJSON.
type ThreadsToken struct {
	Token       string    `json:"-"`
	ExpiresAt   time.Time `json:"expires_at"`
	SeedHash    string    `json:"seed_hash"`
	RefreshedAt time.Time `json:"refreshed_at"`
}

// GetThreadsToken returns the persisted token state, or (nil, nil) when absent.
func (s *Store) GetThreadsToken() (*ThreadsToken, error) {
	var t ThreadsToken
	err := s.sql.QueryRow(
		`SELECT token, expires_at, seed_hash, refreshed_at FROM threads_token WHERE id = 1`,
	).Scan(&t.Token, &t.ExpiresAt, &t.SeedHash, &t.RefreshedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// SaveThreadsToken upserts the singleton token row.
func (s *Store) SaveThreadsToken(token string, expiresAt time.Time, seedHash string, refreshedAt time.Time) error {
	_, err := s.sql.Exec(
		`INSERT INTO threads_token(id, token, expires_at, seed_hash, refreshed_at)
		 VALUES(1, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   token=excluded.token, expires_at=excluded.expires_at,
		   seed_hash=excluded.seed_hash, refreshed_at=excluded.refreshed_at`,
		token, expiresAt.UTC(), seedHash, refreshedAt.UTC(),
	)
	return err
}
