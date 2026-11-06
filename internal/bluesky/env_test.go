package bluesky

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
	// Without the app password nothing is configured, whatever else is set.
	for _, m := range []map[string]string{nil, {"BLUESKY_IDENTIFIER": "me.bsky.social", "BLUESKY_PDS_URL": "https://bsky.social"}} {
		if got, err := (Driver{}).FromEnv(envOf(m)); err != nil || got != nil {
			t.Errorf("FromEnv(%v) = %v, %v; want none", m, got, err)
		}
	}
	got, err := Driver{}.FromEnv(envOf(map[string]string{
		"BLUESKY_APP_PASSWORD": "abcd-efgh-ijkl-mnop",
		"BLUESKY_IDENTIFIER":   "me.bsky.social",
		"BLUESKY_PDS_URL":      "https://bsky.social/",
	}))
	if err != nil || len(got) != 1 {
		t.Fatalf("FromEnv = %v, %v; want one account", got, err)
	}
	a := got[0]
	if want := map[string]any{ConfigIdentifier: "me.bsky.social", ConfigPDSURL: "https://bsky.social"}; !reflect.DeepEqual(a.Config, want) {
		t.Errorf("config = %v, want %v", a.Config, want)
	}
	if a.Key != destination.EnvKeyDefault || a.Secrets[SecretAppPassword] != "abcd-efgh-ijkl-mnop" {
		t.Errorf("key %q, secrets %v", a.Key, a.Secrets)
	}
	if _, err := (Driver{}).Open(context.Background(), destination.OpenRequest{Config: a.Config, Secrets: a.Secrets,
		Host: destinationtest.NewHost(t)}); err != nil {
		t.Fatalf("Open: %v", err)
	}
}

func TestFromEnvErrors(t *testing.T) {
	for _, tc := range []struct {
		env  map[string]string
		want []string
	}{
		{map[string]string{"BLUESKY_APP_PASSWORD": "pw"}, []string{"BLUESKY_IDENTIFIER", "BLUESKY_PDS_URL"}},
		{map[string]string{"BLUESKY_APP_PASSWORD": "pw", "BLUESKY_IDENTIFIER": "me"}, []string{"BLUESKY_PDS_URL"}},
		{map[string]string{"BLUESKY_APP_PASSWORD": "pw", "BLUESKY_PDS_URL": "https://bsky.social"}, []string{"BLUESKY_IDENTIFIER"}},
	} {
		_, err := Driver{}.FromEnv(envOf(tc.env))
		if err == nil {
			t.Errorf("FromEnv(%v) accepted", tc.env)
			continue
		}
		for _, w := range tc.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("FromEnv(%v): error %q doesn't name %s", tc.env, err, w)
			}
		}
	}
}
