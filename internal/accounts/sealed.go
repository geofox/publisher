package accounts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/secrets"
	"github.com/geofox/publisher/internal/store"
)

// StateSeedSecret names the sealed secret that says which environment
// credentials an account's saved state descends from.
const StateSeedSecret = "_state_seed"

// EnvStateSeed identifies the credentials the environment gives an
// account, and the server they go to: saved state (a token refreshed from
// the environment's) applies only while they are the same. It is stored
// sealed, so the database alone can't confirm a guessed credential.
func EnvStateSeed(env map[string]string, audience string) string {
	h := sha256.New()
	field := func(s string) { fmt.Fprintf(h, "%d:%s", len(s), s) } // length-prefixed: no two lists hash alike
	field("publisher env state seed v1")
	field(audience)
	for _, k := range slices.Sorted(maps.Keys(env)) {
		field(k)
		field(env[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// SealedState is the StateStore when a master key is set: the state an
// account from the environment saves (a refreshed Threads token) is kept
// sealed beside it, its secrets in the secret store and the rest in its
// row's configuration. State saved for other environment credentials (the
// operator set a new token) or another server is forgotten.
//
// Nothing is written for an account until it saves state: the seed of its
// environment credentials is kept in memory until then.
type SealedState struct {
	Store    SecretStore
	Keys     secrets.Keyring
	KeyState KeyState

	seeds sync.Map // account id → its EnvStateSeed
}

var _ StateStore = (*SealedState)(nil)

// errStateLocked: the saved state can't be opened with this key.
var errStateLocked = errors.New("its saved state was sealed with a master key this server doesn't have")

// Restore implements StateStore.
func (s *SealedState) Restore(ctx context.Context, row store.Account, env map[string]string, config map[string]any) (map[string]string, map[string]any, error) {
	d, ok := destination.Lookup(row.Type)
	if !ok {
		return env, config, nil
	}
	audience := destination.AudienceOf(d.Manifest(), config)
	seed := EnvStateSeed(env, audience)
	s.seeds.Store(row.ID, seed)
	savedCfg := map[string]any{}
	if row.Config != "" {
		if err := json.Unmarshal([]byte(row.Config), &savedCfg); err != nil {
			return nil, nil, fmt.Errorf("its saved state: %w", err)
		}
	}
	sealed, err := s.Store.Secrets(ctx, row.OwnerID, store.SecretScopeAccount, row.ID)
	if err != nil {
		return nil, nil, err
	}
	saved := map[string]string{}
	stale := false
	for _, x := range sealed {
		b := Binding(x)
		b.Audience = audience
		v, err := s.Keys.Open(b, x.Sealed)
		switch {
		case err == nil:
			saved[x.Name] = v
		case errors.Is(err, secrets.ErrNoKey) || errors.Is(err, secrets.ErrUnknownKey):
			return nil, nil, errStateLocked
		default:
			stale = true // sealed for another server (the operator changed it), or damaged
		}
	}
	if !stale && saved[StateSeedSecret] == seed {
		outS, outC := maps.Clone(env), maps.Clone(config)
		if outC == nil {
			outC = map[string]any{}
		}
		for k, v := range saved {
			if k != StateSeedSecret {
				outS[k] = v
			}
		}
		maps.Copy(outC, savedCfg)
		return outS, outC, nil
	}
	// Not from these credentials (or nothing saved): start from the
	// environment's, and forget what was saved for others. Without a
	// usable key nothing is deleted.
	if len(sealed) == 0 && len(savedCfg) == 0 || s.KeyState.WriteError() != nil {
		return env, config, nil
	}
	patch := map[string]any{}
	for k := range savedCfg {
		patch[k] = nil
	}
	del := make([]string, 0, len(sealed))
	for _, x := range sealed {
		del = append(del, x.Name)
	}
	if err := s.Store.SaveAccountState(ctx, row.OwnerID, row.ID, patch, nil, del); err != nil {
		return nil, nil, fmt.Errorf("forgetting state saved for other credentials: %w", err)
	}
	return env, config, nil
}

// Save implements StateStore.
func (s *SealedState) Save(ctx context.Context, row store.Account, audience string, u destination.StateUpdate) error {
	for k := range u.Secrets {
		if strings.HasPrefix(k, "_") {
			return fmt.Errorf("an account can't save a secret named %s", k)
		}
	}
	in := maps.Clone(u.Secrets)
	seed, ok := s.seeds.Load(row.ID)
	if !ok {
		return errors.New("its environment credentials weren't read at open")
	}
	if in == nil {
		in = map[string]string{}
	}
	in[StateSeedSecret] = seed.(string)
	put, del, err := sealUpdate(s.Keys, s.KeyState, row.OwnerID, row.ID, audience, in)
	if err != nil {
		return err
	}
	return s.Store.SaveAccountState(ctx, row.OwnerID, row.ID, u.Config, put, del)
}

// Prepare seals state for an account from the environment as Save would
// (with its environment credentials' seed), without writing it: moving
// state 2.x kept elsewhere into the secret store in one transaction.
func (s *SealedState) Prepare(row store.Account, env map[string]string, config map[string]any, u destination.StateUpdate) ([]store.SealedSecret, map[string]any, error) {
	d, ok := destination.Lookup(row.Type)
	if !ok {
		return nil, nil, fmt.Errorf("no driver for %s", row.Type)
	}
	audience := destination.AudienceOf(d.Manifest(), config)
	in := maps.Clone(u.Secrets)
	if in == nil {
		in = map[string]string{}
	}
	in[StateSeedSecret] = EnvStateSeed(env, audience)
	put, _, err := sealUpdate(s.Keys, s.KeyState, row.OwnerID, row.ID, audience, in)
	if err != nil {
		return nil, nil, err
	}
	return put, u.Config, nil
}
