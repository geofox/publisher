package drivers

import (
	"context"
	"errors"

	gonostr "fiatjaf.com/nostr"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/nostr"
	"github.com/geofox/publisher/internal/store"
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

// EnvNotices are lines for the startup log about the accounts the
// environment sets (a built-in default worth knowing about).
func EnvNotices(getenv func(string) string) []string {
	var out []string
	if n := nostr.EnvNotice(getenv); n != "" {
		out = append(out, n)
	}
	return out
}

// SecretEnvVars are the credential variables every registered driver
// reads (EnvVar.Secret).
func SecretEnvVars() []string {
	Register()
	var out []string
	for _, typ := range destination.TypesInOrder() {
		d, _ := destination.Lookup(typ)
		if src, ok := d.(destination.EnvSource); ok {
			for _, v := range src.EnvVars() {
				if v.Secret {
					out = append(out, v.Name)
				}
			}
		}
	}
	return out
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

// MediaSigningKey reads the media store's own signing key
// (media.blossom_key: nsec1… or hex).
func MediaSigningKey(s string) (gonostr.SecretKey, error) { return nostr.ParseKey(s) }

// StillSet is an account the environment still configures although its
// variables no longer apply: it was adopted into the app (or adopted,
// then deleted there).
type StillSet struct {
	AccountID, Type string
	Deleted         bool
	// Vars are the type's variables that configure an account.
	Vars []string
	// Keep names a variable still in use for something else.
	Keep string
}

// AdoptedStillSet lists the accounts adopted into the app whose
// variables are still set (rows: the owner's accounts, deleted ones
// included). Their variables are ignored: seeding never makes a second
// account for them. mediaSigned: the environment's Nostr key signs media
// uploads (BLOSSOM_URL), so it stays.
func AdoptedStillSet(rows []store.Account, env []EnvAccount, mediaSigned bool) []StillSet {
	var out []StillSet
	for _, row := range rows {
		if row.Source != store.SourceDB || row.EnvKey == "" {
			continue
		}
		for _, e := range env {
			key := e.Key
			if key == "" {
				key = destination.EnvKeyDefault
			}
			if e.Type != row.Type || key != row.EnvKey {
				continue
			}
			s := StillSet{AccountID: row.ID, Type: row.Type, Deleted: !row.DeletedAt.IsZero()}
			if row.Type == nostr.Type && mediaSigned {
				s.Keep = "keep NSEC_HEX while BLOSSOM_URL is set: it signs media uploads"
			}
			if d, ok := destination.Lookup(row.Type); ok {
				if src, ok := d.(destination.EnvSource); ok {
					for _, v := range src.EnvVars() {
						if v.Required {
							s.Vars = append(s.Vars, v.Name)
						}
					}
				}
			}
			out = append(out, s)
		}
	}
	return out
}

// SyncRelaySeeder keeps an account's starting relay-sync list.
type SyncRelaySeeder interface {
	SeedAccountSyncRelaysOnce(ctx context.Context, accountID string, urls []string) error
}

// NewAccountSeeder gives an account added in the app its starting state
// beside its row: a Nostr account starts relay sync with SYNC_RELAYS (or
// the built-in list of public relays).
func NewAccountSeeder(st SyncRelaySeeder, getenv func(string) string) func(context.Context, store.Account) error {
	return func(ctx context.Context, row store.Account) error {
		if row.Type != nostr.Type {
			return nil
		}
		return st.SeedAccountSyncRelaysOnce(ctx, row.ID, nostr.NewAccountSyncRelays(getenv))
	}
}
