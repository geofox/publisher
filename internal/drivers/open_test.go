package drivers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/destination/destinationtest"
)

// A complete account opens without touching the network; each missing
// field is an invalid error naming it.
func TestOpen(t *testing.T) {
	Register()
	key := "0000000000000000000000000000000000000000000000000000000000000001"
	cases := []struct {
		typ     string
		config  map[string]any
		secrets map[string]string
	}{
		{"bluesky", map[string]any{"pds_url": "https://pds.example", "identifier": "me.example"},
			map[string]string{"app_password": "aaaa-bbbb-cccc-dddd"}},
		{"mastodon", map[string]any{"instance_url": "https://masto.example"},
			map[string]string{"access_token": "tok"}},
		{"threads", map[string]any{"user_id": "me"}, map[string]string{"access_token": "tok"}},
		{"nostr", map[string]any{"bootstrap_relay": "wss://relay.example", "pow_default": float64(8)},
			map[string]string{"secret_key": key}},
	}
	for _, c := range cases {
		t.Run(c.typ, func(t *testing.T) {
			d, ok := destination.Lookup(c.typ)
			if !ok {
				t.Fatalf("%s not registered", c.typ)
			}
			host := destinationtest.NewHost(t)
			acct, err := d.Open(context.Background(), destination.OpenRequest{
				AccountID: "acc_test", Config: c.config, Secrets: c.secrets, Host: host})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if acct.Manifest().Type != c.typ {
				t.Errorf("opened a %s account", acct.Manifest().Type)
			}
			// Every required field missing in turn.
			for k := range c.config {
				if c.typ == "threads" || (c.typ == "nostr" && k != "bootstrap_relay") {
					continue // optional
				}
				cfg := map[string]any{}
				for k2, v := range c.config {
					if k2 != k {
						cfg[k2] = v
					}
				}
				_, err := d.Open(context.Background(), destination.OpenRequest{Config: cfg, Secrets: c.secrets, Host: host})
				assertInvalid(t, err, k)
			}
			for k := range c.secrets {
				_, err := d.Open(context.Background(), destination.OpenRequest{Config: c.config, Host: host})
				assertInvalid(t, err, k)
			}
		})
	}
}

func assertInvalid(t *testing.T, err error, field string) {
	t.Helper()
	var de *destination.Error
	if !errors.As(err, &de) || de.Class != destination.ClassInvalid || !strings.Contains(err.Error(), field) {
		t.Errorf("without %s: err = %v, want an invalid error naming it", field, err)
	}
}

func TestRegistrationOrder(t *testing.T) {
	Register()
	got := strings.Join(destination.TypesInOrder(), ",")
	if want := "bluesky,mastodon,threads,nostr"; !strings.HasPrefix(got, want) {
		t.Fatalf("TypesInOrder = %s, want %s first", got, want)
	}
}
