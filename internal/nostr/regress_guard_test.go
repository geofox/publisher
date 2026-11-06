package nostr

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	gonostr "fiatjaf.com/nostr"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/destination/destinationtest"
)

// A relay hint from a pasted pointer is dialed through a connection that
// checks the address actually dialed: a name that now resolves to a private
// or loopback address (DNS rebinding) is never connected to.
func TestHintRelayIsDialGuarded(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	url := "ws://" + strings.TrimPrefix(srv.URL, "http://")
	p := New(Config{})
	if ev := p.queryGuarded(context.Background(), url, gonostr.Filter{Limit: 1}); ev != nil {
		t.Fatalf("got an event from a loopback relay")
	}
	if hits.Load() != 0 {
		t.Errorf("the loopback relay was connected to %d times", hits.Load())
	}
}

// An account opened by a host dials relays named by content through the
// host's NetContent network, not a client of its own.
func TestHintRelayUsesHostContentNet(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	host := destinationtest.NewHost(t).AllowDial()
	acct, err := Driver{}.Open(context.Background(), destination.OpenRequest{
		Config:  map[string]any{ConfigBootstrapRelay: "wss://relay.example"},
		Secrets: map[string]string{SecretKey: strings.Repeat("01", 32)},
		Host:    host,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer acct.(io.Closer).Close()
	url := "ws://" + strings.TrimPrefix(srv.URL, "http://")
	acct.(*Account).P.queryGuarded(context.Background(), url, gonostr.Filter{Limit: 1})
	if hits.Load() == 0 {
		t.Error("the relay was not reached through the host's network (which allows this test's dials)")
	}
	if !slices.Contains(host.Policies(), destination.NetContent) {
		t.Errorf("policies = %v, want NetContent among them", host.Policies())
	}
}
