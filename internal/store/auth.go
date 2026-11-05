package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"strings"
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

// APIToken is a machine credential row. The secret is never stored or returned
// after creation — only its hash.
type APIToken struct {
	ID         string
	OwnerID    string
	Name       string
	Scopes     []string
	CreatedAt  time.Time
	LastUsedAt time.Time
	ExpiresAt  time.Time // zero: never
	RevokedAt  time.Time
	Revoked    bool
}

// LegacyTokenScopes are what a token made before scopes could do: /publish
// and /upload-media.
var LegacyTokenScopes = []string{"posts:write", "media:write"}

// CreateAPIToken mints a token with the old routes' scopes and no expiry.
func (s *Store) CreateAPIToken(name string) (APIToken, string, error) {
	return s.CreateScopedToken(name, LegacyTokenScopes, time.Time{})
}

// CreateScopedToken mints a token, stores its hash, and returns the row plus
// the raw secret (shown to the operator exactly once). A zero expiresAt
// never expires.
func (s *Store) CreateScopedToken(name string, scopes []string, expiresAt time.Time) (APIToken, string, error) {
	raw := randomToken()
	id := newID()
	now := time.Now().UTC()
	var exp any
	if !expiresAt.IsZero() {
		exp = expiresAt.UTC()
	}
	_, err := s.sql.Exec(
		`INSERT INTO api_tokens(id, name, token_hash, created_at, scopes, expires_at) VALUES(?, ?, ?, ?, ?, ?)`,
		id, name, hashToken(raw), now, strings.Join(scopes, " "), exp)
	if err != nil {
		return APIToken{}, "", err
	}
	return APIToken{ID: id, OwnerID: DefaultOwner, Name: name, Scopes: scopes, CreatedAt: now, ExpiresAt: expiresAt.UTC()}, raw, nil
}

// APITokenByRaw resolves a presented bearer secret to a live (non-revoked,
// unexpired) token and stamps last_used_at. Returns sql.ErrNoRows otherwise.
func (s *Store) APITokenByRaw(raw string) (APIToken, error) {
	h := hashToken(raw)
	now := time.Now().UTC()
	t, err := scanToken(s.sql.QueryRow(`SELECT `+tokenColumns+` FROM api_tokens
		 WHERE token_hash=? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)`, h, now))
	if err != nil {
		return APIToken{}, err
	}
	_, _ = s.sql.Exec(`UPDATE api_tokens SET last_used_at=? WHERE id=?`, now, t.ID)
	t.LastUsedAt = now
	return t, nil
}

// APIToken returns one token by id, never the secret.
func (s *Store) APIToken(id string) (APIToken, error) {
	return scanToken(s.sql.QueryRow(`SELECT `+tokenColumns+` FROM api_tokens WHERE id=?`, id))
}

const tokenColumns = `id, owner_id, name, scopes, created_at, last_used_at, expires_at, revoked_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanToken(r rowScanner) (APIToken, error) {
	var t APIToken
	var scopes string
	var lastUsed, expires, revoked sql.NullTime
	if err := r.Scan(&t.ID, &t.OwnerID, &t.Name, &scopes, &t.CreatedAt, &lastUsed, &expires, &revoked); err != nil {
		return APIToken{}, err
	}
	t.Scopes = strings.Fields(scopes)
	t.LastUsedAt, t.ExpiresAt, t.RevokedAt, t.Revoked = lastUsed.Time, expires.Time, revoked.Time, revoked.Valid
	return t, nil
}

// ListAPITokens returns all tokens (newest first), never the secret.
func (s *Store) ListAPITokens() ([]APIToken, error) {
	rows, err := s.sql.Query(`SELECT ` + tokenColumns + ` FROM api_tokens ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]APIToken, 0)
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeAPIToken soft-deletes a token (keeps the row for audit). Returns
// sql.ErrNoRows when the id is unknown or already revoked, so callers can
// surface a 404 rather than a misleading success.
func (s *Store) RevokeAPIToken(id string) error {
	res, err := s.sql.Exec(`UPDATE api_tokens SET revoked_at=? WHERE id=? AND revoked_at IS NULL`,
		time.Now().UTC(), id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
