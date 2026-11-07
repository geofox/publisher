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

// SettingsInstance is the owner id under which the server's own settings
// (the media store, a network's app) are kept.
const SettingsInstance = "instance"

// SecretScopeSetting is the scope of a secret setting in the secret store
// (ref: the setting's key, name: "value").
const SecretScopeSetting = "setting"

// SettingRow is one saved, non-secret setting.
type SettingRow struct {
	Key       string
	ValueJSON string
	UpdatedAt time.Time
	UpdatedBy string
}

// Settings returns an owner's (or the instance's) saved settings and
// their secret settings (sealed), by key.
func (s *Store) Settings(ctx context.Context, owner string) ([]SettingRow, []SealedSecret, error) {
	rows, err := s.sql.QueryContext(ctx, `SELECT key, value_json, updated_at, COALESCE(updated_by, '')
		FROM settings WHERE owner_id = ? ORDER BY key`, owner)
	if err != nil {
		return nil, nil, err
	}
	var out []SettingRow
	for rows.Next() {
		var r SettingRow
		if err := rows.Scan(&r.Key, &r.ValueJSON, &r.UpdatedAt, &r.UpdatedBy); err != nil {
			rows.Close()
			return nil, nil, err
		}
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	srows, err := s.sql.QueryContext(ctx, `SELECT owner_id, scope, ref, name, audience, sealed, key_id, created_at, updated_at
		FROM secrets WHERE owner_id = ? AND scope = ? ORDER BY ref, name`, owner, SecretScopeSetting)
	if err != nil {
		return nil, nil, err
	}
	sec, err := scanSecrets(srows)
	return out, sec, err
}

// SettingsChange is one write of an owner's (or the instance's)
// settings, applied in one transaction.
type SettingsChange struct {
	Owner string
	// Put saves values (JSON) by key; Clear removes saved values.
	Put   map[string]string
	Clear []string
	// PutSecrets saves sealed secret settings (Scope SecretScopeSetting,
	// Ref the key); ClearSecrets removes them by key.
	PutSecrets   []SealedSecret
	ClearSecrets []string
	// By says who changed them ("session", "token:<id>", "proxy").
	By string
}

// ApplySettings writes a settings change in one transaction.
func (s *Store) ApplySettings(ctx context.Context, c SettingsChange) error {
	if c.Owner == "" {
		return errors.New("settings: no owner")
	}
	for _, x := range c.PutSecrets {
		if x.Owner != c.Owner || x.Scope != SecretScopeSetting || x.Ref == "" || x.Name == "" || x.Sealed == "" || x.KeyID == "" {
			return fmt.Errorf("settings: secret %q isn't a sealed setting of %s", x.Ref, c.Owner)
		}
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC()
	for k, v := range c.Put {
		if !json.Valid([]byte(v)) {
			return fmt.Errorf("settings: %s isn't JSON", k)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings (owner_id, key, value_json, updated_at, updated_by)
			VALUES (?, ?, ?, ?, ?) ON CONFLICT (owner_id, key) DO UPDATE SET value_json = excluded.value_json,
			updated_at = excluded.updated_at, updated_by = excluded.updated_by`, c.Owner, k, v, now, nullIfEmpty(c.By)); err != nil {
			return err
		}
	}
	for _, k := range c.Clear {
		if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE owner_id = ? AND key = ?`, c.Owner, k); err != nil {
			return err
		}
	}
	for _, k := range c.ClearSecrets {
		if _, err := tx.ExecContext(ctx, `DELETE FROM secrets WHERE owner_id = ? AND scope = ? AND ref = ?`,
			c.Owner, SecretScopeSetting, k); err != nil {
			return err
		}
	}
	for _, x := range c.PutSecrets {
		if _, err := tx.ExecContext(ctx, `INSERT INTO secrets (owner_id, scope, ref, name, audience, sealed, key_id, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (owner_id, scope, ref, name) DO UPDATE SET audience = excluded.audience,
			sealed = excluded.sealed, key_id = excluded.key_id, updated_at = excluded.updated_at`,
			x.Owner, x.Scope, x.Ref, x.Name, x.Audience, x.Sealed, x.KeyID, now, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// AccountSet is a named selection of an owner's accounts the composer
// can start from; at most one is the default.
type AccountSet struct {
	ID        string    `json:"id"`
	Owner     string    `json:"-"`
	Name      string    `json:"name"`
	Accounts  []string  `json:"accounts"`
	IsDefault bool      `json:"is_default"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Account set errors.
var (
	ErrAccountSetNotFound = errors.New("account set not found")
	// ErrAccountSetName: the owner already has a set of that name.
	ErrAccountSetName = errors.New("an account set of that name already exists")
	// ErrAccountSetsFull: the owner has MaxAccountSets sets.
	ErrAccountSetsFull = errors.New("at most 50 account sets; delete one first")
	// ErrAccountSetConflict: the set changed since the caller read it.
	ErrAccountSetConflict = errors.New("account set changed since it was read")
)

const accountSetColumns = `id, owner_id, name, accounts, is_default, created_at, updated_at`

func scanAccountSet(r interface{ Scan(...any) error }) (AccountSet, error) {
	var a AccountSet
	var accts string
	if err := r.Scan(&a.ID, &a.Owner, &a.Name, &accts, &a.IsDefault, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return a, err
	}
	if err := json.Unmarshal([]byte(accts), &a.Accounts); err != nil {
		return a, fmt.Errorf("account set %s: %w", a.ID, err)
	}
	if a.Accounts == nil {
		a.Accounts = []string{}
	}
	return a, nil
}

// AccountSets lists an owner's account sets by name.
func (s *Store) AccountSets(ctx context.Context, owner string) ([]AccountSet, error) {
	rows, err := s.sql.QueryContext(ctx, `SELECT `+accountSetColumns+` FROM account_sets WHERE owner_id = ? ORDER BY name COLLATE NOCASE, id`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AccountSet{}
	for rows.Next() {
		a, err := scanAccountSet(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AccountSet returns one of the owner's account sets.
func (s *Store) AccountSet(ctx context.Context, owner, id string) (AccountSet, error) {
	a, err := scanAccountSet(s.sql.QueryRowContext(ctx, `SELECT `+accountSetColumns+` FROM account_sets WHERE owner_id = ? AND id = ?`, owner, id))
	if errors.Is(err, sql.ErrNoRows) {
		return AccountSet{}, ErrAccountSetNotFound
	}
	return a, err
}

// AccountSetChange is a new set, or a change to one (nil: left as is).
type AccountSetChange struct {
	Name      *string
	Accounts  *[]string
	IsDefault *bool
	// IfUpdatedAt, when set, refuses a change (ErrAccountSetConflict)
	// unless the set's updated_at is still this.
	IfUpdatedAt time.Time
}

// MaxAccountSets is how many account sets an owner may have.
const MaxAccountSets = 50

// CreateAccountSet stores a new account set; making it the default
// clears the owner's other default.
func (s *Store) CreateAccountSet(ctx context.Context, owner string, c AccountSetChange) (AccountSet, error) {
	if c.Name == nil || c.Accounts == nil {
		return AccountSet{}, errors.New("account set: name and accounts are required")
	}
	a := AccountSet{ID: "set_" + newID(), Owner: owner, Name: *c.Name, Accounts: *c.Accounts,
		IsDefault: c.IsDefault != nil && *c.IsDefault, CreatedAt: time.Now().UTC()}
	a.UpdatedAt = a.CreatedAt
	err := s.writeAccountSet(ctx, a, true)
	return a, err
}

// UpdateAccountSet changes an account set.
func (s *Store) UpdateAccountSet(ctx context.Context, owner, id string, c AccountSetChange) (AccountSet, error) {
	cur, err := s.AccountSet(ctx, owner, id)
	if err != nil {
		return AccountSet{}, err
	}
	if !c.IfUpdatedAt.IsZero() && !c.IfUpdatedAt.Equal(cur.UpdatedAt) {
		return cur, ErrAccountSetConflict
	}
	next := cur
	if c.Name != nil {
		next.Name = *c.Name
	}
	if c.Accounts != nil {
		next.Accounts = *c.Accounts
	}
	if c.IsDefault != nil {
		next.IsDefault = *c.IsDefault
	}
	next.UpdatedAt = nextUpdatedAt(cur.UpdatedAt)
	return next, s.writeAccountSet(ctx, next, false, cur.UpdatedAt)
}

// writeAccountSet inserts a set, or updates it while its updated_at is
// still was (a concurrent change wins once).
func (s *Store) writeAccountSet(ctx context.Context, a AccountSet, create bool, was ...time.Time) error {
	accts, err := json.Marshal(a.Accounts)
	if err != nil {
		return err
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Names differ in more than case, and the cap holds, checked in the
	// write's own transaction (BEGIN IMMEDIATE), so two at once can't
	// both pass.
	rows, err := tx.QueryContext(ctx, `SELECT id, name FROM account_sets WHERE owner_id = ?`, a.Owner)
	if err != nil {
		return err
	}
	others := 0
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return err
		}
		if id == a.ID {
			continue
		}
		others++
		if strings.EqualFold(name, a.Name) {
			rows.Close()
			return ErrAccountSetName
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if create && others >= MaxAccountSets {
		return ErrAccountSetsFull
	}
	if a.IsDefault {
		// The set that was the default changes too (its ETag with it).
		var oldID string
		var oldAt time.Time
		err := tx.QueryRowContext(ctx, `SELECT id, updated_at FROM account_sets WHERE owner_id = ? AND id <> ? AND is_default = 1`,
			a.Owner, a.ID).Scan(&oldID, &oldAt)
		switch {
		case err == nil:
			if _, err := tx.ExecContext(ctx, `UPDATE account_sets SET is_default = 0, updated_at = ? WHERE owner_id = ? AND id = ?`,
				nextUpdatedAt(oldAt), a.Owner, oldID); err != nil {
				return err
			}
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
	}
	if create {
		_, err = tx.ExecContext(ctx, `INSERT INTO account_sets (`+accountSetColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			a.ID, a.Owner, a.Name, string(accts), a.IsDefault, a.CreatedAt, a.UpdatedAt)
	} else {
		var res sql.Result
		res, err = tx.ExecContext(ctx, `UPDATE account_sets SET name = ?, accounts = ?, is_default = ?, updated_at = ?
			WHERE owner_id = ? AND id = ? AND updated_at = ?`, a.Name, string(accts), a.IsDefault, a.UpdatedAt, a.Owner, a.ID, was[0])
		if err == nil {
			if n, _ := res.RowsAffected(); n == 0 {
				return ErrAccountSetConflict
			}
		}
	}
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: account_sets.owner_id, account_sets.name") {
			return ErrAccountSetName
		}
		return err
	}
	return tx.Commit()
}

// DeleteAccountSet removes an account set.
func (s *Store) DeleteAccountSet(ctx context.Context, owner, id string) error {
	res, err := s.sql.ExecContext(ctx, `DELETE FROM account_sets WHERE owner_id = ? AND id = ?`, owner, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrAccountSetNotFound
	}
	return nil
}
