package accounts

import (
	"context"
	"errors"
	"log/slog"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/secrets"
	"github.com/geofox/publisher/internal/store"
)

// errNoState: the registry has nowhere to keep what accounts save.
var errNoState = destination.ErrStateUnsupported

// host is the registry's side of one open account.
type host struct {
	r   *Registry
	id  string
	row store.Account // as opened; type and source never change
	// audience is where the account's credentials go, as opened
	// (destination.AudienceOf): what they are sealed for.
	audience string
}

// current is the account's row as the registry has it now.
func (h *host) current() store.Account {
	h.r.mu.RLock()
	defer h.r.mu.RUnlock()
	if e, ok := h.r.entries[h.id]; ok {
		return e.row
	}
	return h.row
}

// Save implements destination.AccountHost. An account added in the app
// seals its new credentials for the server it was opened for and keeps
// them with its configuration in one store call; an account from the
// environment saves through the StateStore.
func (h *host) Save(ctx context.Context, u destination.StateUpdate) error {
	if h.row.Source == store.SourceDB {
		return h.saveDB(ctx, u)
	}
	if h.r.opt.State == nil {
		return errNoState
	}
	return h.r.opt.State.Save(ctx, h.current(), h.audience, u)
}

// errMoveAudience: an account tried to change where its credentials go.
var errMoveAudience = errors.New("an account can't send its credentials to another server by itself; the owner connects it again")

func (h *host) saveDB(ctx context.Context, u destination.StateUpdate) error {
	if h.r.opt.Secrets == nil {
		return errNoState
	}
	if d, ok := destination.Lookup(h.row.Type); ok {
		if key := d.Manifest().AudienceKey(); key != "" {
			if v, ok := u.Config[key]; ok && destination.AudienceOf(d.Manifest(), map[string]any{key: v}) != h.audience {
				return errMoveAudience
			}
		}
	}
	put, del, err := sealUpdate(h.r.opt.Keys, h.r.opt.KeyState, h.row.OwnerID, h.id, h.audience, u.Secrets)
	if err != nil {
		return err
	}
	return h.r.opt.Secrets.SaveAccountState(ctx, h.row.OwnerID, h.id, u.Config, put, del)
}

// sealUpdate seals the secrets of a state update for an account (a ""
// value is a deletion). Sealing needs a key the stored values agree with.
func sealUpdate(kr secrets.Keyring, ks KeyState, owner, id, audience string, in map[string]string) (put []store.SealedSecret, del []string, err error) {
	for name, v := range in {
		if v == "" {
			del = append(del, name)
			continue
		}
		if err := ks.WriteError(); err != nil {
			return nil, nil, err
		}
		b := secrets.Binding{Owner: owner, Scope: store.SecretScopeAccount, Ref: id, Name: name, Audience: audience}
		sealed, err := kr.Seal(b, v)
		if err != nil {
			return nil, nil, err
		}
		kid, err := secrets.KeyID(sealed)
		if err != nil {
			return nil, nil, err
		}
		put = append(put, store.SealedSecret{Owner: owner, Scope: b.Scope, Ref: id, Name: name, Audience: audience, Sealed: sealed, KeyID: kid})
	}
	return put, del, nil
}

// Net implements destination.AccountHost. The operator set every address
// of an account from the environment, so its own addresses are reached
// as Publisher always has. An account added in the app reaches its own
// addresses through the guard (public addresses, and the private ones
// PRIVATE_DESTINATIONS lists), whatever the driver asks; an address from
// content reaches public addresses only. Neither reaches the server
// itself.
func (h *host) Net(kind destination.NetPolicy) destination.Net {
	g := h.r.opt.Guard
	if kind == destination.NetAccount {
		if h.row.Source == store.SourceEnv {
			return destination.TrustedNet()
		}
		return destination.GuardedNet(g.Allow)
	}
	return destination.GuardedNet(g.AllowContent)
}

// Logger implements destination.AccountHost.
func (h *host) Logger() *slog.Logger {
	return h.r.log.With("account", h.id, "account_type", h.row.Type)
}
