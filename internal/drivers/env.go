package drivers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	gonostr "fiatjaf.com/nostr"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/netguard"
	"github.com/geofox/publisher/internal/nostr"
)

// EnvAccount is an account the environment configures, with its type.
type EnvAccount struct {
	Type string
	destination.EnvAccount
}

// EnvAccounts asks every registered driver that reads the environment for
// its accounts, in registration order. Every driver's errors are reported
// together, so one restart fixes them all.
func EnvAccounts(getenv func(string) string) ([]EnvAccount, error) {
	Register()
	var out []EnvAccount
	var errs []error
	for _, typ := range destination.TypesInOrder() {
		d, _ := destination.Lookup(typ)
		src, ok := d.(destination.EnvSource)
		if !ok {
			continue
		}
		accts, err := src.FromEnv(getenv)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, a := range accts {
			out = append(out, EnvAccount{Type: typ, EnvAccount: a})
		}
	}
	return out, errors.Join(errs...)
}

// EnvMediaSigner returns the key that signs Blossom uploads in an
// env-configured deployment: the env Nostr account's (part D gives the
// media store a key of its own).
func EnvMediaSigner(accts []EnvAccount) (gonostr.SecretKey, gonostr.PubKey, bool) {
	for _, a := range accts {
		if a.Type != nostr.Type {
			continue
		}
		sk, err := nostr.ParseSecretKey(a.Secrets[nostr.SecretKey])
		if err != nil {
			return gonostr.SecretKey{}, gonostr.PubKey{}, false
		}
		return sk, gonostr.GetPublicKey(sk), true
	}
	return gonostr.SecretKey{}, gonostr.PubKey{}, false
}

// OpenEnv opens the accounts the environment configures. ids gives each
// type's stored account id. Until the account registry (part A2) there is
// one account per type, so the result is keyed by type.
func OpenEnv(ctx context.Context, accts []EnvAccount, ids map[string]string, tuning destination.Tuning) (map[string]destination.Account, error) {
	out := make(map[string]destination.Account, len(accts))
	var errs []error
	for _, a := range accts {
		d, ok := destination.Lookup(a.Type)
		if !ok {
			errs = append(errs, fmt.Errorf("%s: no driver", a.Type))
			continue
		}
		if _, dup := out[a.Type]; dup {
			errs = append(errs, fmt.Errorf("%s: more than one account configured", a.Type))
			continue
		}
		acct, err := d.Open(ctx, destination.OpenRequest{
			AccountID: ids[a.Type],
			Method:    a.Method,
			Config:    a.Config,
			Secrets:   a.Secrets,
			Tuning:    tuning,
			Host:      envHost{log: slog.Default().With("account_type", a.Type, "env_key", a.Key)},
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("%s account from the environment: %w", d.Manifest().Name, err))
			continue
		}
		out[a.Type] = acct
	}
	return out, errors.Join(errs...)
}

// errNoSave: an env account can't persist state until the account registry
// (part A2) stores it. No driver saves state in A1.
var errNoSave = errors.New("this account can't save state yet")

// envHost is the host of an account the environment configures. The
// operator set every address in the account's config (the variables) and
// its network's API hosts are fixed, so NetAccount reaches them as
// Publisher always has. Addresses from content (NetContent) are reached
// through the network guard: public addresses only. Accounts added in the
// app get a host that guards their own addresses too (part C).
type envHost struct{ log *slog.Logger }

func (envHost) Save(context.Context, destination.StateUpdate) error { return errNoSave }

// Net implements destination.AccountHost. A kind it doesn't know gets the
// guarded network: a host may tighten, never loosen.
func (envHost) Net(kind destination.NetPolicy) destination.Net {
	if kind == destination.NetAccount {
		return destination.TrustedNet()
	}
	return destination.ContentNet(netguard.IsBlockedIP)
}

func (h envHost) Logger() *slog.Logger { return h.log }
