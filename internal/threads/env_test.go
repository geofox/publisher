package threads

import (
	"context"
	"reflect"
	"testing"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/destination/destinationtest"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestFromEnv(t *testing.T) {
	// Without the token nothing is configured, whatever else is set.
	for _, m := range []map[string]string{nil, {"THREADS_USER_ID": "123"}} {
		if got, err := (Driver{}).FromEnv(envOf(m)); err != nil || got != nil {
			t.Errorf("FromEnv(%v) = %v, %v; want none", m, got, err)
		}
	}
	for _, tc := range []struct {
		env    map[string]string
		userID string
	}{
		{map[string]string{"THREADS_ACCESS_TOKEN": "tok"}, "me"},
		{map[string]string{"THREADS_ACCESS_TOKEN": "tok", "THREADS_USER_ID": "123"}, "123"},
	} {
		got, err := Driver{}.FromEnv(envOf(tc.env))
		if err != nil || len(got) != 1 {
			t.Fatalf("FromEnv(%v) = %v, %v; want one account", tc.env, got, err)
		}
		a := got[0]
		if want := map[string]any{ConfigUserID: tc.userID}; !reflect.DeepEqual(a.Config, want) {
			t.Errorf("config = %v, want %v", a.Config, want)
		}
		if a.Key != destination.EnvKeyDefault || a.Secrets[SecretAccessToken] != "tok" {
			t.Errorf("key %q, secrets %v", a.Key, a.Secrets)
		}
		acct, err := Driver{}.Open(context.Background(), destination.OpenRequest{Config: a.Config, Secrets: a.Secrets,
			Host: destinationtest.NewHost(t)})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if got := acct.(*Account).C.UserID; got != tc.userID {
			t.Errorf("client user id = %q, want %q", got, tc.userID)
		}
	}
}

// The token is kept exactly as set. The token manager recognizes the env
// token by its hash; trimming it would read as a rotated token on upgrade
// and replace the refreshed token in the database with the old one.
func TestFromEnvKeepsTokenAsSet(t *testing.T) {
	got, err := Driver{}.FromEnv(envOf(map[string]string{"THREADS_ACCESS_TOKEN": "EAAtok \n"}))
	if err != nil || len(got) != 1 {
		t.Fatalf("FromEnv = %v, %v", got, err)
	}
	if tok := got[0].Secrets[SecretAccessToken]; seedHash(tok) != seedHash("EAAtok \n") {
		t.Errorf("token = %q: its hash no longer matches the one stored by earlier versions", tok)
	}
	if got, err := (Driver{}).FromEnv(envOf(map[string]string{"THREADS_ACCESS_TOKEN": "  "})); err != nil || got != nil {
		t.Errorf("a token of spaces only should mean unset: %v, %v", got, err)
	}
}
