package nostr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	gonostr "fiatjaf.com/nostr"
)

// A relay hint from a pasted pointer is dialed through a connection that
// checks the address actually dialed: a name that now resolves to a private
// or loopback address (DNS rebinding) is never connected to.
func TestHintRelayIsDialGuarded(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	url := "ws://" + strings.TrimPrefix(srv.URL, "http://")
	if ev := queryGuarded(context.Background(), url, gonostr.Filter{Limit: 1}); ev != nil {
		t.Fatalf("got an event from a loopback relay")
	}
	if hits.Load() != 0 {
		t.Errorf("the loopback relay was connected to %d times", hits.Load())
	}
}
