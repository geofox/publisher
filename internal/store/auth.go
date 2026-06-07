package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"time"
)

// User is an authenticated identity (OIDC subject). Single-tenant for now:
// recorded for access control, not yet used to scope posts/drafts.
type User struct {
	ID         string
	Subject    string
	Email      string
	Name       string
	CreatedAt  time.Time
	LastSeenAt time.Time
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// UpsertUser inserts the subject or, if it already exists, refreshes its
// email/name/last_seen_at. Returns the stored row (stable id across calls).
func (s *Store) UpsertUser(subject, email, name string) (User, error) {
	now := time.Now().UTC()
	_, err := s.sql.Exec(`
		INSERT INTO users(id, subject, email, name, created_at, last_seen_at)
		VALUES(?, ?, ?, ?, ?, ?)
		ON CONFLICT(subject) DO UPDATE SET
		  email=excluded.email, name=excluded.name, last_seen_at=excluded.last_seen_at`,
		newID(), subject, email, name, now, now)
	if err != nil {
		return User{}, err
	}
	return s.UserBySubject(subject)
}

// UserBySubject loads a user by OIDC subject. Returns sql.ErrNoRows if absent.
func (s *Store) UserBySubject(subject string) (User, error) {
	return scanUser(s.sql.QueryRow(
		`SELECT id, subject, email, name, created_at, last_seen_at FROM users WHERE subject=?`, subject))
}

// UserByID loads a user by internal id. Returns sql.ErrNoRows if absent.
func (s *Store) UserByID(id string) (User, error) {
	return scanUser(s.sql.QueryRow(
		`SELECT id, subject, email, name, created_at, last_seen_at FROM users WHERE id=?`, id))
}

func scanUser(row *sql.Row) (User, error) {
	var u User
	var email, name sql.NullString
	var lastSeen sql.NullTime
	if err := row.Scan(&u.ID, &u.Subject, &email, &name, &u.CreatedAt, &lastSeen); err != nil {
		return User{}, err
	}
	u.Email, u.Name, u.LastSeenAt = email.String, name.String, lastSeen.Time
	return u, nil
}
