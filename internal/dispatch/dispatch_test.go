package dispatch

import (
	"context"
	"errors"
	"strings"
	"testing"

	gonostr "fiatjaf.com/nostr"
	"github.com/geofox/publisher/internal/store"
)

type fakeNostr struct{ fail bool }

func (f fakeNostr) PublishText(ctx context.Context, text string, pow *int, imetas []gonostr.Tag, replyTo *ReplyRef) (TargetResult, error) {
	if f.fail {
		return TargetResult{Platform: "nostr", Status: "failed", Error: "boom"}, errors.New("boom")
	}
	return TargetResult{Platform: "nostr", Status: "success", RemoteID: "ev1"}, nil
}

func (fakeNostr) RebroadcastToRelay(ctx context.Context, signedEventJSON, relayURL string) (bool, string) {
	return true, ""
}
func (fakeNostr) Repost(context.Context, string, string, int, string) (TargetResult, error) {
	return TargetResult{Platform: "nostr", Status: "success"}, nil
}
func (fakeNostr) Quote(context.Context, string, string, string, string, []gonostr.Tag) (TargetResult, error) {
	return TargetResult{Platform: "nostr", Status: "success"}, nil
}

// capturingNostr records the imeta tags it was handed so tests can assert the
// dispatcher rebuilt NIP-92 tags from the archived media records.
type capturingNostr struct{ gotImetas []gonostr.Tag }

func (c *capturingNostr) PublishText(ctx context.Context, text string, pow *int, imetas []gonostr.Tag, replyTo *ReplyRef) (TargetResult, error) {
	c.gotImetas = imetas
	return TargetResult{Platform: "nostr", Status: "success", RemoteID: "ev1"}, nil
}

func (*capturingNostr) RebroadcastToRelay(ctx context.Context, signedEventJSON, relayURL string) (bool, string) {
	return true, ""
}
func (*capturingNostr) Repost(context.Context, string, string, int, string) (TargetResult, error) {
	return TargetResult{Platform: "nostr", Status: "success"}, nil
}
func (*capturingNostr) Quote(context.Context, string, string, string, string, []gonostr.Tag) (TargetResult, error) {
	return TargetResult{Platform: "nostr", Status: "success"}, nil
}

func TestDispatchNostrImeta(t *testing.T) {
	cn := &capturingNostr{}
	d := &Dispatcher{Nostr: cn}
	rec := d.Post(context.Background(), PostSpec{
		MasterText: "hi", Platforms: []string{"nostr"}, Source: "web",
		MediaRecords: []store.Media{
			{Ordinal: 0, BlossomURL: "https://b/x", SHA256: "deadbeef", Mime: "image/png", Dim: "10x10", Blurhash: "LEH"},
			{Ordinal: 1, BlossomURL: "https://b/y", SHA256: "cafe", Mime: "image/jpeg"},
		},
	})
	if rec.Status != "success" {
		t.Fatalf("status = %q, want success", rec.Status)
	}
	if len(cn.gotImetas) != 2 {
		t.Fatalf("nostr got %d imeta tags, want 2 (one per image)", len(cn.gotImetas))
	}
	if got := strings.Join(cn.gotImetas[0], " "); got != "imeta url https://b/x m image/png x deadbeef dim 10x10 blurhash LEH" {
		t.Errorf("imeta[0] = %q", got)
	}
	if got := strings.Join(cn.gotImetas[1], " "); got != "imeta url https://b/y m image/jpeg x cafe" {
		t.Errorf("imeta[1] = %q", got)
	}
}

func TestDispatchNostrNoMedia(t *testing.T) {
	cn := &capturingNostr{}
	d := &Dispatcher{Nostr: cn}
	d.Post(context.Background(), PostSpec{MasterText: "hi", Platforms: []string{"nostr"}, Source: "web"})
	if len(cn.gotImetas) != 0 {
		t.Errorf("text-only post should pass no imeta, got %v", cn.gotImetas)
	}
}

type fakeMasto struct{}

func (fakeMasto) PostText(ctx context.Context, text string, o Overrides, imgs []Img, replyTo *ReplyRef) (TargetResult, error) {
	return TargetResult{Platform: "mastodon", Status: "success", RemoteID: "st1", RemoteURL: "https://m/1"}, nil
}
func (fakeMasto) Reblog(context.Context, string) (TargetResult, error) {
	return TargetResult{Platform: "mastodon", Status: "success"}, nil
}
func (fakeMasto) QuoteStatus(context.Context, string, string, []Img) (TargetResult, error) {
	return TargetResult{Platform: "mastodon", Status: "success"}, nil
}

func TestDispatchFanOut(t *testing.T) {
	d := &Dispatcher{Nostr: fakeNostr{fail: true}, Mastodon: fakeMasto{}}
	rec := d.Post(context.Background(), PostSpec{
		MasterText: "hi", Platforms: []string{"nostr", "mastodon"}, Source: "web",
	})
	if rec.Status != "partial" {
		t.Errorf("status = %q, want partial", rec.Status)
	}
	if len(rec.Targets) != 2 {
		t.Fatalf("targets = %d", len(rec.Targets))
	}
	if rec.Targets[0].Platform != "nostr" || rec.Targets[1].Platform != "mastodon" {
		t.Errorf("targets not in platform order: %+v", rec.Targets)
	}
	if rec.Targets[0].Attempts[0].Error == "" {
		t.Errorf("failed nostr target missing error")
	}
}

func TestDispatchEmptyPlatforms(t *testing.T) {
	d := &Dispatcher{}
	rec := d.Post(context.Background(), PostSpec{MasterText: "hi", Platforms: nil, Source: "web"})
	if rec.Status != "failed" {
		t.Errorf("empty platforms status = %q, want failed", rec.Status)
	}
}

func TestDispatchNilAdapter(t *testing.T) {
	d := &Dispatcher{} // Nostr nil
	rec := d.Post(context.Background(), PostSpec{MasterText: "hi", Platforms: []string{"nostr"}, Source: "web"})
	if rec.Status != "failed" || rec.Targets[0].Attempts[0].Error == "" {
		t.Errorf("nil adapter not handled: %+v", rec)
	}
}

func TestDispatchDedupsPlatforms(t *testing.T) {
	d := &Dispatcher{Nostr: fakeNostr{}}
	rec := d.Post(context.Background(), PostSpec{
		MasterText: "hi", Platforms: []string{"nostr", "nostr"}, Source: "web",
	})
	if len(rec.Targets) != 1 {
		t.Errorf("expected 1 deduped target, got %d", len(rec.Targets))
	}
}

type simpleFakeBsky struct{}

func (simpleFakeBsky) PostBsky(ctx context.Context, text string, o Overrides, imgs []Img, replyTo *ReplyRef) (TargetResult, error) {
	return TargetResult{Platform: "bluesky", Status: "success", RemoteID: "at://x", RemoteURL: "https://bsky.app/x"}, nil
}
func (simpleFakeBsky) RepostBsky(context.Context, string, string) (TargetResult, error) {
	return TargetResult{Platform: "bluesky", Status: "success"}, nil
}
func (simpleFakeBsky) QuoteBsky(context.Context, string, Overrides, []Img, string, string) (TargetResult, error) {
	return TargetResult{Platform: "bluesky", Status: "success"}, nil
}

func TestDispatchBluesky(t *testing.T) {
	d := &Dispatcher{Bluesky: simpleFakeBsky{}}
	rec := d.Post(context.Background(), PostSpec{
		MasterText: "hi", Platforms: []string{"bluesky"}, Source: "web",
		Overrides: map[string]Overrides{"bluesky": {Langs: []string{"en"}}},
	})
	if rec.Status != "success" || rec.Targets[0].Platform != "bluesky" {
		t.Fatalf("bluesky dispatch: %+v", rec)
	}
}

type fakeThreads struct{}

func (fakeThreads) PostThreads(ctx context.Context, text string, o Overrides, imgs []Img, replyTo *ReplyRef) (TargetResult, error) {
	return TargetResult{Platform: "threads", Status: "success", RemoteID: "med1", RemoteURL: "https://t/p"}, nil
}

func TestDispatchThreads(t *testing.T) {
	d := &Dispatcher{Threads: fakeThreads{}}
	rec := d.Post(context.Background(), PostSpec{
		MasterText: "hi", Platforms: []string{"threads"}, Source: "web",
		Overrides: map[string]Overrides{"threads": {TopicTag: "golang"}},
	})
	if rec.Status != "success" || rec.Targets[0].Platform != "threads" {
		t.Fatalf("threads dispatch: %+v", rec)
	}
}

type partialNostr struct{}

func (partialNostr) PublishText(ctx context.Context, text string, pow *int, imetas []gonostr.Tag, replyTo *ReplyRef) (TargetResult, error) {
	return TargetResult{
		Platform: "nostr", Status: "partial", RemoteID: "ev", RemoteURL: "https://njump.me/x",
		Relays: []store.RelayState{{URL: "wss://a", Status: "ok"}, {URL: "wss://b", Status: "failed"}},
	}, nil
}
func (partialNostr) RebroadcastToRelay(ctx context.Context, signedEventJSON, relayURL string) (bool, string) {
	return true, ""
}
func (partialNostr) Repost(context.Context, string, string, int, string) (TargetResult, error) {
	return TargetResult{Platform: "nostr", Status: "success"}, nil
}
func (partialNostr) Quote(context.Context, string, string, string, string, []gonostr.Tag) (TargetResult, error) {
	return TargetResult{Platform: "nostr", Status: "success"}, nil
}

// A single partial target must make the whole post partial (not failed) — the
// in-memory rec.Status returned to the API must match store.recomputeStatus.
func TestPostPartialTarget(t *testing.T) {
	d := &Dispatcher{Nostr: partialNostr{}}
	rec := d.Post(context.Background(), PostSpec{MasterText: "hi", Platforms: []string{"nostr"}, Source: "web"})
	if rec.Status != "partial" {
		t.Errorf("post status = %q, want partial", rec.Status)
	}
	if rec.Targets[0].Status != "partial" {
		t.Errorf("target status = %q, want partial", rec.Targets[0].Status)
	}
}

func TestNostrStatusFromRelays(t *testing.T) {
	cases := []struct {
		in   []store.RelayState
		want string
	}{
		{[]store.RelayState{{Status: "ok"}, {Status: "ok"}}, "success"},
		{[]store.RelayState{{Status: "ok"}, {Status: "failed"}}, "partial"},
		{[]store.RelayState{{Status: "failed"}, {Status: "failed"}}, "failed"},
		{[]store.RelayState{{Status: "ok"}, {Status: "ok"}, {Status: "skipped"}}, "success"}, // skipped excluded
		{[]store.RelayState{{Status: "failed"}, {Status: "skipped"}}, "failed"},
		{[]store.RelayState{{Status: "skipped"}}, "failed"}, // nothing attempted
	}
	for _, c := range cases {
		if got := nostrStatusFromRelays(c.in); got != c.want {
			t.Errorf("status(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPostAlertsOnFailure(t *testing.T) {
	st := openDispatchStore(t)
	al := &fakeAlerter{} // defined in retrier_test.go (same package)
	d := &Dispatcher{Store: st, Alerter: al} // no adapters → bluesky post fails
	rec := d.Post(context.Background(), PostSpec{
		MasterText: "hello", Platforms: []string{"bluesky"},
	})
	if rec.Status == "success" {
		t.Fatal("expected a non-success post in this no-adapter test")
	}
	if len(al.calls) != 1 {
		t.Errorf("expected one immediate-failure alert, got %d", len(al.calls))
	}
}
