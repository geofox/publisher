package nostr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/geofox/publisher/internal/destination"
)

// The check asks the home relay for its NIP-11 document; Nostr has no
// credential to refuse, so AuthOK stays unset even when authenticated.
func TestCheckAsksTheHomeRelay(t *testing.T) {
	var accept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept = r.Header.Get("Accept")
		_, _ = w.Write([]byte(`{"name":"relay"}`))
	}))
	defer srv.Close()
	a := testAccount(&capturePool{})
	a.P.cfg.NIP65BootstrapRelay = "ws://" + strings.TrimPrefix(srv.URL, "http://")
	for _, auth := range []bool{false, true} {
		h := a.Check(context.Background(), destination.CheckRequest{Authenticated: auth})
		if !h.OK || !h.Reachable || h.AuthOK != nil {
			t.Errorf("authenticated=%v: health = %+v", auth, h)
		}
	}
	if accept != "application/nostr+json" {
		t.Errorf("Accept = %q", accept)
	}
	srv.Close()
	if h := a.Check(context.Background(), destination.CheckRequest{}); h.OK || h.Reachable {
		t.Errorf("closed relay: health = %+v", h)
	}
}

func TestRelayInfoURL(t *testing.T) {
	for in, want := range map[string]string{
		"wss://relay.example":     "https://relay.example",
		"ws://relay.example:7777": "http://relay.example:7777",
		"https://relay.example":   "https://relay.example",
	} {
		if got := relayInfoURL(in); got != want {
			t.Errorf("relayInfoURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIdentityRemoteIDIsThePublicKey(t *testing.T) {
	a := testAccount(&capturePool{})
	id, err := a.Identity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if id.RemoteID != a.P.OwnerPubkey().Hex() || !strings.HasPrefix(id.Handle, "npub1") {
		t.Errorf("identity = %+v", id)
	}
}
