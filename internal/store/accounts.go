package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Account is one destination account an owner publishes to.
type Account struct {
	ID        string    `json:"id"`
	OwnerID   string    `json:"-"`
	Type      string    `json:"type"`
	Label     string    `json:"label"`
	Handle    string    `json:"handle,omitempty"`
	Source    string    `json:"source"` // env (step 4 adds db)
	EnvKey    string    `json:"-"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// RemoteID is the account's stable id on its network ("" until its
	// identity was first read).
	RemoteID string `json:"remote_id,omitempty"`
	// DefaultSelected: the composer selects the account for a new post.
	DefaultSelected bool `json:"default_selected"`
	// PublicFeed: the account's posts go to the public feed.
	PublicFeed bool `json:"public_feed"`
	// AuthMethod is the destination.AuthMethod the account was added with
	// ("" for an env account).
	AuthMethod string `json:"auth_method,omitempty"`
	// Config is the account's non-secret configuration, as JSON (an
	// account added in the app; "{}" otherwise).
	Config string `json:"-"`
	// DeletedAt is when the owner deleted the account (zero: not deleted).
	// A deleted account keeps its row, so History can still name it.
	DeletedAt time.Time `json:"deleted_at,omitzero"`
}

// Account sources.
const (
	SourceEnv = "env" // configured by environment variables
	SourceDB  = "db"  // added in the app, credentials in secrets
)

// AccountMeta is what the owner can change on an account; a nil field is
// left as it is.
type AccountMeta struct {
	Label           *string
	Enabled         *bool
	DefaultSelected *bool
	PublicFeed      *bool
	// IfUpdatedAt, when set, refuses the change (ErrAccountConflict)
	// unless the account's updated_at is still this: the caller read it.
	IfUpdatedAt time.Time
	// Actor and TokenID say who changed it, for the account.updated event
	// ("session", "token" or "proxy"; the token's id for a token).
	Actor   string
	TokenID string
}

// EnvKeyDefault is the env_key of the account seeded from a type's
// unnumbered env vars (the only kind until step 4).
const EnvKeyDefault = "default"

func newAccountID() string { return "acc_" + newID() }

// EnvAccount is one account the environment configures.
type EnvAccount struct {
	Type string
	// Key tells apart several accounts of a type in the environment
	// (EnvKeyDefault for a type's unnumbered variables).
	Key   string
	Label string // the label a new row gets
}

// SeedEnvAccounts gives every account the environment configures a stored
// row, created on first sight with an id that then never changes (and the
// given label). It never changes an existing row: whether an env account
// can be used is the account registry's call, and enabled is the owner's.
// Rows of accounts added in the app are not touched. Returns the owner's
// env rows, oldest first.
func (s *Store) SeedEnvAccounts(ctx context.Context, owner string, configured []EnvAccount) ([]Account, error) {
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC()
	for i, c := range configured {
		key := c.Key
		if key == "" {
			key = EnvKeyDefault
		}
		// Rows seeded together keep the order given (the registry orders
		// a type's accounts oldest first; equal times would fall back to
		// the random ids).
		at := now.Add(time.Duration(i) * time.Microsecond)
		if _, err := tx.ExecContext(ctx, `INSERT INTO accounts (id, owner_id, type, label, source, env_key, enabled, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'env', ?, 1, ?, ?)
			ON CONFLICT (owner_id, type, env_key) DO NOTHING`,
			newAccountID(), owner, c.Type, c.Label, key, at, at); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.queryAccounts(ctx, `SELECT `+accountColumns+` FROM accounts WHERE owner_id = ? AND source = 'env'
		ORDER BY created_at, id`, owner)
}

// InsertAccount adds an account row exactly as given (its id included). An
// empty owner is DefaultOwner, an empty source "env", and zero times now.
// For fixtures and imports; env accounts are seeded with SeedEnvAccounts.
func (s *Store) InsertAccount(ctx context.Context, a Account) error {
	if a.OwnerID == "" {
		a.OwnerID = DefaultOwner
	}
	if a.Source == "" {
		a.Source = "env"
	}
	now := time.Now().UTC()
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	if a.UpdatedAt.IsZero() {
		a.UpdatedAt = a.CreatedAt
	}
	_, err := s.sql.ExecContext(ctx, `INSERT INTO accounts (id, owner_id, type, label, handle, source, env_key, enabled,
		created_at, updated_at, remote_id, default_selected, public_feed) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.OwnerID, a.Type, a.Label, nullStr(a.Handle), a.Source, nullStr(a.EnvKey), a.Enabled,
		a.CreatedAt.UTC(), a.UpdatedAt.UTC(), nullStr(a.RemoteID), a.DefaultSelected, a.PublicFeed)
	return err
}

// Accounts lists the owner's accounts, enabled or not, oldest first;
// deleted ones are left out.
func (s *Store) Accounts(ctx context.Context, owner string) ([]Account, error) {
	return s.queryAccounts(ctx, `SELECT `+accountColumns+` FROM accounts WHERE owner_id = ? AND deleted_at IS NULL
		ORDER BY created_at, id`, owner)
}

// AccountsWithDeleted lists the owner's accounts, deleted ones included,
// oldest first.
func (s *Store) AccountsWithDeleted(ctx context.Context, owner string) ([]Account, error) {
	return s.queryAccounts(ctx, `SELECT `+accountColumns+` FROM accounts WHERE owner_id = ? ORDER BY created_at, id`, owner)
}

// AllAccounts lists every owner's live accounts, oldest first (the
// account registry loads them all: account ids are global).
func (s *Store) AllAccounts(ctx context.Context) ([]Account, error) {
	return s.queryAccounts(ctx, `SELECT `+accountColumns+` FROM accounts WHERE deleted_at IS NULL ORDER BY created_at, id`)
}

func (s *Store) queryAccounts(ctx context.Context, query string, args ...any) ([]Account, error) {
	rows, err := s.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

const accountColumns = `id, owner_id, type, label, COALESCE(handle, ''), source, COALESCE(env_key, ''),
	enabled, created_at, updated_at, COALESCE(remote_id, ''), default_selected, public_feed,
	auth_method, config_json, deleted_at`

func scanAccount(r interface{ Scan(...any) error }) (Account, error) {
	var a Account
	var deleted sql.NullTime
	err := r.Scan(&a.ID, &a.OwnerID, &a.Type, &a.Label, &a.Handle, &a.Source, &a.EnvKey,
		&a.Enabled, &a.CreatedAt, &a.UpdatedAt, &a.RemoteID, &a.DefaultSelected, &a.PublicFeed,
		&a.AuthMethod, &a.Config, &deleted)
	if deleted.Valid {
		a.DeletedAt = deleted.Time
	}
	return a, err
}

// ErrAccountNotFound: no account of that owner has that id.
var ErrAccountNotFound = errors.New("account not found")

// ErrAccountConflict: the account changed since the caller read it.
var ErrAccountConflict = errors.New("account changed since it was read")

// Account returns one of the owner's accounts, deleted or not (see
// DeletedAt).
func (s *Store) Account(ctx context.Context, owner, id string) (Account, error) {
	a, err := scanAccount(s.sql.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM accounts WHERE owner_id = ? AND id = ?`, owner, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrAccountNotFound
	}
	return a, err
}

// UpdateAccountMeta changes what the owner set on an account. updated_at
// moves only when a value changes, so it can serve as the account's ETag.
// A change logs an account.updated event naming the fields that changed
// (never their values) and who changed them.
func (s *Store) UpdateAccountMeta(ctx context.Context, owner, id string, m AccountMeta) (Account, error) {
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return Account{}, err
	}
	defer func() { _ = tx.Rollback() }()
	cur, err := scanAccount(tx.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM accounts
		WHERE owner_id = ? AND id = ? AND deleted_at IS NULL`, owner, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrAccountNotFound
	}
	if err != nil {
		return Account{}, err
	}
	if !m.IfUpdatedAt.IsZero() && !m.IfUpdatedAt.Equal(cur.UpdatedAt) {
		return cur, ErrAccountConflict
	}
	next := cur
	if m.Label != nil {
		next.Label = *m.Label
	}
	if m.Enabled != nil {
		next.Enabled = *m.Enabled
	}
	if m.DefaultSelected != nil {
		next.DefaultSelected = *m.DefaultSelected
	}
	if m.PublicFeed != nil {
		next.PublicFeed = *m.PublicFeed
	}
	if next == cur {
		return cur, nil
	}
	next.UpdatedAt = time.Now().UTC()
	if !next.UpdatedAt.After(cur.UpdatedAt) {
		next.UpdatedAt = cur.UpdatedAt.Add(time.Microsecond)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE accounts SET label = ?, enabled = ?, default_selected = ?, public_feed = ?, updated_at = ?
		WHERE id = ?`, next.Label, next.Enabled, next.DefaultSelected, next.PublicFeed, next.UpdatedAt, id); err != nil {
		return Account{}, err
	}
	var fields []string
	for _, f := range []struct {
		name    string
		changed bool
	}{
		{"label", next.Label != cur.Label},
		{"enabled", next.Enabled != cur.Enabled},
		{"default_selected", next.DefaultSelected != cur.DefaultSelected},
		{"public_feed", next.PublicFeed != cur.PublicFeed},
	} {
		if f.changed {
			fields = append(fields, f.name)
		}
	}
	if err := emitOwner(ctx, tx, owner, EventAccountUpdated, EventData{AccountID: id, Platform: cur.Type,
		Fields: fields, Actor: m.Actor, TokenID: m.TokenID}, next.UpdatedAt); err != nil {
		return Account{}, err
	}
	if err := tx.Commit(); err != nil {
		return Account{}, err
	}
	s.ev.fire()
	return next, nil
}

// SetAccountIdentity records the handle and remote id an account's network
// reported. It is not an owner's change, so updated_at stays.
func (s *Store) SetAccountIdentity(ctx context.Context, id, handle, remoteID string) error {
	_, err := s.sql.ExecContext(ctx, `UPDATE accounts SET handle = ?, remote_id = ? WHERE id = ?`,
		nullStr(handle), nullStr(remoteID), id)
	return err
}
