package store

import (
	"context"
	"database/sql"
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
	rows, err := s.sql.QueryContext(ctx, `SELECT id, owner_id, type, label, COALESCE(handle, ''), source, COALESCE(env_key, ''),
		enabled, created_at, updated_at FROM accounts WHERE owner_id = ? ORDER BY created_at, id`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.OwnerID, &a.Type, &a.Label, &a.Handle, &a.Source, &a.EnvKey,
			&a.Enabled, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
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
