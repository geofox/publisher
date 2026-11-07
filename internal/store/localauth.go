package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Local sign-in (AUTH_MODE=local): the owner's passkeys and optional
// password, the first-run setup token, re-authentication nonces and the
// failed password attempts. Every function runs its own transaction
// (BEGIN IMMEDIATE, see Open), so a check and the write it guards are atomic.

// LocalSubject is the local owner's users.subject.
const LocalSubject = "local:owner"

// Errors of local sign-in.
var (
	// ErrSetupDone: a local credential already exists, so the first one
	// can't be created again.
	ErrSetupDone = errors.New("setup is already done")
	// ErrLastSignInMethod: removing it would leave no way to sign in.
	ErrLastSignInMethod = errors.New("that is the last way to sign in")
	// ErrInvalidToken: a setup token or nonce that is unknown, used,
	// expired, or bound to something else.
	ErrInvalidToken = errors.New("the token is unknown, used or expired")
	// ErrCounterReplay: a passkey's signature counter didn't move forward.
	ErrCounterReplay = errors.New("the passkey's signature counter went backwards")
	// ErrDuplicatePasskey: that passkey (its credential id) is stored already.
	ErrDuplicatePasskey = errors.New("that passkey is added already")
)

// WebAuthnCredential is a stored passkey.
type WebAuthnCredential struct {
	ID              string
	UserID          string
	RPID            string
	CredentialID    []byte
	PublicKey       []byte
	SignCount       uint32
	Flags           uint8 // go-webauthn's CredentialFlags byte (backup eligible/state, ...)
	Transports      []string
	AAGUID          []byte
	AttestationType string
	Name            string
	CreatedAt       time.Time
	LastUsedAt      time.Time
}

const webauthnColumns = `id, user_id, rp_id, credential_id, public_key, sign_count, flags, transports, aaguid,
	attestation_type, name, created_at, last_used_at`

func scanWebAuthn(r rowScanner) (WebAuthnCredential, error) {
	var c WebAuthnCredential
	var transports string
	var used sql.NullTime
	var flags, count int64
	if err := r.Scan(&c.ID, &c.UserID, &c.RPID, &c.CredentialID, &c.PublicKey, &count, &flags, &transports, &c.AAGUID,
		&c.AttestationType, &c.Name, &c.CreatedAt, &used); err != nil {
		return WebAuthnCredential{}, err
	}
	c.SignCount, c.Flags = uint32(count), uint8(flags)
	c.Transports = strings.Fields(transports)
	c.LastUsedAt = used.Time
	return c, nil
}

// LocalUser is the local owner, or sql.ErrNoRows before setup.
func (s *Store) LocalUser(ctx context.Context) (User, error) {
	return scanUser(s.sql.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE subject = ? AND source = 'local'`, LocalSubject))
}

// EnsureLocalUser returns the local owner, creating its row (without any
// way to sign in) when missing: a first passkey needs its user handle
// before it exists.
func (s *Store) EnsureLocalUser(ctx context.Context) (User, error) {
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback() }()
	u, err := localUserTx(ctx, tx, time.Now().UTC())
	if err != nil {
		return User{}, err
	}
	return u, tx.Commit()
}

// localUserTx returns the local owner, creating it.
func localUserTx(ctx context.Context, tx *sql.Tx, now time.Time) (User, error) {
	if _, err := tx.ExecContext(ctx, `INSERT INTO users (id, subject, name, created_at, source) VALUES (?, ?, 'Owner', ?, 'local')
		ON CONFLICT(subject) DO NOTHING`, newID(), LocalSubject, now); err != nil {
		return User{}, err
	}
	u, err := scanUser(tx.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE subject = ?`, LocalSubject))
	if err == nil && u.Source != UserLocal {
		return User{}, errors.New("the local owner's subject is held by an identity provider's user")
	}
	return u, err
}

// localMethodsTx counts the local owner's ways to sign in: the password
// and the passkeys for relying party rpID (a passkey made for another
// host, before PUBLIC_URL changed, can't be used). With rpID "" (no
// PUBLIC_URL, so no passkey can be used yet) every passkey counts.
func localMethodsTx(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, rpID string) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM webauthn_credentials w JOIN users u ON u.id = w.user_id
			WHERE u.source = 'local' AND (? = '' OR w.rp_id = ?)) +
		(SELECT COUNT(*) FROM password_credentials p JOIN users u ON u.id = p.user_id WHERE u.source = 'local')`,
		rpID, rpID).Scan(&n)
	return n, err
}

// HasLocalCredential reports whether the local owner can sign in with
// relying party rpID (setup is done).
func (s *Store) HasLocalCredential(ctx context.Context, rpID string) (bool, error) {
	n, err := localMethodsTx(ctx, s.sql, rpID)
	return n > 0, err
}

// CreateFirstPasskey stores the first local credential (for its relying
// party), a passkey, and the local owner, in one transaction:
// ErrSetupDone when one exists, so two setups finishing at once can't
// both win.
func (s *Store) CreateFirstPasskey(ctx context.Context, c WebAuthnCredential) (User, WebAuthnCredential, error) {
	var u User
	err := s.firstCredential(ctx, c.RPID, func(tx *sql.Tx, now time.Time) error {
		var err error
		if u, err = localUserTx(ctx, tx, now); err != nil {
			return err
		}
		c, err = insertWebAuthnTx(ctx, tx, u.ID, c, now)
		return err
	})
	return u, c, err
}

// CreateFirstPassword is CreateFirstPasskey for a password (a PHC
// string), with relying party rpID.
func (s *Store) CreateFirstPassword(ctx context.Context, rpID, phc string) (User, error) {
	var u User
	err := s.firstCredential(ctx, rpID, func(tx *sql.Tx, now time.Time) error {
		var err error
		if u, err = localUserTx(ctx, tx, now); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO password_credentials (user_id, phc, updated_at) VALUES (?, ?, ?)`, u.ID, phc, now)
		return err
	})
	return u, err
}

func (s *Store) firstCredential(ctx context.Context, rpID string, insert func(*sql.Tx, time.Time) error) error {
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	n, err := localMethodsTx(ctx, tx, rpID)
	if err != nil {
		return err
	}
	if n > 0 {
		return ErrSetupDone
	}
	if err := insert(tx, time.Now().UTC()); err != nil {
		return err
	}
	// Setup is done: no token opens it again.
	if _, err := tx.ExecContext(ctx, `DELETE FROM setup_tokens`); err != nil {
		return err
	}
	return tx.Commit()
}

func insertWebAuthnTx(ctx context.Context, tx *sql.Tx, userID string, c WebAuthnCredential, now time.Time) (WebAuthnCredential, error) {
	c.ID, c.UserID, c.CreatedAt = "pk_"+newID()[:20], userID, now
	_, err := tx.ExecContext(ctx, `INSERT INTO webauthn_credentials (id, user_id, rp_id, credential_id, public_key, sign_count,
		flags, transports, aaguid, attestation_type, name, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, userID, c.RPID, c.CredentialID, c.PublicKey, int64(c.SignCount), int64(c.Flags), strings.Join(c.Transports, " "),
		c.AAGUID, c.AttestationType, c.Name, now)
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: webauthn_credentials.credential_id") {
		err = ErrDuplicatePasskey
	}
	return c, err
}

// AddWebAuthnCredential stores another passkey for a user.
func (s *Store) AddWebAuthnCredential(ctx context.Context, userID string, c WebAuthnCredential) (WebAuthnCredential, error) {
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return WebAuthnCredential{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if c, err = insertWebAuthnTx(ctx, tx, userID, c, time.Now().UTC()); err != nil {
		return WebAuthnCredential{}, err
	}
	return c, tx.Commit()
}

// WebAuthnCredentials lists a user's passkeys, oldest first.
func (s *Store) WebAuthnCredentials(ctx context.Context, userID string) ([]WebAuthnCredential, error) {
	rows, err := s.sql.QueryContext(ctx, `SELECT `+webauthnColumns+` FROM webauthn_credentials WHERE user_id = ? ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WebAuthnCredential{}
	for rows.Next() {
		c, err := scanWebAuthn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// WebAuthnCredentialByCredID finds a passkey by the authenticator's
// credential id; sql.ErrNoRows when unknown.
func (s *Store) WebAuthnCredentialByCredID(ctx context.Context, credID []byte) (WebAuthnCredential, error) {
	return scanWebAuthn(s.sql.QueryRowContext(ctx, `SELECT `+webauthnColumns+` FROM webauthn_credentials WHERE credential_id = ?`, credID))
}

// UseWebAuthnCredential records a successful assertion: the new signature
// counter, flags and time. The counter must move forward unless the
// authenticator always reports 0 (ErrCounterReplay otherwise); the check
// is in the UPDATE, so two assertions racing with one counter value can't
// both pass.
func (s *Store) UseWebAuthnCredential(ctx context.Context, id string, count uint32, flags uint8) error {
	res, err := s.sql.ExecContext(ctx, `UPDATE webauthn_credentials SET sign_count = ?, flags = ?, last_used_at = ?
		WHERE id = ? AND (sign_count < ? OR (? = 0 AND sign_count = 0))`,
		int64(count), int64(flags), time.Now().UTC(), id, int64(count), int64(count))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrCounterReplay
	}
	return nil
}

// RenameWebAuthnCredential names one of a user's passkeys; sql.ErrNoRows
// when the user has no such passkey.
func (s *Store) RenameWebAuthnCredential(ctx context.Context, userID, id, name string) error {
	res, err := s.sql.ExecContext(ctx, `UPDATE webauthn_credentials SET name = ? WHERE id = ? AND user_id = ?`, name, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// RemoveWebAuthnCredential deletes one of a user's passkeys, unless it is
// the last way to sign in with relying party rpID (ErrLastSignInMethod);
// sql.ErrNoRows when the user has no such passkey.
func (s *Store) RemoveWebAuthnCredential(ctx context.Context, userID, id, rpID string) error {
	return s.removeMethod(ctx, rpID, `DELETE FROM webauthn_credentials WHERE id = ? AND user_id = ?`, id, userID)
}

// RemovePassword deletes a user's password, unless it is the last way to
// sign in; sql.ErrNoRows when none is set.
func (s *Store) RemovePassword(ctx context.Context, userID, rpID string) error {
	return s.removeMethod(ctx, rpID, `DELETE FROM password_credentials WHERE user_id = ?`, userID)
}

// removeMethod deletes a way to sign in, unless it was usable and no
// other usable one is left (removing a passkey made for another host is
// always allowed).
func (s *Store) removeMethod(ctx context.Context, rpID, del string, args ...any) error {
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	before, err := localMethodsTx(ctx, tx, rpID)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, del, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	if after, err := localMethodsTx(ctx, tx, rpID); err != nil {
		return err
	} else if after == 0 && before > 0 {
		return ErrLastSignInMethod
	}
	return tx.Commit()
}

// PasswordPHC is a user's password hash (a PHC string); sql.ErrNoRows when
// none is set.
func (s *Store) PasswordPHC(ctx context.Context, userID string) (string, error) {
	var phc string
	err := s.sql.QueryRowContext(ctx, `SELECT phc FROM password_credentials WHERE user_id = ?`, userID).Scan(&phc)
	return phc, err
}

// SetPassword stores a user's password hash, replacing any.
func (s *Store) SetPassword(ctx context.Context, userID, phc string) error {
	_, err := s.sql.ExecContext(ctx, `INSERT INTO password_credentials (user_id, phc, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET phc = excluded.phc, updated_at = excluded.updated_at`, userID, phc, time.Now().UTC())
	return err
}

// PasswordSetAt is when a user's password was last set (zero when none).
func (s *Store) PasswordSetAt(ctx context.Context, userID string) (time.Time, error) {
	var t time.Time
	err := s.sql.QueryRowContext(ctx, `SELECT updated_at FROM password_credentials WHERE user_id = ?`, userID).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	return t, err
}

// MintSetupToken stores a new setup token valid for ttl, invalidating
// every earlier one, and returns it raw (only its hash is stored).
func (s *Store) MintSetupToken(ctx context.Context, ttl time.Duration) (string, time.Time, error) {
	raw := randomToken()
	exp := time.Now().UTC().Add(ttl)
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM setup_tokens`); err != nil {
		return "", time.Time{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO setup_tokens (token_hash, expires_at) VALUES (?, ?)`, hashToken(raw), exp); err != nil {
		return "", time.Time{}, err
	}
	return raw, exp, tx.Commit()
}

// ConsumeSetupToken uses a setup token once: ErrInvalidToken when it is
// unknown, used or expired.
func (s *Store) ConsumeSetupToken(ctx context.Context, raw string) error {
	now := time.Now().UTC()
	res, err := s.sql.ExecContext(ctx, `UPDATE setup_tokens SET used_at = ? WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?`,
		now, hashToken(raw), now)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrInvalidToken
	}
	return nil
}

// SetupTokenCurrent reports whether raw is the newest setup token (used
// or not): a newer one, reset-login or the end of setup deletes it.
func (s *Store) SetupTokenCurrent(ctx context.Context, raw string) (bool, error) {
	var n int
	err := s.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM setup_tokens WHERE token_hash = ?`, hashToken(raw)).Scan(&n)
	return n > 0, err
}

// RecordLoginFailure notes a failed password attempt from addr.
func (s *Store) RecordLoginFailure(ctx context.Context, addr string, at time.Time) error {
	_, err := s.sql.ExecContext(ctx, `INSERT INTO login_failures (addr, at) VALUES (?, ?)`, addr, at.UTC())
	return err
}

// LoginFailures are the failed password attempts since, oldest first:
// from addr, or from anywhere when addr is "".
func (s *Store) LoginFailures(ctx context.Context, addr string, since time.Time) ([]time.Time, error) {
	rows, err := s.sql.QueryContext(ctx, `SELECT at FROM login_failures WHERE (? = '' OR addr = ?) AND at > ? ORDER BY at`,
		addr, addr, since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var t time.Time
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ClearLoginFailures forgets addr's failed attempts (after it signed in).
func (s *Store) ClearLoginFailures(ctx context.Context, addr string) error {
	_, err := s.sql.ExecContext(ctx, `DELETE FROM login_failures WHERE addr = ?`, addr)
	return err
}

// Failed attempts are kept a day, and at most this many.
const (
	LoginFailureRetention = 24 * time.Hour
	maxLoginFailures      = 10000
)

// PruneLoginFailures drops attempts older than a day and all but the
// newest 10,000.
func (s *Store) PruneLoginFailures(ctx context.Context, now time.Time) error {
	if _, err := s.sql.ExecContext(ctx, `DELETE FROM login_failures WHERE at <= ?`, now.UTC().Add(-LoginFailureRetention)); err != nil {
		return err
	}
	_, err := s.sql.ExecContext(ctx, `DELETE FROM login_failures WHERE rowid NOT IN
		(SELECT rowid FROM login_failures ORDER BY at DESC LIMIT ?)`, maxLoginFailures)
	return err
}

// CreateReauthNonce mints a single-use nonce for one operation (on one
// target, "" for none) by one session, valid for ttl.
func (s *Store) CreateReauthNonce(ctx context.Context, sessionHash, operation, target string, ttl time.Duration) (string, time.Time, error) {
	raw := randomToken()
	exp := time.Now().UTC().Add(ttl)
	_, err := s.sql.ExecContext(ctx, `INSERT INTO reauth_nonces (nonce_hash, session_id, operation, target, expires_at) VALUES (?, ?, ?, ?, ?)`,
		hashToken(raw), sessionHash, operation, target, exp)
	return raw, exp, err
}

// ConsumeReauthNonce uses a nonce once: ErrInvalidToken unless it is
// unexpired and was minted for this session, operation and target. A
// nonce presented for another operation or session is spent all the same.
// With spend false a good nonce is only checked (kept for the
// operation's last step).
func (s *Store) ConsumeReauthNonce(ctx context.Context, raw, sessionHash, operation, target string, spend bool) error {
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var sess, op, tgt string
	var exp time.Time
	h := hashToken(raw)
	err = tx.QueryRowContext(ctx, `SELECT session_id, operation, target, expires_at FROM reauth_nonces WHERE nonce_hash = ?`, h).
		Scan(&sess, &op, &tgt, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrInvalidToken
	}
	if err != nil {
		return err
	}
	good := sess == sessionHash && op == operation && tgt == target && exp.After(time.Now())
	if spend || !good {
		if _, err := tx.ExecContext(ctx, `DELETE FROM reauth_nonces WHERE nonce_hash = ?`, h); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	if !good {
		return ErrInvalidToken
	}
	return nil
}

// ResetLocalLogin removes every local credential, the local owner's
// sessions and nonces, setup tokens, the known browsers and the failed
// password attempts (the recovery command).
func (s *Store) ResetLocalLogin(ctx context.Context) error {
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{
		`DELETE FROM reauth_nonces WHERE session_id IN (SELECT s.id FROM sessions s JOIN users u ON u.id = s.user_id WHERE u.source = 'local')`,
		`DELETE FROM sessions WHERE user_id IN (SELECT id FROM users WHERE source = 'local')`,
		`DELETE FROM webauthn_credentials WHERE user_id IN (SELECT id FROM users WHERE source = 'local')`,
		`DELETE FROM password_credentials WHERE user_id IN (SELECT id FROM users WHERE source = 'local')`,
		`DELETE FROM setup_tokens`,
		`DELETE FROM instance_meta WHERE key = 'device_key'`, // forget every known browser
		`DELETE FROM login_failures`,                         // the owner gets in at once
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CreatedSchema is the schema version the database had when this build's
// line first migrated it (instance_meta created_schema): the latest
// version for a database created by it.
func (s *Store) CreatedSchema(ctx context.Context) (int, error) {
	var v string
	if err := s.sql.QueryRowContext(ctx, `SELECT value FROM instance_meta WHERE key = 'created_schema'`).Scan(&v); err != nil {
		return 0, err
	}
	return strconv.Atoi(v)
}

// InstanceKey is a random 32-byte key kept in instance_meta under name,
// made on first use.
func (s *Store) InstanceKey(ctx context.Context, name string) ([]byte, error) {
	if _, err := s.sql.ExecContext(ctx, `INSERT INTO instance_meta (key, value) VALUES (?, lower(hex(randomblob(32))))
		ON CONFLICT(key) DO NOTHING`, name); err != nil {
		return nil, err
	}
	var v string
	if err := s.sql.QueryRowContext(ctx, `SELECT value FROM instance_meta WHERE key = ?`, name).Scan(&v); err != nil {
		return nil, err
	}
	return hex.DecodeString(v)
}
