package drivers

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/geofox/publisher/internal/destination"
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

// ErrStateUnsupported: the account can't keep state before part C.
var ErrStateUnsupported = errors.New("this account can't save state in this version")

func (l LegacyState) now() time.Time {
	if l.Now != nil {
		return l.Now().UTC()
	}
	return time.Now().UTC()
}

func legacyThreads(row store.Account) bool {
	return row.Type == threads.Type && row.Source == "env"
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
func (l LegacyState) Save(_ context.Context, row store.Account, u destination.StateUpdate) error {
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
