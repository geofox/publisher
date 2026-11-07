package nostr

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/destination/destinationtest"
)

// A matching key pair: secret 00..01 and its x-only public key.
const (
	envNSEC = "0000000000000000000000000000000000000000000000000000000000000001"
	envPUB  = "79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"
	// The public key of secret 00..03 (BIP-340 test vector 0).
	otherPUB = "f9308a019258c31049344f85f89d5229b531c845836f99b08601f113bce036f9"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestFromEnvUnset(t *testing.T) {
	got, err := Driver{}.FromEnv(envOf(nil))
	if err != nil || got != nil {
		t.Fatalf("FromEnv() = %v, %v; want none", got, err)
	}
}

// NSEC_HEX alone configures the account with the defaults (public relays,
// none of them this project's own); the public key is derived, and the
// result opens.
func TestFromEnvDefaults(t *testing.T) {
	got, err := Driver{}.FromEnv(envOf(map[string]string{"NSEC_HEX": envNSEC}))
	if err != nil || len(got) != 1 {
		t.Fatalf("FromEnv = %v, %v; want one account", got, err)
	}
	a := got[0]
	if a.Key != destination.EnvKeyDefault || a.Secrets[SecretKey] != envNSEC {
		t.Errorf("key %q, secrets %v", a.Key, a.Secrets)
	}
	want := map[string]any{
		ConfigBootstrapRelay: "wss://relay.damus.io",
		ConfigFallbackRelays: []string{"wss://nos.lol", "wss://relay.damus.io", "wss://relay.primal.net"},
		ConfigPrimaryRelays:  []string{"wss://relay.damus.io"},
		ConfigPrimaryFanout:  false,
		ConfigPOWDefault:     16,
		ConfigPOWMax:         28,
		ConfigSyncRelaysSeed: []string{"wss://nos.lol", "wss://relay.damus.io", "wss://nostr.wine", "wss://nostr.land",
			"wss://relay.nostr.band", "wss://purplepag.es", "wss://relay.snort.social", "wss://nostr.mom"},
	}
	if !reflect.DeepEqual(a.Config, want) {
		t.Errorf("config =\n%#v\nwant\n%#v", a.Config, want)
	}
	acct, err := Driver{}.Open(context.Background(), destination.OpenRequest{Config: a.Config, Secrets: a.Secrets,
		Host: destinationtest.NewHost(t)})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	p := acct.(*Account).P
	if p.OwnerPubkey().Hex() != envPUB || p.HomeRelay() != "wss://relay.damus.io" || p.PrimaryFanout() {
		t.Errorf("opened publisher: pubkey %s, home %s, fanout %v", p.OwnerPubkey().Hex(), p.HomeRelay(), p.PrimaryFanout())
	}
}

func TestFromEnvSet(t *testing.T) {
	got, err := Driver{}.FromEnv(envOf(map[string]string{
		"NSEC_HEX":               envNSEC,
		"OWNER_PUBKEY":           envPUB,
		"NIP65_BOOTSTRAP_RELAY":  "wss://home.example",
		"FALLBACK_RELAYS":        "wss://a.example, wss://b.example",
		"NOSTR_PRIMARY_FANOUT":   "TRUE",
		"POW_DIFFICULTY_DEFAULT": "8",
		"POW_DIFFICULTY_MAX":     "20",
		"SYNC_RELAYS":            "wss://s.example",
	}))
	if err != nil || len(got) != 1 {
		t.Fatalf("FromEnv = %v, %v", got, err)
	}
	c := got[0].Config
	// PRIMARY_RELAYS defaults to the bootstrap relay.
	if !reflect.DeepEqual(c[ConfigPrimaryRelays], []string{"wss://home.example"}) ||
		c[ConfigBootstrapRelay] != "wss://home.example" ||
		!reflect.DeepEqual(c[ConfigFallbackRelays], []string{"wss://a.example", "wss://b.example"}) ||
		c[ConfigPrimaryFanout] != true || c[ConfigPOWDefault] != 8 || c[ConfigPOWMax] != 20 ||
		!reflect.DeepEqual(c[ConfigSyncRelaysSeed], []string{"wss://s.example"}) {
		t.Errorf("config = %#v", c)
	}
}

// A PRIMARY_RELAYS of only separators falls back to the bootstrap relay.
func TestFromEnvEmptyPrimaryRelays(t *testing.T) {
	got, err := Driver{}.FromEnv(envOf(map[string]string{"NSEC_HEX": envNSEC, "PRIMARY_RELAYS": " , "}))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"wss://relay.damus.io"}; !reflect.DeepEqual(got[0].Config[ConfigPrimaryRelays], want) {
		t.Errorf("primary relays = %v, want %v", got[0].Config[ConfigPrimaryRelays], want)
	}
}

func TestFromEnvErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want []string // each must appear in the error
	}{
		{"pubkey without key", map[string]string{"OWNER_PUBKEY": envPUB}, []string{"OWNER_PUBKEY", "NSEC_HEX"}},
		{"bad key", map[string]string{"NSEC_HEX": "zz"}, []string{"NSEC_HEX"}},
		// The library pads short keys with zeros: "1" would be secret key 1,
		// which anyone can compute.
		{"short key", map[string]string{"NSEC_HEX": "1"}, []string{"NSEC_HEX", "64-character", "leading zeros"}},
		{"truncated key", map[string]string{"NSEC_HEX": envNSEC[1:]}, []string{"NSEC_HEX", "64-character", "leading zeros"}},
		{"long key", map[string]string{"NSEC_HEX": envNSEC + "00"}, []string{"NSEC_HEX", "64-character"}},
		{"negative pow", map[string]string{"NSEC_HEX": envNSEC, "POW_DIFFICULTY_DEFAULT": "-1"}, []string{"POW_DIFFICULTY_DEFAULT", "negative"}},
		{"negative pow max", map[string]string{"NSEC_HEX": envNSEC, "POW_DIFFICULTY_MAX": "-1"}, []string{"POW_DIFFICULTY_MAX", "negative"}},
		{"key in quotes", map[string]string{"NSEC_HEX": `"` + envNSEC + `"`}, []string{"NSEC_HEX"}},
		{"nsec instead of hex", map[string]string{"NSEC_HEX": "nsec1vl029mgpspedva04g90vltkh6fvh240zqtv9k0t9af8935ke9laqsnlfe5"},
			[]string{"NSEC_HEX", "nak key decode"}},
		{"bad pubkey", map[string]string{"NSEC_HEX": envNSEC, "OWNER_PUBKEY": "zz"}, []string{"OWNER_PUBKEY"}},
		{"pubkey mismatch", map[string]string{"NSEC_HEX": envNSEC, "OWNER_PUBKEY": otherPUB}, []string{"does not match", envPUB}},
		{"fanout typo", map[string]string{"NSEC_HEX": envNSEC, "NOSTR_PRIMARY_FANOUT": "flase"}, []string{"NOSTR_PRIMARY_FANOUT", `"flase"`}},
		{"pow not a number", map[string]string{"NSEC_HEX": envNSEC, "POW_DIFFICULTY_DEFAULT": "high"}, []string{"POW_DIFFICULTY_DEFAULT"}},
		{"every error at once", map[string]string{"NSEC_HEX": envNSEC, "POW_DIFFICULTY_MAX": "x", "NOSTR_PRIMARY_FANOUT": "maybe"},
			[]string{"POW_DIFFICULTY_MAX", "NOSTR_PRIMARY_FANOUT"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Driver{}.FromEnv(envOf(tc.env))
			if err == nil {
				t.Fatalf("FromEnv = %v, want an error", got)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q doesn't mention %s", err, w)
				}
			}
			// The secret never appears in an error, in any form.
			if v := tc.env["NSEC_HEX"]; len(v) > 8 && strings.Contains(err.Error(), strings.Trim(v, `"`)[:8]) {
				t.Errorf("error %q contains the secret key", err)
			}
		})
	}
}

// A list variable set to spaces only is an empty list, as in 1.x, not the
// default (the author's relays).
func TestFromEnvBlankListIsEmpty(t *testing.T) {
	got, err := Driver{}.FromEnv(envOf(map[string]string{"NSEC_HEX": envNSEC, "FALLBACK_RELAYS": " ", "SYNC_RELAYS": " "}))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{ConfigFallbackRelays, ConfigSyncRelaysSeed} {
		if l, _ := got[0].Config[k].([]string); len(l) != 0 {
			t.Errorf("%s = %v, want empty", k, l)
		}
	}
}

// Open names the field that is wrong.
func TestOpenNamesNegativePOWField(t *testing.T) {
	for _, field := range []string{ConfigPOWDefault, ConfigPOWMax} {
		_, err := Driver{}.Open(context.Background(), destination.OpenRequest{
			Config:  map[string]any{ConfigBootstrapRelay: "wss://r.example", field: -1},
			Secrets: map[string]string{SecretKey: envNSEC},
			Host:    destinationtest.NewHost(t)})
		if err == nil || !strings.HasPrefix(err.Error(), field+":") {
			t.Errorf("%s: err = %v", field, err)
		}
	}
}

// Open refuses a short key too (a stored config can be edited by hand).
func TestOpenRefusesShortKey(t *testing.T) {
	_, err := Driver{}.Open(context.Background(), destination.OpenRequest{
		Config:  map[string]any{ConfigBootstrapRelay: "wss://r.example"},
		Secrets: map[string]string{SecretKey: "1"},
		Host:    destinationtest.NewHost(t)})
	var de *destination.Error
	if !errors.As(err, &de) || de.Class != destination.ClassInvalid || !strings.Contains(err.Error(), SecretKey) {
		t.Fatalf("err = %v, want an invalid error naming %s", err, SecretKey)
	}
}
