package nostr

import (
	"context"
	"errors"
	"testing"
	"time"

	gonostr "fiatjaf.com/nostr"

	"github.com/geofox/publisher/internal/progress"
)

// recordingSink captures progress emissions for assertion.
type recordingSink struct {
	queued map[string][]string
	relays [][3]string // {url, status, msg}
}

func (s *recordingSink) Platform(string, string, string, string) {}
func (s *recordingSink) RelaysQueued(plat string, urls []string) {
	if s.queued == nil {
		s.queued = map[string][]string{}
	}
	s.queued[plat] = urls
}
func (s *recordingSink) Relay(_, url, status, msg string) {
	s.relays = append(s.relays, [3]string{url, status, msg})
}

// fakePool satisfies publishPool with a pre-filled channel.
type fakePool struct {
	ch chan gonostr.PublishResult
}

func (f *fakePool) PublishMany(_ context.Context, _ []string, _ gonostr.Event) chan gonostr.PublishResult {
	return f.ch
}

func (f *fakePool) QuerySingle(_ context.Context, _ []string, _ gonostr.Filter, _ gonostr.SubscriptionOptions) *gonostr.RelayEvent {
	return nil
}

func TestPublishToRelaysEmits(t *testing.T) {
	// Build a pre-filled channel with two results: one ok, one error.
	ch := make(chan gonostr.PublishResult, 2)
	ch <- gonostr.PublishResult{RelayURL: "wss://a", Error: nil}
	ch <- gonostr.PublishResult{RelayURL: "wss://b", Error: errors.New("connection refused")}
	close(ch)

	p := &Publisher{
		cfg: Config{
			PublishTimeout: 5 * time.Second,
		},
		pool:  &fakePool{ch: ch},
		cache: map[gonostr.PubKey]relayCacheEntry{},
	}

	sink := &recordingSink{}
	ctx := progress.WithSink(context.Background(), sink)

	urls := []string{"wss://a", "wss://b"}
	results := p.publishToRelays(ctx, gonostr.Event{}, urls)

	// Verify relay results returned correctly.
	if len(results) != 2 {
		t.Fatalf("expected 2 relay results, got %d", len(results))
	}

	// (a) RelaysQueued("nostr", [both urls]) was emitted.
	queued, ok := sink.queued["nostr"]
	if !ok {
		t.Fatal("RelaysQueued was not emitted for platform 'nostr'")
	}
	if len(queued) != 2 || queued[0] != "wss://a" || queued[1] != "wss://b" {
		t.Errorf("RelaysQueued got %v, want [wss://a wss://b]", queued)
	}

	// (b) Per-relay Relay emissions.
	if len(sink.relays) != 2 {
		t.Fatalf("expected 2 Relay emissions, got %d", len(sink.relays))
	}

	// Find each relay by URL since channel order is deterministic here.
	byURL := map[string][3]string{}
	for _, r := range sink.relays {
		byURL[r[0]] = r
	}

	if r, ok := byURL["wss://a"]; !ok {
		t.Error("no Relay emission for wss://a")
	} else {
		if r[1] != progress.RelayOK {
			t.Errorf("wss://a status: got %q, want %q", r[1], progress.RelayOK)
		}
		if r[2] != "" {
			t.Errorf("wss://a msg: got %q, want empty", r[2])
		}
	}

	if r, ok := byURL["wss://b"]; !ok {
		t.Error("no Relay emission for wss://b")
	} else {
		if r[1] != progress.RelayFailed {
			t.Errorf("wss://b status: got %q, want %q", r[1], progress.RelayFailed)
		}
		if r[2] == "" {
			t.Error("wss://b msg: expected non-empty error message")
		}
	}
}
