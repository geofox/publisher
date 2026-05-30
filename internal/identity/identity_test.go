package identity

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeP struct {
	h, n, a string
	err     error
}

func (f fakeP) Profile(context.Context) (string, string, string, error) { return f.h, f.n, f.a, f.err }

type fakeN struct {
	npub, n, a string
}

func (f fakeN) OwnerProfile(context.Context) (string, string, string, error) {
	return f.npub, f.n, f.a, nil
}

func TestGetAggregatesAndIsFailSoft(t *testing.T) {
	s := &Service{
		Bluesky:  fakeP{h: "geoffrey.one", n: "Geoffrey", a: "https://cdn/av.jpg"},
		Mastodon: fakeP{err: errors.New("token expired")},               // empty + error → omitted
		Threads:  fakeP{h: "@geoffrey", err: errors.New("rate limited")}, // handle despite error → kept
		Nostr:    fakeN{npub: "npub1abc", n: "Geo"},
	}
	id := s.Get(context.Background())

	if _, ok := id.Accounts["mastodon"]; ok {
		t.Errorf("mastodon should be omitted when the fetch errors with no usable data")
	}
	if got := id.Accounts["threads"].Handle; got != "@geoffrey" {
		t.Errorf("threads handle should survive a non-empty error: got %q", got)
	}
	if id.Accounts["bluesky"].Handle != "geoffrey.one" || id.Accounts["nostr"].Handle != "npub1abc" {
		t.Errorf("accounts = %+v", id.Accounts)
	}
	if id.Name != "Geoffrey" {
		t.Errorf("Name = %q, want Geoffrey (bluesky preferred)", id.Name)
	}
	if id.Avatar != "https://cdn/av.jpg" {
		t.Errorf("Avatar = %q, want the bluesky avatar", id.Avatar)
	}
	if id.Monogram != "G" {
		t.Errorf("Monogram = %q, want G", id.Monogram)
	}
}

type countingN struct{ n *int }

func (c countingN) OwnerProfile(context.Context) (string, string, string, error) {
	*c.n++
	return "npub1x", "", "", nil
}

func TestGetCachesWithinTTL(t *testing.T) {
	calls := 0
	s := &Service{TTL: time.Hour, Nostr: countingN{&calls}}
	a := s.Get(context.Background())
	b := s.Get(context.Background())
	if a != b {
		t.Errorf("expected the cached identity pointer to be reused within the TTL")
	}
	if calls != 1 {
		t.Errorf("expected 1 fetch within TTL, got %d", calls)
	}
}

func TestGetEmptyWhenNoProfilers(t *testing.T) {
	id := (&Service{}).Get(context.Background())
	if id == nil || len(id.Accounts) != 0 {
		t.Errorf("want empty accounts, got %+v", id)
	}
}
