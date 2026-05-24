package verify

import (
	"context"
	"testing"
)

func TestDetectPlatform(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{goodEvent, "nostr"},
		{`  {"id":"x","pubkey":"y","sig":"z"}  `, "nostr"},
		{"at://did:plc:abc/app.bsky.feed.post/123", "bluesky"},
		{"https://bsky.app/profile/alice.bsky.social/post/3kabc", "bluesky"},
		{"https://mastodon.social/@alice/111222333", "mastodon"},
		{"https://example.social/users/bob/statuses/999", "mastodon"},
		{"https://www.threads.net/@alice/post/ABC123", "threads"},
		{"https://threads.net/@bob/post/XYZ", "threads"},
		{"https://www.threads.com/@zuck/post/DYSAIo_FL77", "threads"},
		{"https://threads.com/@carol/post/123", "threads"},
	}
	for _, c := range cases {
		got, err := detectPlatform(c.raw)
		if err != nil {
			t.Errorf("detectPlatform(%q) error: %v", c.raw, err)
			continue
		}
		if got != c.want {
			t.Errorf("detectPlatform(%q) = %s, want %s", c.raw, got, c.want)
		}
	}
}

func TestDetectNeventRejected(t *testing.T) {
	_, err := detectPlatform("nevent1qqs...")
	if err == nil {
		t.Fatal("expected nevent to be rejected with guidance")
	}
}

func TestServiceRoutesThreads(t *testing.T) {
	s := &Service{Mastodon: fakeVerifier{"mastodon"}, Threads: fakeVerifier{"threads"}}
	v := s.Verify(context.Background(), Input{Raw: "https://www.threads.net/@a/post/1"})
	if v.Platform != "threads" {
		t.Fatalf("routed to %s, want threads", v.Platform)
	}
}

type fakeVerifier struct{ platform string }

func (f fakeVerifier) Verify(_ context.Context, _ Input) Verdict {
	return Verdict{Platform: f.platform, Status: StatusVerified, Checks: []Check{}}
}

func TestServiceRoutesByDetection(t *testing.T) {
	s := &Service{
		Nostr:    fakeVerifier{"nostr"},
		Bluesky:  fakeVerifier{"bluesky"},
		Mastodon: fakeVerifier{"mastodon"},
	}
	v := s.Verify(context.Background(), Input{Raw: "https://bsky.app/profile/a/post/1"})
	if v.Platform != "bluesky" {
		t.Fatalf("routed to %s, want bluesky", v.Platform)
	}
}

func TestServiceExplicitOverride(t *testing.T) {
	s := &Service{Nostr: fakeVerifier{"nostr"}, Mastodon: fakeVerifier{"mastodon"}}
	v := s.Verify(context.Background(), Input{Raw: "anything", Platform: "mastodon"})
	if v.Platform != "mastodon" {
		t.Fatalf("override routed to %s, want mastodon", v.Platform)
	}
}

func TestServiceUnconfiguredPlatformErrors(t *testing.T) {
	// Bluesky/Mastodon verifiers are nil; a bsky URL should yield a clean
	// StatusError (not a panic, not a verified verdict).
	s := &Service{Nostr: fakeVerifier{"nostr"}}
	v := s.Verify(context.Background(), Input{Raw: "https://bsky.app/profile/a/post/1"})
	if v.Status != StatusError {
		t.Fatalf("status = %s, want error", v.Status)
	}
}

func TestDetectJSONNotNostr(t *testing.T) {
	// JSON object without pubkey/sig is not a Nostr event → detection error.
	if _, err := detectPlatform(`{"foo":"bar"}`); err == nil {
		t.Fatal("expected error for non-Nostr JSON object")
	}
}

type panicVerifier struct{}

func (panicVerifier) Verify(_ context.Context, _ Input) Verdict { panic("boom") }

func TestServiceRecoversFromPanic(t *testing.T) {
	s := &Service{Nostr: panicVerifier{}}
	v := s.Verify(context.Background(), Input{Raw: goodEvent}) // detects nostr → panicVerifier
	if v.Status != StatusError {
		t.Fatalf("status = %s, want error after panic", v.Status)
	}
	if v.Error == "" {
		t.Errorf("expected an error message describing the panic")
	}
}
