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
}

// AccountMeta is what the owner can change on an account; a nil field is
// left as it is.
type AccountMeta struct {
	Label           *string
	Enabled         *bool
	DefaultSelected *bool
	PublicFeed      *bool
}

// EnvKeyDefault is the env_key of the account seeded from a type's
// unnumbered env vars (the only kind until step 4).
const EnvKeyDefault = "default"

func newAccountID() string { return "acc_" + newID() }

// EnvAccount is one account the environment configures.
type EnvAccount struct {
	Type  string
	Label string // the destination's display name
}

// SeedEnvAccounts makes the owner's env-sourced accounts match the
// environment: one enabled account per configured type (created on first
// sight, with an id that then never changes), and every other env account
// disabled. Disabled accounts keep their history. Returns the accounts by
// type.
func (s *Store) SeedEnvAccounts(ctx context.Context, owner string, configured []EnvAccount) (map[string]Account, error) {
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE accounts SET enabled = 0, updated_at = ?
		WHERE owner_id = ? AND source = 'env' AND enabled = 1`, now, owner); err != nil {
		return nil, err
	}
	for _, c := range configured {
		if _, err := tx.ExecContext(ctx, `INSERT INTO accounts (id, owner_id, type, label, source, env_key, enabled, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'env', ?, 1, ?, ?)
			ON CONFLICT (owner_id, type, env_key) DO UPDATE SET enabled = 1, label = excluded.label, updated_at = excluded.updated_at`,
			newAccountID(), owner, c.Type, c.Label, EnvKeyDefault, now, now); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	all, err := s.Accounts(ctx, owner)
	if err != nil {
		return nil, err
	}
	out := map[string]Account{}
	for _, a := range all {
		if a.Enabled && a.Source == "env" && a.EnvKey == EnvKeyDefault {
			out[a.Type] = a
		}
	}
	return out, nil
}

// Accounts lists the owner's accounts, enabled or not, oldest first.
func (s *Store) Accounts(ctx context.Context, owner string) ([]Account, error) {
	rows, err := s.sql.QueryContext(ctx, `SELECT `+accountColumns+` FROM accounts WHERE owner_id = ? ORDER BY created_at, id`, owner)
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
	enabled, created_at, updated_at, COALESCE(remote_id, ''), default_selected, public_feed`

func scanAccount(r interface{ Scan(...any) error }) (Account, error) {
	var a Account
	err := r.Scan(&a.ID, &a.OwnerID, &a.Type, &a.Label, &a.Handle, &a.Source, &a.EnvKey,
		&a.Enabled, &a.CreatedAt, &a.UpdatedAt, &a.RemoteID, &a.DefaultSelected, &a.PublicFeed)
	return a, err
}

// ErrAccountNotFound: no account of that owner has that id.
var ErrAccountNotFound = errors.New("account not found")

// Account returns one of the owner's accounts.
func (s *Store) Account(ctx context.Context, owner, id string) (Account, error) {
	a, err := scanAccount(s.sql.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM accounts WHERE owner_id = ? AND id = ?`, owner, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrAccountNotFound
	}
	return a, err
}

// UpdateAccountMeta changes what the owner set on an account. updated_at
// moves only when a value changes, so it can serve as the account's ETag.
func (s *Store) UpdateAccountMeta(ctx context.Context, owner, id string, m AccountMeta) (Account, error) {
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return Account{}, err
	}
	defer func() { _ = tx.Rollback() }()
	cur, err := scanAccount(tx.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM accounts WHERE owner_id = ? AND id = ?`, owner, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrAccountNotFound
	}
	if err != nil {
		return Account{}, err
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
	return next, tx.Commit()
}

// SetAccountIdentity records the handle and remote id an account's network
// reported. It is not an owner's change, so updated_at stays.
func (s *Store) SetAccountIdentity(ctx context.Context, id, handle, remoteID string) error {
	_, err := s.sql.ExecContext(ctx, `UPDATE accounts SET handle = ?, remote_id = ? WHERE id = ?`,
		nullStr(handle), nullStr(remoteID), id)
	return err
}

// envAccountFor returns the id of the owner's env account of type typ,
// creating it (disabled: nothing configured it) when history needs one, e.g.
// a post that names a destination this deployment has no credentials for.
func envAccountFor(ctx context.Context, tx *sql.Tx, owner, typ string) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM accounts WHERE owner_id = ? AND type = ? AND source = 'env' AND env_key = ?`,
		owner, typ, EnvKeyDefault).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	now := time.Now().UTC()
	id = newAccountID()
	_, err = tx.ExecContext(ctx, `INSERT INTO accounts (id, owner_id, type, label, source, env_key, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'env', ?, 0, ?, ?)`, id, owner, typ, typ, EnvKeyDefault, now, now)
	return id, err
}
