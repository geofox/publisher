package nostr

import (
	"context"
	"net"
	"reflect"
	"testing"
)

type fakeLookup map[string][]string

func (f fakeLookup) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	var out []net.IPAddr
	for _, a := range f[host] {
		out = append(out, net.IPAddr{IP: net.ParseIP(a)})
	}
	if out == nil {
		return nil, &net.DNSError{Err: "no such host", Name: host}
	}
	return out, nil
}

func TestPublicRelayHints(t *testing.T) {
	r := fakeLookup{"relay.example": {"93.184.216.34"}, "rebind.example": {"93.184.216.34", "10.0.0.5"},
		"a.example": {"1.1.1.1"}, "b.example": {"1.1.1.2"}, "c.example": {"1.1.1.3"}, "d.example": {"1.1.1.4"}}
	got := publicRelayHints(context.Background(), r, []string{
		"ws://relay.example", "wss://127.0.0.1", "wss://169.254.169.254", "wss://[::1]:7777",
		"wss://localhost", "wss://redis", "wss://rebind.example", "wss://nowhere.example", "wss://x.onion",
		"https://relay.example",
		"wss://relay.example", "wss://a.example", "wss://b.example", "wss://c.example", "wss://d.example",
	})
	want := []string{"wss://relay.example", "wss://a.example", "wss://b.example", "wss://c.example"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hints = %v, want %v", got, want)
	}
}
