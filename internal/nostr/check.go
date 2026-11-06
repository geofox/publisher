package nostr

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/geofox/publisher/internal/destination"
)

var _ destination.Checker = (*Account)(nil)

// Check implements destination.Checker: the home relay answers its NIP-11
// information request. Nostr has no credential a network can refuse (the
// key signs locally), so an authenticated check is the same and leaves
// AuthOK unset.
func (a *Account) Check(ctx context.Context, _ destination.CheckRequest) destination.Health {
	client := a.HTTP
	if client == nil {
		client = destination.TrustedNet().Client(10 * time.Second)
	}
	return destination.CheckReachable(ctx, client, relayInfoURL(a.P.HomeRelay()),
		http.Header{"Accept": {"application/nostr+json"}})
}

// relayInfoURL is the HTTP address of a relay's NIP-11 document.
func relayInfoURL(relay string) string {
	switch {
	case strings.HasPrefix(relay, "wss://"):
		return "https://" + strings.TrimPrefix(relay, "wss://")
	case strings.HasPrefix(relay, "ws://"):
		return "http://" + strings.TrimPrefix(relay, "ws://")
	}
	return relay
}
