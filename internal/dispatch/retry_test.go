package dispatch

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gonostr "fiatjaf.com/nostr"
	"github.com/geofox/publisher/internal/store"
)

type fakeFetcher struct{}

func (fakeFetcher) Fetch(ctx context.Context, url string) ([]byte, string, error) {
	return []byte("bytes"), "image/png", nil
}

type retryMasto struct{ calls int }

func (m *retryMasto) PostText(ctx context.Context, text string, o Overrides, imgs []Img, replyTo *ReplyRef) (TargetResult, error) {
	m.calls++
	return TargetResult{Platform: "mastodon", Status: "success", RemoteID: "st2", RemoteURL: "https://m/2"}, nil
}
func (m *retryMasto) Reblog(context.Context, string) (TargetResult, error) {
	return TargetResult{Platform: "mastodon", Status: "success"}, nil
}
func (m *retryMasto) QuoteStatus(context.Context, string, string, []Img) (TargetResult, error) {
	return TargetResult{Platform: "mastodon", Status: "success"}, nil
}

func TestDispatcherRetry(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	_ = db.SavePost(&store.Post{
		ID: "p1", CreatedAt: time.Now().UTC(), Platforms: []string{"mastodon", "nostr"}, Source: "web", Status: "partial",
		Targets: []store.Target{
			{Platform: "mastodon", Status: "failed", FinalText: "hi", FieldsJSON: `{"visibility":"public"}`,
				Attempts: []store.Attempt{{AttemptNo: 1, Status: "failed", Error: "boom", AttemptedAt: time.Now()}}},
			{Platform: "nostr", Status: "success", FinalText: "hi", RemoteID: "ev1",
				Attempts: []store.Attempt{{AttemptNo: 1, Status: "success", AttemptedAt: time.Now()}}},
		},
	})

	mock := &retryMasto{}
	d := &Dispatcher{Mastodon: mock, Store: db, Fetcher: fakeFetcher{}}
	post, err := d.Retry(context.Background(), "p1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if mock.calls != 1 {
		t.Errorf("mastodon retried %d times, want 1 (nostr was successful, must be skipped)", mock.calls)
	}
	if post.Status != "success" {
		t.Errorf("post status = %q, want success", post.Status)
	}
	for _, tg := range post.Targets {
		if tg.Platform == "mastodon" && (tg.Status != "success" || tg.AttemptCount != 2) {
			t.Errorf("mastodon target not retried: %+v", tg)
		}
	}
}

func TestRetryNostrImeta(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	_ = db.SavePost(&store.Post{
		ID: "p3", CreatedAt: time.Now().UTC(), Platforms: []string{"nostr"}, Source: "web", Status: "failed",
		Media: []store.Media{{Ordinal: 0, BlossomURL: "https://b/z", SHA256: "beef", Mime: "image/png", Dim: "5x5"}},
		Targets: []store.Target{
			{Platform: "nostr", Status: "failed", FinalText: "hi",
				Attempts: []store.Attempt{{AttemptNo: 1, Status: "failed", Error: "boom", AttemptedAt: time.Now()}}},
		},
	})
	cn := &capturingNostr{}
	d := &Dispatcher{Nostr: cn, Store: db, Fetcher: fakeFetcher{}}
	post, err := d.Retry(context.Background(), "p3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if post.Status != "success" {
		t.Errorf("status = %q, want success", post.Status)
	}
	if len(cn.gotImetas) != 1 {
		t.Fatalf("retry passed %d imeta tags, want 1 (rebuilt from archived media)", len(cn.gotImetas))
	}
	if got := strings.Join(cn.gotImetas[0], " "); got != "imeta url https://b/z m image/png x beef dim 5x5" {
		t.Errorf("retry imeta = %q", got)
	}
}

type retryBsky struct{ calls int }

func (b *retryBsky) PostBsky(ctx context.Context, text string, o Overrides, imgs []Img, replyTo *ReplyRef) (TargetResult, error) {
	b.calls++
	return TargetResult{Platform: "bluesky", Status: "success", RemoteID: "bb"}, nil
}
func (b *retryBsky) RepostBsky(context.Context, string, string) (TargetResult, error) {
	return TargetResult{Platform: "bluesky", Status: "success"}, nil
}
func (b *retryBsky) QuoteBsky(context.Context, string, Overrides, []Img, string, string) (TargetResult, error) {
	return TargetResult{Platform: "bluesky", Status: "success"}, nil
}

type relayFakeNostr struct {
	rebroadcastOK bool
	gotRelay      string
}

func (f *relayFakeNostr) PublishText(ctx context.Context, text string, pow *int, imetas []gonostr.Tag, replyTo *ReplyRef) (TargetResult, error) {
	return TargetResult{Platform: "nostr", Status: "success"}, nil
}
func (f *relayFakeNostr) Repost(context.Context, string, string, int, string) (TargetResult, error) {
	return TargetResult{Platform: "nostr", Status: "success"}, nil
}
func (f *relayFakeNostr) Quote(context.Context, string, string, string, string, []gonostr.Tag) (TargetResult, error) {
	return TargetResult{Platform: "nostr", Status: "success"}, nil
}
func (f *relayFakeNostr) RebroadcastToRelay(ctx context.Context, signedEventJSON, relayURL string) (bool, string) {
	f.gotRelay = relayURL
	if f.rebroadcastOK {
		return true, ""
	}
	return false, "still failing"
}

func TestDispatcherRetryRelay(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	_ = db.SavePost(&store.Post{
		ID: "p9", CreatedAt: time.Now().UTC(), Platforms: []string{"nostr"}, Source: "web", Status: "partial",
		Targets: []store.Target{{
			Platform: "nostr", Status: "partial", FinalText: "hi",
			SignedEventJSON: `{"id":"ev","sig":"x"}`,
			Relays: []store.RelayState{
				{URL: "wss://relay.geoffrey.one", Status: "ok"},
				{URL: "wss://relay.damus.io", Status: "failed", Message: "503"},
			},
			Attempts: []store.Attempt{{AttemptNo: 1, Status: "partial", AttemptedAt: time.Now()}},
		}},
	})
	fake := &relayFakeNostr{rebroadcastOK: true}
	d := &Dispatcher{Nostr: fake, Store: db}
	post, err := d.RetryRelay(context.Background(), "p9", "wss://relay.damus.io")
	if err != nil {
		t.Fatal(err)
	}
	if fake.gotRelay != "wss://relay.damus.io" {
		t.Errorf("rebroadcast to %q, want relay.damus.io", fake.gotRelay)
	}
	if post.Status != "success" {
		t.Errorf("post status = %q, want success", post.Status)
	}
}

func TestRetryRelayStillFailing(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	_ = db.SavePost(&store.Post{
		ID: "p10", CreatedAt: time.Now().UTC(), Platforms: []string{"nostr"}, Source: "web", Status: "partial",
		Targets: []store.Target{{
			Platform: "nostr", Status: "partial", FinalText: "hi", SignedEventJSON: `{"id":"ev"}`,
			Relays: []store.RelayState{
				{URL: "wss://relay.geoffrey.one", Status: "ok"},
				{URL: "wss://relay.damus.io", Status: "failed", Message: "503"},
			},
			Attempts: []store.Attempt{{AttemptNo: 1, Status: "partial", AttemptedAt: time.Now()}},
		}},
	})
	d := &Dispatcher{Nostr: &relayFakeNostr{rebroadcastOK: false}, Store: db}
	post, err := d.RetryRelay(context.Background(), "p10", "wss://relay.damus.io")
	if err != nil {
		t.Fatal(err)
	}
	if post.Status != "partial" {
		t.Errorf("post status = %q, want partial (relay still failing)", post.Status)
	}
}

func TestRetryRelayRejectsIneligible(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	_ = db.SavePost(&store.Post{
		ID: "p11", CreatedAt: time.Now().UTC(), Platforms: []string{"nostr"}, Source: "web", Status: "partial",
		Targets: []store.Target{{
			Platform: "nostr", Status: "partial", FinalText: "hi", SignedEventJSON: `{"id":"ev"}`,
			Relays: []store.RelayState{
				{URL: "wss://relay.geoffrey.one", Status: "ok"},
				{URL: "ws://x.onion", Status: "skipped"},
			},
			Attempts: []store.Attempt{{AttemptNo: 1, Status: "partial", AttemptedAt: time.Now()}},
		}},
	})
	d := &Dispatcher{Nostr: &relayFakeNostr{rebroadcastOK: true}, Store: db}
	for _, relay := range []string{"wss://unknown.relay", "ws://x.onion", "wss://relay.geoffrey.one"} {
		if _, err := d.RetryRelay(context.Background(), "p11", relay); !errors.Is(err, ErrBadRelayRetry) {
			t.Errorf("RetryRelay(%q) err = %v, want ErrBadRelayRetry", relay, err)
		}
	}
}

// refreshNostr returns a fresh successful publish (new event + all relays ok),
// so a whole-platform retry of a previously-failed nostr target must refresh
// the stored relay rows and signed event.
type refreshNostr struct{}

func (refreshNostr) PublishText(ctx context.Context, text string, pow *int, imetas []gonostr.Tag, replyTo *ReplyRef) (TargetResult, error) {
	return TargetResult{
		Platform: "nostr", Status: "success", RemoteID: "newev", SignedEventJSON: `{"id":"newev"}`,
		Relays: []store.RelayState{
			{URL: "wss://relay.geoffrey.one", Status: "ok"},
			{URL: "wss://relay.damus.io", Status: "ok"},
		},
	}, nil
}
func (refreshNostr) RebroadcastToRelay(ctx context.Context, signedEventJSON, relayURL string) (bool, string) {
	return true, ""
}
func (refreshNostr) Repost(context.Context, string, string, int, string) (TargetResult, error) {
	return TargetResult{Platform: "nostr", Status: "success"}, nil
}
func (refreshNostr) Quote(context.Context, string, string, string, string, []gonostr.Tag) (TargetResult, error) {
	return TargetResult{Platform: "nostr", Status: "success"}, nil
}

func TestRetryNostrRefreshesRelays(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	_ = db.SavePost(&store.Post{
		ID: "p12", CreatedAt: time.Now().UTC(), Platforms: []string{"nostr"}, Source: "web", Status: "failed",
		Targets: []store.Target{{
			Platform: "nostr", Status: "failed", FinalText: "hi", SignedEventJSON: `{"id":"oldev"}`,
			Relays: []store.RelayState{
				{URL: "wss://relay.geoffrey.one", Status: "failed"},
				{URL: "wss://relay.damus.io", Status: "failed"},
			},
			Attempts: []store.Attempt{{AttemptNo: 1, Status: "failed", AttemptedAt: time.Now()}},
		}},
	})
	d := &Dispatcher{Nostr: refreshNostr{}, Store: db, Fetcher: fakeFetcher{}}
	post, err := d.Retry(context.Background(), "p12", []string{"nostr"})
	if err != nil {
		t.Fatal(err)
	}
	tg := post.Targets[0]
	if tg.Status != "success" {
		t.Errorf("target status = %q, want success", tg.Status)
	}
	if tg.SignedEventJSON != `{"id":"newev"}` {
		t.Errorf("signed event not refreshed: %q", tg.SignedEventJSON)
	}
	for _, r := range tg.Relays {
		if r.Status != "ok" {
			t.Errorf("relay %s stale: status=%q, want ok", r.URL, r.Status)
		}
	}
}

func TestRetryPlatformFilter(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	_ = db.SavePost(&store.Post{
		ID: "p2", CreatedAt: time.Now().UTC(), Platforms: []string{"mastodon", "bluesky"}, Source: "web", Status: "failed",
		Targets: []store.Target{
			{Platform: "mastodon", Status: "failed", FinalText: "hi",
				Attempts: []store.Attempt{{AttemptNo: 1, Status: "failed", AttemptedAt: time.Now()}}},
			{Platform: "bluesky", Status: "failed", FinalText: "hi",
				Attempts: []store.Attempt{{AttemptNo: 1, Status: "failed", AttemptedAt: time.Now()}}},
		},
	})
	masto := &retryMasto{}
	bsky := &retryBsky{}
	d := &Dispatcher{Mastodon: masto, Bluesky: bsky, Store: db, Fetcher: fakeFetcher{}}
	post, err := d.Retry(context.Background(), "p2", []string{"bluesky"})
	if err != nil {
		t.Fatal(err)
	}
	if masto.calls != 0 {
		t.Errorf("mastodon should NOT be retried (filtered out), got %d calls", masto.calls)
	}
	if bsky.calls != 1 {
		t.Errorf("bluesky should be retried once, got %d", bsky.calls)
	}
	if post.Status != "partial" {
		t.Errorf("post status = %q, want partial (mastodon still failed, bluesky now success)", post.Status)
	}
}
