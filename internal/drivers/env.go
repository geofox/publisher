package drivers

import (
	"errors"

	gonostr "fiatjaf.com/nostr"

	"github.com/geofox/publisher/internal/destination"
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
