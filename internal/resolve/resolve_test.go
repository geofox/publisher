package resolve

import (
	"context"
	"errors"
	"testing"
)

type fakeSource struct {
	gotInput string
	ref      *SourceRef
	err      error
}

func (f *fakeSource) ResolveSource(_ context.Context, input string) (*SourceRef, error) {
	f.gotInput = input
	return f.ref, f.err
}

func TestDetect(t *testing.T) {
	cases := map[string]string{
		"https://bsky.app/profile/alice.bsky.social/post/3kabc": "bluesky",
		"https://mastodon.social/@alice/112233445566":           "mastodon",
		"https://example.town/users/bob/statuses/9":             "mastodon",
		"https://www.threads.net/@alice/post/Cabc":              "threads",
		"https://threads.com/@alice/post/Cabc":                  "threads",
		"nostr:nevent1qqsxyz":                                   "nostr",
		"nevent1QQSXYZ":                                         "nostr",   // bech32 is case-insensitive
		"https://bsky.app:443/profile/a/post/x":                "bluesky", // host carries a port
		"https://www.threads.net:443/@a/post/x":                "threads",
		"nevent1qqsxyz":                                         "nostr",
		"note1qqsxyz":                                           "nostr",
		"naddr1qqsxyz":                                          "nostr",
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef": "nostr",
		"not a url or id": "",
	}
	for in, want := range cases {
		if got := detect(in); got != want {
			t.Errorf("detect(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveDelegatesByPlatform(t *testing.T) {
	bsky := &fakeSource{ref: &SourceRef{Platform: "bluesky"}}
	s := &Service{Bluesky: bsky}
	ref, err := s.Resolve(context.Background(), "https://bsky.app/profile/a/post/x")
	if err != nil || ref.Platform != "bluesky" {
		t.Fatalf("ref=%v err=%v", ref, err)
	}
	if bsky.gotInput == "" {
		t.Errorf("input not forwarded to bluesky source")
	}
}

func TestResolveThreadsUnsupported(t *testing.T) {
	s := &Service{}
	_, err := s.Resolve(context.Background(), "https://www.threads.net/@a/post/x")
	if !errors.Is(err, ErrThreadsUnsupported) {
		t.Fatalf("want ErrThreadsUnsupported, got %v", err)
	}
}

func TestResolveUnrecognized(t *testing.T) {
	s := &Service{}
	_, err := s.Resolve(context.Background(), "hello world")
	if !errors.Is(err, ErrUnrecognized) {
		t.Fatalf("want ErrUnrecognized, got %v", err)
	}
}
