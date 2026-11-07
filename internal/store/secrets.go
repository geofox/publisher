package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SealedSecret is one row of the secret store: a value sealed by
// internal/secrets for its binding (owner, scope, ref, name, audience).
// The store never sees the plaintext.
type SealedSecret struct {
	Owner    string
	Scope    string
	Ref      string
	Name     string
	Audience string
	Sealed   string
	KeyID    string
	// CreatedAt and UpdatedAt are the store's (ignored on writes).
	CreatedAt time.Time
	UpdatedAt time.Time
}

// SecretScopeAccount is the scope of an account's credentials (ref: the
// account id).
const SecretScopeAccount = "account"

// Account events beyond account.updated: each names fields, never values.
const (
	EventAccountCreated = "account.created"
	EventAccountDeleted = "account.deleted"
	EventAccountAdopted = "account.adopted"
)

// DuplicateAccountError: the owner already has a live account of that type
// with that remote id.
type DuplicateAccountError struct {
	ID    string
	Label string
}

func (e *DuplicateAccountError) Error() string {
	return fmt.Sprintf("you already have this account (%s)", e.Label)
}

// ErrAccountDeleted: the account was deleted.
var ErrAccountDeleted = errors.New("account deleted")

// ErrNotDBAccount: the change needs an account added in the app.
var ErrNotDBAccount = errors.New("the account is configured by environment variables")

// NewAccountID returns a fresh account id; a caller that seals an
// account's secrets needs the id first (it is part of their binding).
func NewAccountID() string { return newAccountID() }

// NewAccount is an account added in the app.
type NewAccount struct {
	ID         string // from NewAccountID
	OwnerID    string
	Type       string
	Label      string
	Handle     string
	RemoteID   string
	AuthMethod string
	Config     map[string]any
	// Secrets are its credentials, sealed for scope "account" and ref ID.
	Secrets []SealedSecret
	// Actor and TokenID say who added it (the account.created event).
	Actor   string
	TokenID string
}

// CredentialChange replaces an account's configuration and every one of
// its secrets (new credentials, or the env values when it is adopted).
type CredentialChange struct {
	AuthMethod string
	Config     map[string]any
	Secrets    []SealedSecret
	// Handle and RemoteID are the identity the new credentials read.
	Handle   string
	RemoteID string
	// IfUpdatedAt, when set, refuses the change (ErrAccountConflict)
	// unless the account's updated_at is still this.
	IfUpdatedAt time.Time
	// Fields names what changed, for the event (never values).
	Fields  []string
	Actor   string
	TokenID string
}

func checkSecrets(owner, ref string, ss []SealedSecret) error {
	seen := map[string]bool{}
	for _, s := range ss {
		switch {
		case s.Owner != owner || s.Scope != SecretScopeAccount || s.Ref != ref:
			return fmt.Errorf("secret %s is bound to %s/%s/%s, not this account", s.Name, s.Owner, s.Scope, s.Ref)
		case s.Name == "" || s.Sealed == "" || s.KeyID == "":
			return errors.New("a secret needs a name, a sealed value and its key id")
		case !strings.HasPrefix(s.Sealed, "v1."+s.KeyID+"."):
			return fmt.Errorf("secret %s isn't sealed under key %s", s.Name, s.KeyID)
		case seen[s.Name]:
			return fmt.Errorf("secret %s given twice", s.Name)
		}
		seen[s.Name] = true
	}
	return nil
}

func insertSecrets(ctx context.Context, tx *sql.Tx, ss []SealedSecret, now time.Time) error {
	for _, s := range ss {
		if _, err := tx.ExecContext(ctx, `INSERT INTO secrets (owner_id, scope, ref, name, audience, sealed, key_id, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (owner_id, scope, ref, name) DO UPDATE SET audience = excluded.audience, sealed = excluded.sealed,
			  key_id = excluded.key_id, updated_at = excluded.updated_at`,
			s.Owner, s.Scope, s.Ref, s.Name, s.Audience, s.Sealed, s.KeyID, now, now); err != nil {
			return err
		}
	}
	return nil
}

func configJSON(c map[string]any) (string, error) {
	if c == nil {
		return "{}", nil
	}
	b, err := json.Marshal(c)
	return string(b), err
}

// isUniqueRemote reports whether err is the partial unique index on
// (owner_id, type, remote_id).
func isUniqueRemote(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: accounts.owner_id, accounts.type, accounts.remote_id")
}

// liveDuplicate is the owner's live account of typ with remoteID, other
// than id.
func liveDuplicate(ctx context.Context, q ctxQuerier, owner, typ, remoteID, id string) (*DuplicateAccountError, error) {
	if remoteID == "" {
		return nil, nil
	}
	var d DuplicateAccountError
	err := q.QueryRowContext(ctx, `SELECT id, label FROM accounts WHERE owner_id = ? AND type = ? AND remote_id = ?
		AND deleted_at IS NULL AND id != ?`, owner, typ, remoteID, id).Scan(&d.ID, &d.Label)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// CreateAccountWithSecrets stores an account added in the app and its
// secrets in one transaction, with an account.created event. The account
// starts enabled and not selected by default. An existing live account of
// the type with the remote id is a *DuplicateAccountError.
func (s *Store) CreateAccountWithSecrets(ctx context.Context, n NewAccount) (Account, error) {
	if n.ID == "" || n.OwnerID == "" || n.Type == "" {
		return Account{}, errors.New("a new account needs an id, an owner and a type")
	}
	if err := checkSecrets(n.OwnerID, n.ID, n.Secrets); err != nil {
		return Account{}, err
	}
	cfg, err := configJSON(n.Config)
	if err != nil {
		return Account{}, err
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return Account{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if d, err := liveDuplicate(ctx, tx, n.OwnerID, n.Type, n.RemoteID, n.ID); err != nil || d != nil {
		if err == nil {
			err = d
		}
		return Account{}, err
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO accounts (id, owner_id, type, label, handle, source, env_key, config_json, enabled,
		created_at, updated_at, remote_id, default_selected, public_feed, auth_method)
		VALUES (?, ?, ?, ?, ?, 'db', NULL, ?, 1, ?, ?, ?, 0, 1, ?)`,
		n.ID, n.OwnerID, n.Type, n.Label, nullStr(n.Handle), cfg, now, now, nullStr(n.RemoteID), n.AuthMethod); err != nil {
		if isUniqueRemote(err) {
			if d, _ := liveDuplicate(ctx, tx, n.OwnerID, n.Type, n.RemoteID, n.ID); d != nil {
				return Account{}, d
			}
			return Account{}, &DuplicateAccountError{}
		}
		return Account{}, err
	}
	if err := insertSecrets(ctx, tx, n.Secrets, now); err != nil {
		return Account{}, err
	}
	if err := emitOwner(ctx, tx, n.OwnerID, EventAccountCreated, EventData{AccountID: n.ID, Platform: n.Type,
		Fields: secretNames(n.Secrets), Actor: n.Actor, TokenID: n.TokenID}, now); err != nil {
		return Account{}, err
	}
	a, err := scanAccount(tx.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM accounts WHERE id = ?`, n.ID))
	if err != nil {
		return Account{}, err
	}
	if err := tx.Commit(); err != nil {
		return Account{}, err
	}
	s.ev.fire()
	return a, nil
}

func secretNames(ss []SealedSecret) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.Name)
	}
	return out
}

// liveAccountTx reads the owner's live account for a change.
func liveAccountTx(ctx context.Context, tx *sql.Tx, owner, id string) (Account, error) {
	a, err := scanAccount(tx.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM accounts WHERE owner_id = ? AND id = ?`, owner, id))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Account{}, ErrAccountNotFound
	case err != nil:
		return Account{}, err
	case !a.DeletedAt.IsZero():
		return a, ErrAccountDeleted
	}
	return a, nil
}

// nextUpdatedAt is a time after cur (updated_at serves as the ETag).
func nextUpdatedAt(cur time.Time) time.Time {
	t := time.Now().UTC()
	if !t.After(cur) {
		t = cur.Add(time.Microsecond)
	}
	return t
}

// ReplaceAccountCredentials replaces an app-added account's configuration
// and all of its secrets in one transaction (an account.updated event
// names c.Fields). An env account is ErrNotDBAccount; another live
// account with the new remote id a *DuplicateAccountError.
func (s *Store) ReplaceAccountCredentials(ctx context.Context, owner, id string, c CredentialChange) (Account, error) {
	return s.replaceCredentials(ctx, owner, id, c, false)
}

// AdoptAccount moves an env account into the app: its configuration and
// credentials (the environment's values, sealed) are stored and its
// source becomes db, keeping its id, env key, flags and history. From then
// on its variables are ignored. An account.adopted event names the fields.
func (s *Store) AdoptAccount(ctx context.Context, owner, id string, c CredentialChange) (Account, error) {
	return s.replaceCredentials(ctx, owner, id, c, true)
}

func (s *Store) replaceCredentials(ctx context.Context, owner, id string, c CredentialChange, adopt bool) (Account, error) {
	if err := checkSecrets(owner, id, c.Secrets); err != nil {
		return Account{}, err
	}
	cfg, err := configJSON(c.Config)
	if err != nil {
		return Account{}, err
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return Account{}, err
	}
	defer func() { _ = tx.Rollback() }()
	cur, err := liveAccountTx(ctx, tx, owner, id)
	if err != nil {
		return Account{}, err
	}
	switch {
	case adopt && cur.Source != SourceEnv:
		return cur, errors.New("the account is already in the app")
	case !adopt && cur.Source != SourceDB:
		return cur, ErrNotDBAccount
	case !c.IfUpdatedAt.IsZero() && !c.IfUpdatedAt.Equal(cur.UpdatedAt):
		return cur, ErrAccountConflict
	}
	if d, err := liveDuplicate(ctx, tx, owner, cur.Type, c.RemoteID, id); err != nil || d != nil {
		if err == nil {
			err = d
		}
		return Account{}, err
	}
	now := nextUpdatedAt(cur.UpdatedAt)
	handle := c.Handle
	if handle == "" {
		handle = cur.Handle
	}
	remote := c.RemoteID
	if remote == "" {
		remote = cur.RemoteID
	}
	if _, err := tx.ExecContext(ctx, `UPDATE accounts SET source = 'db', auth_method = ?, config_json = ?, handle = ?, remote_id = ?,
		updated_at = ? WHERE id = ?`, c.AuthMethod, cfg, nullStr(handle), nullStr(remote), now, id); err != nil {
		if isUniqueRemote(err) {
			return Account{}, &DuplicateAccountError{}
		}
		return Account{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM secrets WHERE owner_id = ? AND scope = ? AND ref = ?`, owner, SecretScopeAccount, id); err != nil {
		return Account{}, err
	}
	if err := insertSecrets(ctx, tx, c.Secrets, now); err != nil {
		return Account{}, err
	}
	typ := EventAccountUpdated
	if adopt {
		typ = EventAccountAdopted
	}
	if err := emitOwner(ctx, tx, owner, typ, EventData{AccountID: id, Platform: cur.Type, Fields: c.Fields,
		Actor: c.Actor, TokenID: c.TokenID}, now); err != nil {
		return Account{}, err
	}
	a, err := scanAccount(tx.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM accounts WHERE id = ?`, id))
	if err != nil {
		return Account{}, err
	}
	if err := tx.Commit(); err != nil {
		return Account{}, err
	}
	s.ev.fire()
	return a, nil
}

// SaveAccountState keeps state an app-added account saved itself (a
// refreshed token and its expiry): config replaces the stored
// configuration when non-nil, put upserts secrets and del removes them,
// in one transaction. It is not the owner's change: no event, and
// updated_at stays.
func (s *Store) SaveAccountState(ctx context.Context, owner, id string, config map[string]any, put []SealedSecret, del []string) error {
	if err := checkSecrets(owner, id, put); err != nil {
		return err
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	cur, err := liveAccountTx(ctx, tx, owner, id)
	if err != nil {
		return err
	}
	if cur.Source != SourceDB {
		return ErrNotDBAccount
	}
	now := time.Now().UTC()
	if config != nil {
		cfg, err := configJSON(config)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE accounts SET config_json = ? WHERE id = ?`, cfg, id); err != nil {
			return err
		}
	}
	if err := insertSecrets(ctx, tx, put, now); err != nil {
		return err
	}
	for _, name := range del {
		if _, err := tx.ExecContext(ctx, `DELETE FROM secrets WHERE owner_id = ? AND scope = ? AND ref = ? AND name = ?`,
			owner, SecretScopeAccount, id, name); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AccountDeleteReason is the error an account's canceled items carry.
const AccountDeleteReason = "its account was deleted"

// DeleteAccount soft-deletes an app-added account: deleted_at is set and it
// is turned off, its secrets and Nostr sync relay lists are deleted, and
// what it still had to send (scheduled posts, retries) is canceled with
// the reason. History keeps its posts. An env account is ErrNotDBAccount.
func (s *Store) DeleteAccount(ctx context.Context, owner, id, actor, tokenID string) error {
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	cur, err := liveAccountTx(ctx, tx, owner, id)
	if err != nil {
		return err
	}
	if cur.Source != SourceDB {
		return ErrNotDBAccount
	}
	now := nextUpdatedAt(cur.UpdatedAt)
	for _, q := range []string{
		`UPDATE accounts SET deleted_at = ?1, enabled = 0, default_selected = 0, updated_at = ?1 WHERE id = ?2`,
		`DELETE FROM secrets WHERE owner_id = ?3 AND scope = 'account' AND ref = ?2`,
		`DELETE FROM nostr_sync_relays WHERE account_id = ?2`,
		`DELETE FROM nostr_sync_relay_lists WHERE account_id = ?2`,
	} {
		if _, err := tx.ExecContext(ctx, q, now, id, owner); err != nil {
			return err
		}
	}
	if err := cancelAccountItems(ctx, tx, id, now); err != nil {
		return err
	}
	if err := emitOwner(ctx, tx, owner, EventAccountDeleted, EventData{AccountID: id, Platform: cur.Type,
		Actor: actor, TokenID: tokenID}, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.ev.fire()
	return nil
}

// cancelAccountItems cancels what the account still had to send, as
// CancelPost does for a post: planned items and failed ones still to be
// retried become canceled (with the reason), unknown ones stop being
// retried, the targets are marked given up when failed or partial, and
// each post's status is recomputed.
func cancelAccountItems(ctx context.Context, tx *sql.Tx, accountID string, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT pt.id, pt.post_id FROM post_targets pt
		WHERE pt.account_id = ? AND pt.gave_up_at IS NULL AND EXISTS (SELECT 1 FROM target_items ti
		  WHERE ti.target_id = pt.id AND ti.status IN ('planned', 'failed', 'unknown'))`, accountID)
	if err != nil {
		return err
	}
	type target struct {
		id   int64
		post string
	}
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.id, &t.post); err != nil {
			rows.Close()
			return err
		}
		targets = append(targets, t)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, t := range targets {
		before, err := readPostState(ctx, tx, t.post)
		if err != nil {
			return err
		}
		irows, err := tx.QueryContext(ctx, `UPDATE target_items SET status = 'canceled', next_attempt_at = NULL, error = ?
			WHERE target_id = ? AND status IN ('planned', 'failed') RETURNING id, ordinal`, AccountDeleteReason, t.id)
		if err != nil {
			return err
		}
		var ords []int
		for irows.Next() {
			var item int64
			var ord int
			if err := irows.Scan(&item, &ord); err != nil {
				irows.Close()
				return err
			}
			ords = append(ords, ord)
		}
		if err := irows.Close(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE target_items SET next_attempt_at = NULL WHERE target_id = ? AND status = 'unknown'`, t.id); err != nil {
			return err
		}
		var platform string
		if err := tx.QueryRowContext(ctx, `SELECT platform FROM post_targets WHERE id = ?`, t.id).Scan(&platform); err != nil {
			return err
		}
		for _, ord := range ords {
			o := ord
			if err := emit(ctx, tx, t.post, EventItemCanceled, EventData{Status: ItemCanceled, TargetID: t.id,
				AccountID: accountID, Platform: platform, Ordinal: &o, Error: AccountDeleteReason}, now); err != nil {
				return err
			}
		}
		statuses, err := itemStatuses(ctx, tx, t.id)
		if err != nil {
			return err
		}
		st := targetStatusOf(statuses)
		var gaveUp any
		if st == "failed" || st == "partial" {
			gaveUp = now
		}
		if _, err := tx.ExecContext(ctx, `UPDATE post_targets SET status = ?, gave_up_at = COALESCE(?, gave_up_at) WHERE id = ?`,
			st, gaveUp, t.id); err != nil {
			return err
		}
		if err := recomputeStatus(tx, t.post); err != nil {
			return err
		}
		if err := emitPostChange(ctx, tx, t.post, before, now); err != nil {
			return err
		}
	}
	return nil
}

// Secrets returns the sealed secrets of one owner, scope and ref.
func (s *Store) Secrets(ctx context.Context, owner, scope, ref string) ([]SealedSecret, error) {
	rows, err := s.sql.QueryContext(ctx, `SELECT owner_id, scope, ref, name, audience, sealed, key_id, created_at, updated_at
		FROM secrets WHERE owner_id = ? AND scope = ? AND ref = ? ORDER BY name`, owner, scope, ref)
	if err != nil {
		return nil, err
	}
	return scanSecrets(rows)
}

func scanSecrets(rows *sql.Rows) ([]SealedSecret, error) {
	defer rows.Close()
	var out []SealedSecret
	for rows.Next() {
		var x SealedSecret
		if err := rows.Scan(&x.Owner, &x.Scope, &x.Ref, &x.Name, &x.Audience, &x.Sealed, &x.KeyID, &x.CreatedAt, &x.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// SecretKeyIDs counts the stored secrets by the key that sealed them.
func (s *Store) SecretKeyIDs(ctx context.Context) (map[string]int, error) {
	rows, err := s.sql.QueryContext(ctx, `SELECT key_id, COUNT(*) FROM secrets GROUP BY key_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

// ResealSecrets passes every stored secret to fn and writes back the ones
// it changed (a rotation to a new master key), all in one transaction: an
// error from fn changes nothing. Returns how many changed.
func (s *Store) ResealSecrets(ctx context.Context, fn func(SealedSecret) (sealed, keyID string, changed bool, err error)) (int, error) {
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT owner_id, scope, ref, name, audience, sealed, key_id, created_at, updated_at FROM secrets`)
	if err != nil {
		return 0, err
	}
	all, err := scanSecrets(rows)
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	n := 0
	for _, x := range all {
		sealed, kid, changed, err := fn(x)
		if err != nil {
			return 0, fmt.Errorf("secret %s/%s/%s: %w", x.Scope, x.Ref, x.Name, err)
		}
		if !changed {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE secrets SET sealed = ?, key_id = ?, updated_at = ?
			WHERE owner_id = ? AND scope = ? AND ref = ? AND name = ?`, sealed, kid, now, x.Owner, x.Scope, x.Ref, x.Name); err != nil {
			return 0, err
		}
		n++
	}
	return n, tx.Commit()
}
