package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Connecting an account by signing in at its network (spec §6.1): the
// flows under way, and the OAuth clients registered with servers.

// Secret scopes of the connect flows (ref: the flow id) and of the OAuth
// clients (ref: the client id).
const (
	SecretScopeOAuthFlow   = "oauth_flow"
	SecretScopeOAuthClient = "oauth_client"
)

// Flow statuses.
const (
	FlowPending   = "pending"
	FlowCompleted = "completed"
	FlowFailed    = "failed"
	FlowChoose    = "choose"
)

// ErrFlowNotFound: no such flow (or not the owner's).
var ErrFlowNotFound = errors.New("connect flow not found")

// ErrFlowClaimed: the flow's callback already came (or it expired).
var ErrFlowClaimed = errors.New("connect flow already used or expired")

// OAuthFlow is a connect flow. AccountID is the account a reconnect is
// for ("" for a new account).
type OAuthFlow struct {
	ID              string
	OwnerID         string
	UserID          string
	Type            string
	Method          string
	AccountID       string
	Label           string
	RedirectURI     string
	Status          string
	ResultAccountID string
	ErrorCode       string
	CreatedAt       time.Time
	ExpiresAt       time.Time
	UsedAt          *time.Time
}

// NewFlowID returns a fresh flow id; its secrets are bound to it.
func NewFlowID() string { return "flw_" + newID() }

const oauthFlowColumns = `id, owner_id, user_id, type, method, COALESCE(account_id, ''), label, redirect_uri, status,
	COALESCE(result_account_id, ''), error_code, created_at, expires_at, used_at`

func scanOAuthFlow(row interface{ Scan(...any) error }) (OAuthFlow, error) {
	var f OAuthFlow
	var used sql.NullTime
	err := row.Scan(&f.ID, &f.OwnerID, &f.UserID, &f.Type, &f.Method, &f.AccountID, &f.Label, &f.RedirectURI, &f.Status,
		&f.ResultAccountID, &f.ErrorCode, &f.CreatedAt, &f.ExpiresAt, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthFlow{}, ErrFlowNotFound
	}
	if used.Valid {
		t := used.Time
		f.UsedAt = &t
	}
	return f, err
}

// checkBound refuses secrets not bound to (owner, scope, ref).
func checkBound(owner, scope, ref string, ss []SealedSecret) error {
	for _, s := range ss {
		switch {
		case s.Owner != owner || s.Scope != scope || s.Ref != ref:
			return fmt.Errorf("secret %s is bound to %s/%s/%s, not %s/%s/%s", s.Name, s.Owner, s.Scope, s.Ref, owner, scope, ref)
		case s.Name == "" || s.Sealed == "" || s.KeyID == "":
			return errors.New("a secret needs a name, a sealed value and its key id")
		case !strings.HasPrefix(s.Sealed, "v1."+s.KeyID+"."):
			return fmt.Errorf("secret %s isn't sealed under key %s", s.Name, s.KeyID)
		}
	}
	return nil
}

// CreateOAuthFlow stores a new flow (Status pending) found by stateHash,
// with its sealed data, in one transaction.
func (s *Store) CreateOAuthFlow(ctx context.Context, f OAuthFlow, stateHash string, data []SealedSecret) error {
	if f.ID == "" || f.OwnerID == "" || f.UserID == "" || stateHash == "" || f.RedirectURI == "" {
		return errors.New("a flow needs an id, an owner, a user, a state and a redirect URI")
	}
	if err := checkBound(f.OwnerID, SecretScopeOAuthFlow, f.ID, data); err != nil {
		return err
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO oauth_flows (id, owner_id, user_id, type, method, account_id, label,
		redirect_uri, state_hash, status, created_at, expires_at) VALUES (?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?)`,
		f.ID, f.OwnerID, f.UserID, f.Type, f.Method, f.AccountID, f.Label, f.RedirectURI, stateHash, FlowPending,
		now, f.ExpiresAt.UTC()); err != nil {
		return err
	}
	if err := insertSecrets(ctx, tx, data, now); err != nil {
		return err
	}
	return tx.Commit()
}

// OAuthFlow returns one of the owner's flows.
func (s *Store) OAuthFlow(ctx context.Context, owner, id string) (OAuthFlow, error) {
	return scanOAuthFlow(s.sql.QueryRowContext(ctx, `SELECT `+oauthFlowColumns+` FROM oauth_flows WHERE owner_id = ? AND id = ?`, owner, id))
}

// OAuthFlowByState returns the flow a callback's state names (by its
// hash), whoever's it is.
func (s *Store) OAuthFlowByState(ctx context.Context, stateHash string) (OAuthFlow, error) {
	return scanOAuthFlow(s.sql.QueryRowContext(ctx, `SELECT `+oauthFlowColumns+` FROM oauth_flows WHERE state_hash = ?`, stateHash))
}

// ClaimOAuthFlow marks a pending flow's callback as come, once: two
// callbacks racing get one claim. ErrFlowClaimed when it was claimed
// before or has expired.
func (s *Store) ClaimOAuthFlow(ctx context.Context, id string, now time.Time) error {
	res, err := s.sql.ExecContext(ctx, `UPDATE oauth_flows SET used_at = ?
		WHERE id = ? AND used_at IS NULL AND status = ? AND expires_at > ?`, now.UTC(), id, FlowPending, now.UTC())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrFlowClaimed
	}
	return nil
}

// FlowOutcome is how a flow ended (or that it waits for a choice).
type FlowOutcome struct {
	Status    string
	AccountID string
	ErrorCode string
	// Put replaces the flow's sealed data (the candidates of a choice);
	// Drop deletes the named ones.
	Put  []SealedSecret
	Drop []string
}

// FinishOAuthFlow records a flow's outcome. A flow that has ended
// (completed or failed) keeps its outcome; its sealed data is deleted then.
func (s *Store) FinishOAuthFlow(ctx context.Context, owner, id string, o FlowOutcome) error {
	switch o.Status {
	case FlowCompleted, FlowFailed, FlowChoose:
	default:
		return fmt.Errorf("not a flow outcome: %q", o.Status)
	}
	if err := checkBound(owner, SecretScopeOAuthFlow, id, o.Put); err != nil {
		return err
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE oauth_flows SET status = ?, result_account_id = NULLIF(?, ''), error_code = ?
		WHERE owner_id = ? AND id = ? AND status IN (?, ?)`, o.Status, o.AccountID, o.ErrorCode, owner, id, FlowPending, FlowChoose)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrFlowClaimed
	}
	now := time.Now().UTC()
	if o.Status != FlowChoose {
		if _, err := tx.ExecContext(ctx, `DELETE FROM secrets WHERE owner_id = ? AND scope = ? AND ref = ?`,
			owner, SecretScopeOAuthFlow, id); err != nil {
			return err
		}
	} else {
		for _, name := range o.Drop {
			if _, err := tx.ExecContext(ctx, `DELETE FROM secrets WHERE owner_id = ? AND scope = ? AND ref = ? AND name = ?`,
				owner, SecretScopeOAuthFlow, id, name); err != nil {
				return err
			}
		}
		if err := insertSecrets(ctx, tx, o.Put, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PurgeOAuthFlows deletes the flows that expired before cutoff, with
// their sealed data. Returns how many went.
func (s *Store) PurgeOAuthFlows(ctx context.Context, cutoff time.Time) (int, error) {
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM secrets WHERE scope = ? AND ref IN (SELECT id FROM oauth_flows WHERE expires_at < ?)`,
		SecretScopeOAuthFlow, cutoff.UTC()); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM oauth_flows WHERE expires_at < ?`, cutoff.UTC())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), tx.Commit()
}

// OAuthClient is a client Publisher registered with a server (issuer)
// for a redirect URI; its secret is in the secret store.
type OAuthClient struct {
	ID          string
	OwnerID     string
	Type        string
	Issuer      string
	RedirectURI string
	ClientID    string
	Scopes      string
	CreatedAt   time.Time
}

// ErrOAuthClientNotFound: no client registered for that server and
// redirect URI.
var ErrOAuthClientNotFound = errors.New("no OAuth client registered")

// NewOAuthClientID returns a fresh client row id; its secret is bound to it.
func NewOAuthClientID() string { return "occ_" + newID() }

// OAuthClientFor returns the client registered with issuer for
// redirectURI.
func (s *Store) OAuthClientFor(ctx context.Context, owner, typ, issuer, redirectURI string) (OAuthClient, error) {
	var c OAuthClient
	err := s.sql.QueryRowContext(ctx, `SELECT id, owner_id, type, issuer, redirect_uri, client_id, scopes, created_at
		FROM oauth_clients WHERE owner_id = ? AND type = ? AND issuer = ? AND redirect_uri = ?`, owner, typ, issuer, redirectURI).
		Scan(&c.ID, &c.OwnerID, &c.Type, &c.Issuer, &c.RedirectURI, &c.ClientID, &c.Scopes, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthClient{}, ErrOAuthClientNotFound
	}
	return c, err
}

// SaveOAuthClient stores a client (replacing the one registered for the
// same server and redirect URI, and its secret) with its sealed secrets.
func (s *Store) SaveOAuthClient(ctx context.Context, c OAuthClient, secrets []SealedSecret) error {
	if c.ID == "" || c.OwnerID == "" || c.Issuer == "" || c.RedirectURI == "" || c.ClientID == "" {
		return errors.New("a client needs an id, an owner, an issuer, a redirect URI and a client id")
	}
	if err := checkBound(c.OwnerID, SecretScopeOAuthClient, c.ID, secrets); err != nil {
		return err
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM secrets WHERE owner_id = ? AND scope = ? AND ref IN
		(SELECT id FROM oauth_clients WHERE owner_id = ? AND type = ? AND issuer = ? AND redirect_uri = ?)`,
		c.OwnerID, SecretScopeOAuthClient, c.OwnerID, c.Type, c.Issuer, c.RedirectURI); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_clients WHERE owner_id = ? AND type = ? AND issuer = ? AND redirect_uri = ?`,
		c.OwnerID, c.Type, c.Issuer, c.RedirectURI); err != nil {
		return err
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO oauth_clients (id, owner_id, type, issuer, redirect_uri, client_id, scopes, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, c.ID, c.OwnerID, c.Type, c.Issuer, c.RedirectURI, c.ClientID, c.Scopes, now); err != nil {
		return err
	}
	if err := insertSecrets(ctx, tx, secrets, now); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteOAuthClient forgets a client and its secret (a server that no
// longer knows it).
func (s *Store) DeleteOAuthClient(ctx context.Context, owner, id string) error {
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM secrets WHERE owner_id = ? AND scope = ? AND ref = ?`, owner, SecretScopeOAuthClient, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_clients WHERE owner_id = ? AND id = ?`, owner, id); err != nil {
		return err
	}
	return tx.Commit()
}
