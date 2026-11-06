package mastodon

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/destination/destinationtest"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestFromEnv(t *testing.T) {
	// Without the token nothing is configured, whatever else is set.
	for _, m := range []map[string]string{nil, {"MASTODON_BASE_URL": "https://mastodon.social"}} {
		if got, err := (Driver{}).FromEnv(envOf(m)); err != nil || got != nil {
			t.Errorf("FromEnv(%v) = %v, %v; want none", m, got, err)
		}
	}
	got, err := Driver{}.FromEnv(envOf(map[string]string{"MASTODON_TOKEN": "tok", "MASTODON_BASE_URL": "https://mastodon.social/"}))
	if err != nil || len(got) != 1 {
		t.Fatalf("FromEnv = %v, %v; want one account", got, err)
	}
	a := got[0]
	if want := map[string]any{ConfigInstanceURL: "https://mastodon.social"}; !reflect.DeepEqual(a.Config, want) {
		t.Errorf("config = %v, want %v", a.Config, want)
	}
	if a.Key != destination.EnvKeyDefault || a.Secrets[SecretAccessToken] != "tok" {
		t.Errorf("key %q, secrets %v", a.Key, a.Secrets)
	}
	if _, err := (Driver{}).Open(context.Background(), destination.OpenRequest{Config: a.Config, Secrets: a.Secrets,
		Host: destinationtest.NewHost(t)}); err != nil {
		t.Fatalf("Open: %v", err)
	}
}

// A token without an instance used to fail every send; now it fails at
// start, naming the missing variable.
func TestFromEnvTokenWithoutInstance(t *testing.T) {
	const secret = "s3cr3t-t0ken"
	_, err := Driver{}.FromEnv(envOf(map[string]string{"MASTODON_TOKEN": secret}))
	if err == nil || !strings.Contains(err.Error(), "MASTODON_BASE_URL") {
		t.Fatalf("err = %v, want one naming MASTODON_BASE_URL", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error %q contains the token", err)
	}
}
