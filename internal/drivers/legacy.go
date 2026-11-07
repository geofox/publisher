package drivers

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/nostr"
	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/threads"
)

// ThreadsTokenStore is where 2.x kept the Threads token's state (the
// threads_token row).
type ThreadsTokenStore interface {
	GetThreadsToken() (*store.ThreadsToken, error)
	SaveThreadsToken(token string, expiresAt time.Time, seedHash string, refreshedAt time.Time) error
}

// LegacyState keeps the state of accounts from the environment where 2.x
// kept it, until part C stores state per account: a Threads account's
// token, expiry and refresh time stay in the threads_token row, so a
// downgrade still finds them. It is the one place that names the Threads
// type for this. Other types have no state to keep yet.
type LegacyState struct {
	Store ThreadsTokenStore
	Now   func() time.Time // nil: time.Now
}

// ErrStateUnsupported: without a master key, only 2.x's Threads account
// keeps state.
var ErrStateUnsupported = destination.ErrStateUnsupported

func (l LegacyState) now() time.Time {
	if l.Now != nil {
		return l.Now().UTC()
	}
	return time.Now().UTC()
}

// legacyThreads reports whether row is the Threads account 2.x kept its
// token for: the operator's, from the unnumbered variables. There is one
// saved token, so no other account may read or write it (another would
// publish with that account's token, or overwrite it).
func legacyThreads(row store.Account) bool {
	return row.Type == threads.Type && row.Source == "env" &&
		row.EnvKey == destination.EnvKeyDefault && row.OwnerID == store.DefaultOwner
}

// Restore returns the state the account opens with. For the Threads
// account: the saved token and its times while the environment's token
// is the one they came from (its hash matches); otherwise the environment's
// token is adopted with a provisional expiry (11 days, so the first
// refresh a day later learns the real one), and saved at once so a restart
// doesn't move the schedule. A failure to read or save is an error: the
// account stays shut rather than overwrite a refreshed token.
func (l LegacyState) Restore(_ context.Context, row store.Account, secrets map[string]string, config map[string]any) (map[string]string, map[string]any, error) {
	if !legacyThreads(row) {
		return secrets, config, nil
	}
	env := secrets[threads.SecretAccessToken]
	hash := threads.SeedHash(env)
	cur, err := l.Store.GetThreadsToken()
	if err != nil {
		return nil, nil, fmt.Errorf("reading the saved Threads token: %w", err)
	}
	if cur == nil || cur.SeedHash != hash {
		now := l.now()
		cur = &store.ThreadsToken{Token: env, ExpiresAt: now.Add(threads.SeedProvisional), SeedHash: hash, RefreshedAt: now}
		if err := l.Store.SaveThreadsToken(cur.Token, cur.ExpiresAt, cur.SeedHash, cur.RefreshedAt); err != nil {
			return nil, nil, fmt.Errorf("saving the Threads token from the environment: %w", err)
		}
	}
	outS := maps.Clone(secrets)
	outS[threads.SecretAccessToken] = cur.Token
	outC := maps.Clone(config)
	if outC == nil {
		outC = map[string]any{}
	}
	outC[threads.ConfigExpiresAt] = cur.ExpiresAt.UTC().Format(time.RFC3339Nano)
	outC[threads.ConfigRefreshedAt] = cur.RefreshedAt.UTC().Format(time.RFC3339Nano)
	return outS, outC, nil
}

// Save writes a Threads account's token and times in one upsert, keeping
// the seed hash (the environment's token they descend from). Values the
// update leaves out keep their saved ones.
func (l LegacyState) Save(_ context.Context, row store.Account, _ string, u destination.StateUpdate) error {
	if !legacyThreads(row) {
		return ErrStateUnsupported
	}
	cur, err := l.Store.GetThreadsToken()
	if err != nil {
		return err
	}
	if cur == nil {
		return errors.New("no saved Threads token to update")
	}
	next := *cur
	if tok, ok := u.Secrets[threads.SecretAccessToken]; ok {
		if tok == "" {
			return errors.New("a Threads account can't drop its token")
		}
		next.Token = tok
	}
	for key, dst := range map[string]*time.Time{threads.ConfigExpiresAt: &next.ExpiresAt, threads.ConfigRefreshedAt: &next.RefreshedAt} {
		t, ok, err := destination.ConfigTime(u.Config, key)
		if err != nil {
			return destination.InvalidField(key, err.Error())
		}
		if ok {
			*dst = t
		}
	}
	return l.Store.SaveThreadsToken(next.Token, next.ExpiresAt, next.SeedHash, next.RefreshedAt)
}

// LegacyStore is where 2.x kept state of "the" account of a type that now
// belongs to one account.
type LegacyStore interface {
	AdoptLegacySyncRelays(ctx context.Context, accountID string) (bool, error)
	SeedAccountSyncRelaysOnce(ctx context.Context, accountID string, urls []string) error
}

// AdoptLegacy gives state 2.x kept for the one account of a type to the
// env account that owns it now: the relay-sync list goes to the Nostr
// account of the unnumbered variables (2.x's one account; else the first
// env Nostr account) when that account never had one. Then each env
// Nostr account that never had a list starts with its SYNC_RELAYS. rows
// are the env rows (SeedEnvAccounts); env says which rows the environment
// configures. Idempotent: a list the owner changed, even emptied, stays.
func AdoptLegacy(ctx context.Context, st LegacyStore, rows []store.Account, env []EnvAccount) error {
	var errs []error
	heir := ""
	for _, row := range rows {
		if row.Type == nostr.Type && row.Source == "env" && (heir == "" || row.EnvKey == destination.EnvKeyDefault) {
			heir = row.ID
			if row.EnvKey == destination.EnvKeyDefault {
				break
			}
		}
	}
	if heir != "" {
		if _, err := st.AdoptLegacySyncRelays(ctx, heir); err != nil {
			errs = append(errs, fmt.Errorf("relay-sync list: %w", err))
		}
	}
	for _, row := range rows {
		if row.Type != nostr.Type || row.Source != "env" {
			continue
		}
		for _, e := range env {
			if e.Type != row.Type || e.Key != row.EnvKey {
				continue
			}
			seed, _ := destination.ConfigStrings(e.Config, nostr.ConfigSyncRelaysSeed)
			if err := st.SeedAccountSyncRelaysOnce(ctx, row.ID, seed); err != nil {
				errs = append(errs, fmt.Errorf("relay-sync list: %w", err))
			}
		}
	}
	return errors.Join(errs...)
}

// ThreadsMoveStore is what moving 2.x's Threads token needs.
type ThreadsMoveStore interface {
	GetThreadsToken() (*store.ThreadsToken, error)
	MoveThreadsToken(ctx context.Context, owner, id string, patch map[string]any, put []store.SealedSecret) error
	Checkpoint(ctx context.Context) error
}

// StatePreparer seals state for an account from the environment without
// writing it (accounts.SealedState.Prepare).
type StatePreparer func(row store.Account, env map[string]string, config map[string]any, u destination.StateUpdate) ([]store.SealedSecret, map[string]any, error)

// MoveThreadsToken moves the Threads token 2.x kept in plaintext (the
// threads_token row) into the sealed state of the account it belongs to,
// once a master key is set: while the environment's token is the one it
// descends from, it becomes that account's saved state; otherwise it
// belongs to no account and is only removed. Then the write-ahead log is
// checkpointed, so no plaintext copy stays in it. It reports where the
// token went: the account it moved to ("" when it was only removed), and
// done when the old row is gone (an error with done set is the checkpoint
// only). rows are the env rows, env the accounts the environment
// configures.
func MoveThreadsToken(ctx context.Context, st ThreadsMoveStore, prepare StatePreparer, rows []store.Account, env []EnvAccount) (to string, done bool, err error) {
	tok, err := st.GetThreadsToken()
	if err != nil || tok == nil {
		return "", tok == nil && err == nil, err
	}
	var owner, id string
	var put []store.SealedSecret
	var patch map[string]any
	for _, row := range rows {
		if !legacyThreads(row) {
			continue
		}
		for _, e := range env {
			if e.Type != threads.Type || e.Key != destination.EnvKeyDefault || threads.SeedHash(e.Secrets[threads.SecretAccessToken]) != tok.SeedHash {
				continue
			}
			put, patch, err = prepare(row, e.Secrets, e.Config, destination.StateUpdate{
				Secrets: map[string]string{threads.SecretAccessToken: tok.Token},
				Config: map[string]any{
					threads.ConfigExpiresAt:   tok.ExpiresAt.UTC().Format(time.RFC3339Nano),
					threads.ConfigRefreshedAt: tok.RefreshedAt.UTC().Format(time.RFC3339Nano),
				},
			})
			if err != nil {
				return "", false, fmt.Errorf("sealing the Threads token: %w", err)
			}
			owner, id = row.OwnerID, row.ID
		}
	}
	if err := st.MoveThreadsToken(ctx, owner, id, patch, put); err != nil {
		return "", false, err
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := st.Checkpoint(cctx); err != nil {
		return id, true, fmt.Errorf("the token moved, but the write-ahead log may hold a copy until the next checkpoint: %w", err)
	}
	return id, true, nil
}
