package nostr

import (
	"context"
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

// NSEC_HEX alone configures the account with today's defaults; the public
// key is derived, and the result opens.
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
		ConfigBootstrapRelay: "wss://relay.geoffrey.one",
		ConfigFallbackRelays: []string{"wss://relay.geoffrey.one", "wss://nos.lol", "wss://relay.damus.io"},
		ConfigPrimaryRelays:  []string{"wss://relay.geoffrey.one"},
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
	if p.OwnerPubkey().Hex() != envPUB || p.HomeRelay() != "wss://relay.geoffrey.one" || p.PrimaryFanout() {
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
	if want := []string{"wss://relay.geoffrey.one"}; !reflect.DeepEqual(got[0].Config[ConfigPrimaryRelays], want) {
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
			if strings.Contains(err.Error(), envNSEC) {
				t.Errorf("error %q contains the secret key", err)
			}
		})
	}
}
