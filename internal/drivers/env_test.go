package drivers

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/geofox/publisher/internal/destination"
)

const (
	testNSEC = "0000000000000000000000000000000000000000000000000000000000000001"
	testPUB  = "79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

var fullEnv = map[string]string{
	"NSEC_HEX":             testNSEC,
	"BLUESKY_APP_PASSWORD": "aaaa-bbbb-cccc-dddd",
	"BLUESKY_IDENTIFIER":   "me.example",
	"BLUESKY_PDS_URL":      "https://pds.example",
	"MASTODON_TOKEN":       "tok",
	"MASTODON_BASE_URL":    "https://masto.example",
	"THREADS_ACCESS_TOKEN": "tok",
}

// Every driver's accounts, in registration order; they all open, keyed by
// type, and the Nostr account's key signs media.
func TestEnvAccounts(t *testing.T) {
	env, err := EnvAccounts(envOf(fullEnv))
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, e := range env {
		types = append(types, e.Type)
	}
	if want := []string{"bluesky", "mastodon", "threads", "nostr"}; !reflect.DeepEqual(types, want) {
		t.Fatalf("types = %v, want %v", types, want)
	}
	accts, err := OpenEnv(context.Background(), env, map[string]string{"nostr": "acc_n"}, destination.Tuning{})
	if err != nil {
		t.Fatal(err)
	}
	if len(accts) != 4 {
		t.Fatalf("opened %d accounts, want 4", len(accts))
	}
	for typ, a := range accts {
		if a.Manifest().Type != typ {
			t.Errorf("accounts[%s] is a %s account", typ, a.Manifest().Type)
		}
	}
	_, pub, ok := EnvMediaSigner(env)
	if !ok || pub.Hex() != testPUB {
		t.Errorf("media signer = %s, %v; want %s", pub.Hex(), ok, testPUB)
	}
}

func TestEnvAccountsNone(t *testing.T) {
	env, err := EnvAccounts(envOf(nil))
	if err != nil || len(env) != 0 {
		t.Fatalf("EnvAccounts() = %v, %v; want none", env, err)
	}
	if _, _, ok := EnvMediaSigner(env); ok {
		t.Error("a media signer without a Nostr account")
	}
}

// Every driver's errors come back together, so one restart fixes them all.
func TestEnvAccountsJoinsErrors(t *testing.T) {
	_, err := EnvAccounts(envOf(map[string]string{
		"MASTODON_TOKEN":       "tok",
		"BLUESKY_APP_PASSWORD": "pw",
		"OWNER_PUBKEY":         testPUB,
	}))
	if err == nil {
		t.Fatal("accepted")
	}
	for _, want := range []string{"MASTODON_BASE_URL", "BLUESKY_IDENTIFIER", "BLUESKY_PDS_URL", "NSEC_HEX"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q doesn't name %s", err, want)
		}
	}
}

func TestOpenEnvRefusesTwoOfAType(t *testing.T) {
	env, err := EnvAccounts(envOf(map[string]string{"MASTODON_TOKEN": "tok", "MASTODON_BASE_URL": "https://m.example"}))
	if err != nil {
		t.Fatal(err)
	}
	env = append(env, env[0])
	if _, err := OpenEnv(context.Background(), env, nil, destination.Tuning{}); err == nil {
		t.Fatal("two accounts of one type opened")
	}
}

// An env account's host reaches the network as before and can't save yet.
func TestEnvHost(t *testing.T) {
	h := envHost{}
	if err := h.Save(context.Background(), destination.StateUpdate{}); err == nil {
		t.Error("Save succeeded")
	}
	if n := h.Net(destination.NetTrusted); n.DialContext == nil || n.Proxy == nil {
		t.Error("trusted net has no dialer or proxy")
	}
}
