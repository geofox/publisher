package nostr

import (
	"encoding/hex"
	"errors"

	gonostr "fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"

	"github.com/geofox/publisher/internal/destination"
)

// Auth methods.
const (
	// MethodSecretKey connects with a key the owner has (nsec1… or hex).
	MethodSecretKey = "secret_key"
	// MethodGenerate makes a new key.
	MethodGenerate = "generate"
)

// newAccountFallbackRelays are the fallback relays offered to a new
// account when the operator set none: large public relays, never this
// instance's own.
var newAccountFallbackRelays = []string{"wss://nos.lol", "wss://relay.damus.io"}

func authMethods() []destination.AuthMethod {
	return []destination.AuthMethod{
		{
			ID:    MethodSecretKey,
			Flow:  destination.FlowFields,
			Label: "Secret key",
			Help:  "The key of the Nostr identity to post as. It signs on this server and is never sent anywhere.",
			Fields: []destination.FieldSpec{
				{Key: SecretKey, Label: "Secret key", Type: destination.FieldString, Secret: true, Revealable: true,
					Required: true, Placeholder: "nsec1…", MaxLength: 100,
					Pattern: `nsec1[02-9ac-hj-np-zAC-HJ-NP-Z]{58}|[0-9a-fA-F]{64}`,
					Help:    "nsec1… or 64 hex characters."},
			},
		},
		{
			ID:    MethodGenerate,
			Flow:  destination.FlowGenerate,
			Label: "Make a new key",
			Help:  "Publisher makes a new Nostr identity. You can show its key afterwards to keep a copy.",
		},
	}
}

func configFields() []destination.FieldSpec {
	zero, most := 0, 40
	return []destination.FieldSpec{
		{Key: ConfigBootstrapRelay, Label: "Home relay", Type: destination.FieldURL, URL: &destination.RelayURL,
			Required: true, Placeholder: "wss://relay.example.com",
			Help: "Your own relay, or the one you use most. Publisher reads your relay list (NIP-65) here, " +
				"and relay sync compares your other relays with it."},
		{Key: ConfigFallbackRelays, Label: "Fallback relays", Type: destination.FieldURLList, URL: &destination.RelayURL,
			Default: newAccountFallbackRelays, Advanced: true,
			Help: "Where posts go when your relay list can't be read."},
		{Key: ConfigPrimaryRelays, Label: "Primary relays", Type: destination.FieldURLList, URL: &destination.RelayURL,
			Advanced: true,
			Help:     "With primary fan-out on, a post waits only for these and the rest get it in the background. Your home relay when empty."},
		{Key: ConfigPrimaryFanout, Label: "Primary fan-out", Type: destination.FieldBool, Default: false, Advanced: true,
			Help: "Answer as soon as the primary relays have the post."},
		{Key: ConfigPOWDefault, Label: "Proof of work", Type: destination.FieldInt, Min: &zero, Max: &most,
			Default: DefaultPOW, Advanced: true, Help: "NIP-13 difficulty of a post; 0 turns it off."},
		{Key: ConfigPOWMax, Label: "Most proof of work", Type: destination.FieldInt, Min: &zero, Max: &most,
			Default: DefaultPOWMax, Advanced: true, Help: "The most a post may ask for."},
	}
}

var (
	_ destination.ConfigDefaulter     = Driver{}
	_ destination.CredentialGenerator = Driver{}
	_ destination.SecretFormatter     = Driver{}
)

// ConfigDefaults implements destination.ConfigDefaulter: a new account
// starts with the relays and proof of work the operator set in the
// environment, where they are set. The built-in defaults of those
// variables (this instance's relay among them) are never offered.
func (Driver) ConfigDefaults(getenv func(string) string) map[string]any {
	d := map[string]any{}
	if v := destination.EnvString(getenv, "NIP65_BOOTSTRAP_RELAY", ""); v != "" {
		d[ConfigBootstrapRelay] = v
	}
	for key, name := range map[string]string{ConfigFallbackRelays: "FALLBACK_RELAYS", ConfigPrimaryRelays: "PRIMARY_RELAYS"} {
		if l := destination.EnvList(getenv, name, ""); len(l) > 0 {
			d[key] = l
		}
	}
	if b, err := destination.EnvBool(getenv, "NOSTR_PRIMARY_FANOUT", false); err == nil && getenv("NOSTR_PRIMARY_FANOUT") != "" {
		d[ConfigPrimaryFanout] = b
	}
	for key, name := range map[string]string{ConfigPOWDefault: "POW_DIFFICULTY_DEFAULT", ConfigPOWMax: "POW_DIFFICULTY_MAX"} {
		if n, err := destination.EnvInt(getenv, name, -1); err == nil && n >= 0 && getenv(name) != "" {
			d[key] = n
		}
	}
	return d
}

// NewAccountSyncRelays is the relay-sync list a new account starts with:
// SYNC_RELAYS when set, else the built-in list (public relays only).
func NewAccountSyncRelays(getenv func(string) string) []string {
	return destination.EnvList(getenv, "SYNC_RELAYS", defaultSyncRelays)
}

// GenerateCredentials implements destination.CredentialGenerator: a new
// secret key, as hex.
func (Driver) GenerateCredentials(method string) (map[string]string, error) {
	if method != MethodGenerate {
		return nil, errors.New("this method doesn't generate a key")
	}
	sk := gonostr.Generate()
	return map[string]string{SecretKey: hex.EncodeToString(sk[:])}, nil
}

// FormatSecret implements destination.SecretFormatter: the secret key as
// nsec1…, however it was stored (a generated key is hex). A value that
// isn't a key is given back as it is.
func (Driver) FormatSecret(key, value string) string {
	if key != SecretKey {
		return value
	}
	sk, err := ParseKey(value)
	if err != nil {
		return value
	}
	return nip19.EncodeNsec(sk)
}
