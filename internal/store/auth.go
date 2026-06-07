package store

import (
	"crypto/rand"
	"crypto/sha256"
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

// hashToken returns the hex SHA-256 of a secret. Cookie/token secrets are high
// entropy random values, so SHA-256 (not bcrypt) is the right primitive: there
// is nothing to brute-force, and we never need to reverse it.
func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func randomToken() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:]) // 64 hex chars / 256 bits
}

// CreateSession mints a random session secret, stores only its hash, and
// returns the raw secret to put in the cookie. The DB never holds a live cookie.
func (s *Store) CreateSession(userID string, ttl time.Duration) (string, error) {
	raw := randomToken()
	now := time.Now().UTC()
	_, err := s.sql.Exec(
		`INSERT INTO sessions(id, user_id, created_at, expires_at) VALUES(?, ?, ?, ?)`,
		hashToken(raw), userID, now, now.Add(ttl))
	if err != nil {
		return "", err
	}
	return raw, nil
}

// SessionUser resolves the raw cookie value to its (unexpired) user.
func (s *Store) SessionUser(raw string) (User, error) {
	var userID string
	err := s.sql.QueryRow(
		`SELECT user_id FROM sessions WHERE id=? AND expires_at > ?`,
		hashToken(raw), time.Now().UTC()).Scan(&userID)
	if err != nil {
		return User{}, err
	}
	return s.UserByID(userID)
}

// DeleteSession removes a session by its raw cookie value (logout).
func (s *Store) DeleteSession(raw string) error {
	_, err := s.sql.Exec(`DELETE FROM sessions WHERE id=?`, hashToken(raw))
	return err
}

// SweepExpiredSessions deletes expired rows; returns how many were removed.
func (s *Store) SweepExpiredSessions() (int64, error) {
	res, err := s.sql.Exec(`DELETE FROM sessions WHERE expires_at <= ?`, time.Now().UTC())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
