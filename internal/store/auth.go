package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// User is an authenticated identity: an OIDC subject (Source "oidc") or
// the local owner (Source "local"). Single-tenant for now: recorded for
// access control, not yet used to scope posts/drafts.
type User struct {
	ID         string
	Subject    string
	Email      string
	Name       string
	Source     string // UserOIDC or UserLocal
	CreatedAt  time.Time
	LastSeenAt time.Time
}

// User sources.
const (
	UserOIDC  = "oidc"
	UserLocal = "local"
)

// ErrNotOIDCUser is returned by UpsertUser for a subject that belongs to
// the local owner: an identity provider can't sign in as it.
var ErrNotOIDCUser = errors.New("that subject is not an OIDC identity")

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// UpsertUser inserts the OIDC subject or, if it already exists, refreshes
// its email/name/last_seen_at. Returns the stored row (stable id across
// calls). A subject held by the local owner is refused (ErrNotOIDCUser).
func (s *Store) UpsertUser(subject, email, name string) (User, error) {
	now := time.Now().UTC()
	_, err := s.sql.Exec(`
		INSERT INTO users(id, subject, email, name, created_at, last_seen_at, source)
		VALUES(?, ?, ?, ?, ?, ?, 'oidc')
		ON CONFLICT(subject) DO UPDATE SET
		  email=excluded.email, name=excluded.name, last_seen_at=excluded.last_seen_at
		WHERE users.source = 'oidc'`,
		newID(), subject, email, name, now, now)
	if err != nil {
		return User{}, err
	}
	u, err := s.UserBySubject(subject)
	if err == nil && u.Source != UserOIDC {
		return User{}, ErrNotOIDCUser
	}
	return u, err
}

// UserBySubject loads a user by subject. Returns sql.ErrNoRows if absent.
func (s *Store) UserBySubject(subject string) (User, error) {
	return scanUser(s.sql.QueryRow(`SELECT `+userColumns+` FROM users WHERE subject=?`, subject))
}

// UserByID loads a user by internal id. Returns sql.ErrNoRows if absent.
func (s *Store) UserByID(id string) (User, error) {
	return scanUser(s.sql.QueryRow(`SELECT `+userColumns+` FROM users WHERE id=?`, id))
}

const userColumns = `id, subject, email, name, source, created_at, last_seen_at`

func scanUser(row rowScanner) (User, error) {
	var u User
	var email, name sql.NullString
	var lastSeen sql.NullTime
	if err := row.Scan(&u.ID, &u.Subject, &email, &name, &u.Source, &u.CreatedAt, &lastSeen); err != nil {
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
// returns the raw secret to put in the cookie. The DB never holds a live
// cookie. The session is an OIDC sign-in; see CreateSessionWith.
func (s *Store) CreateSession(userID string, ttl time.Duration) (string, error) {
	return s.CreateSessionWith(userID, ttl, SessionMeta{Method: "oidc"})
}

// SessionMeta is how a session was started, shown in the owner's list of
// sessions.
type SessionMeta struct {
	Method    string // oidc, passkey, password, setup
	UserAgent string
	Addr      string
}

// maxUserAgent bounds the stored User-Agent.
const maxUserAgent = 200

// CreateSessionWith is CreateSession with how the session was started.
func (s *Store) CreateSessionWith(userID string, ttl time.Duration, m SessionMeta) (string, error) {
	raw := randomToken()
	now := time.Now().UTC()
	ua := m.UserAgent
	if len(ua) > maxUserAgent {
		ua = ua[:maxUserAgent]
	}
	_, err := s.sql.Exec(
		`INSERT INTO sessions(id, user_id, created_at, expires_at, public_id, last_seen_at, method, user_agent, addr)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		hashToken(raw), userID, now, now.Add(ttl), newPublicID(), now, m.Method, strings.ToValidUTF8(ua, ""), m.Addr)
	if err != nil {
		return "", err
	}
	return raw, nil
}

// newPublicID is a session's id in the API: random, unrelated to the
// cookie or its hash.
func newPublicID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Session is one signed-in browser. ID is the cookie's hash (internal
// only); the API names a session by PublicID.
type Session struct {
	ID         string
	PublicID   string
	UserID     string
	Method     string
	UserAgent  string
	Addr       string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastSeenAt time.Time
}

const sessionColumns = `id, COALESCE(public_id, ''), user_id, method, user_agent, addr, created_at, expires_at, last_seen_at`

func scanSession(r rowScanner) (Session, error) {
	var x Session
	var seen sql.NullTime
	if err := r.Scan(&x.ID, &x.PublicID, &x.UserID, &x.Method, &x.UserAgent, &x.Addr, &x.CreatedAt, &x.ExpiresAt, &seen); err != nil {
		return Session{}, err
	}
	x.LastSeenAt = seen.Time
	return x, nil
}

// sessionTouchEvery bounds how often a session's last use is written.
const sessionTouchEvery = time.Minute

// SessionUser resolves the raw cookie value to its (unexpired) user.
func (s *Store) SessionUser(raw string) (User, error) {
	_, u, err := s.SessionByRaw(raw)
	return u, err
}

// SessionByRaw resolves the raw cookie value to its unexpired session and
// user, and notes when it was last used (at most once a minute).
func (s *Store) SessionByRaw(raw string) (Session, User, error) {
	now := time.Now().UTC()
	x, err := scanSession(s.sql.QueryRow(`SELECT `+sessionColumns+` FROM sessions WHERE id=? AND expires_at > ?`,
		hashToken(raw), now))
	if err != nil {
		return Session{}, User{}, err
	}
	u, err := s.UserByID(x.UserID)
	if err != nil {
		return Session{}, User{}, err
	}
	if now.Sub(x.LastSeenAt) >= sessionTouchEvery {
		_, _ = s.sql.Exec(`UPDATE sessions SET last_seen_at=? WHERE id=?`, now, x.ID)
		x.LastSeenAt = now
	}
	return x, u, nil
}

// SessionHash is the stored id of a raw session cookie.
func SessionHash(raw string) string { return hashToken(raw) }

// DeleteSession removes a session by its raw cookie value (logout), and
// the re-authentication nonces it holds.
func (s *Store) DeleteSession(raw string) error {
	return s.deleteSessions(context.Background(), `id = ?`, hashToken(raw))
}

// deleteSessions removes the sessions matching where, and their nonces,
// in one transaction.
func (s *Store) deleteSessions(ctx context.Context, where string, args ...any) error {
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM reauth_nonces WHERE session_id IN (SELECT id FROM sessions WHERE `+where+`)`, args...); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE `+where, args...); err != nil {
		return err
	}
	return tx.Commit()
}

// ListSessions is a user's unexpired sessions, most recently used first.
func (s *Store) ListSessions(ctx context.Context, userID string) ([]Session, error) {
	rows, err := s.sql.QueryContext(ctx, `SELECT `+sessionColumns+` FROM sessions
		WHERE user_id = ? AND expires_at > ? ORDER BY COALESCE(last_seen_at, created_at) DESC, created_at DESC`,
		userID, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		x, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// DeleteSessionByPublicID ends one of a user's sessions. sql.ErrNoRows when
// the user has no such session.
func (s *Store) DeleteSessionByPublicID(ctx context.Context, userID, publicID string) error {
	var id string
	if err := s.sql.QueryRowContext(ctx, `SELECT id FROM sessions WHERE public_id = ? AND user_id = ?`, publicID, userID).Scan(&id); err != nil {
		return err
	}
	return s.deleteSessions(ctx, `id = ?`, id)
}

// DeleteOtherSessions ends every session of the user but keepHash (""
// ends them all), with their nonces.
func (s *Store) DeleteOtherSessions(ctx context.Context, userID, keepHash string) error {
	return s.deleteSessions(ctx, `user_id = ? AND id != ?`, userID, keepHash)
}

// SweepExpiredSessions deletes expired rows (and expired re-authentication
// nonces); returns how many sessions were removed.
func (s *Store) SweepExpiredSessions() (int64, error) {
	now := time.Now().UTC()
	_, _ = s.sql.Exec(`DELETE FROM reauth_nonces WHERE expires_at <= ? OR session_id IN (SELECT id FROM sessions WHERE expires_at <= ?)`, now, now)
	res, err := s.sql.Exec(`DELETE FROM sessions WHERE expires_at <= ?`, now)
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
