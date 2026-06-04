package dispatch

import (
	"context"
	"sync"
	"testing"

	"github.com/geofox/publisher/internal/progress"
)

// capturingSink records Platform calls for assertions.
type capturingSink struct {
	mu        sync.Mutex
	platforms [][3]string // plat, status, detail
}

func (s *capturingSink) Platform(plat, status, detail, _ string) {
	s.mu.Lock()
	s.platforms = append(s.platforms, [3]string{plat, status, detail})
	s.mu.Unlock()
}
func (s *capturingSink) RelaysQueued(string, []string)        {}
func (s *capturingSink) Relay(string, string, string, string) {}

func (s *capturingSink) has(plat, status string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.platforms {
		if e[0] == plat && e[1] == status {
			return true
		}
	}
	return false
}

// TestPostEmitsRunningThenTerminal verifies that Post emits StatusRunning before
// calling runChain and a terminal status afterward.
func TestPostEmitsRunningThenTerminal(t *testing.T) {
	d := &Dispatcher{Bluesky: simpleFakeBsky{}}
	sink := &capturingSink{}
	ctx := progress.WithSink(context.Background(), sink)
	rec := d.Post(ctx, PostSpec{Platforms: []string{"bluesky"}, MasterText: "hi"})
	if rec.Status != "success" {
		t.Fatalf("post status = %q, want success", rec.Status)
	}
	if !sink.has("bluesky", progress.StatusRunning) {
		t.Error("expected a StatusRunning emission for bluesky, got none")
	}
	if !sink.has("bluesky", progress.StatusSuccess) {
		t.Error("expected a StatusSuccess emission for bluesky, got none")
	}
}

// TestPostEmitsFailedOnError verifies that a failed platform emits StatusFailed.
func TestPostEmitsFailedOnError(t *testing.T) {
	d := &Dispatcher{Nostr: fakeNostr{fail: true}}
	sink := &capturingSink{}
	ctx := progress.WithSink(context.Background(), sink)
	d.Post(ctx, PostSpec{Platforms: []string{"nostr"}, MasterText: "hi"})
	if !sink.has("nostr", progress.StatusRunning) {
		t.Error("expected StatusRunning for nostr")
	}
	if !sink.has("nostr", progress.StatusFailed) {
		t.Error("expected StatusFailed for nostr")
	}
}

// TestInteractEmitsRunningThenTerminal verifies that Interact (reply path) emits
// running before and a terminal status after runChain.
func TestInteractEmitsRunningThenTerminal(t *testing.T) {
	d := &Dispatcher{Bluesky: &fakeBskyActor{}}
	sink := &capturingSink{}
	ctx := progress.WithSink(context.Background(), sink)
	post := d.Interact(ctx, InteractSpec{
		Action: actionReply, SourcePlatform: "bluesky",
		Ref:  InteractRef{URI: "at://x", CID: "cx"},
		Text: "great",
	})
	if post.Status != "success" {
		t.Fatalf("interact status = %q, want success", post.Status)
	}
	if !sink.has("bluesky", progress.StatusRunning) {
		t.Error("expected StatusRunning emission for bluesky in Interact")
	}
	if !sink.has("bluesky", progress.StatusSuccess) {
		t.Error("expected StatusSuccess emission for bluesky in Interact")
	}
}

// TestInteractRepostEmitsProgress verifies that the repost path also emits progress.
func TestInteractRepostEmitsProgress(t *testing.T) {
	d := &Dispatcher{Bluesky: &fakeBskyActor{}}
	sink := &capturingSink{}
	ctx := progress.WithSink(context.Background(), sink)
	d.Interact(ctx, InteractSpec{
		Action: actionRepost, SourcePlatform: "bluesky",
		Ref: InteractRef{URI: "at://x", CID: "cx"},
	})
	if !sink.has("bluesky", progress.StatusRunning) {
		t.Error("expected StatusRunning emission for bluesky repost")
	}
	if !sink.has("bluesky", progress.StatusSuccess) {
		t.Error("expected StatusSuccess emission for bluesky repost")
	}
}

// TestRunChainEmitsThreadCounter verifies that a multi-segment chain emits
// per-segment running updates with a "thread k/n" detail.
func TestRunChainEmitsThreadCounter(t *testing.T) {
	f := &fakeBsky{failAt: -1}
	d := &Dispatcher{Bluesky: f}
	sink := &capturingSink{}
	ctx := progress.WithSink(context.Background(), sink)
	// "aaa\n---\nbbb\n---\nccc" splits into 3 segments via the --- markers.
	out := d.runChain(ctx, "bluesky", "aaa\n---\nbbb\n---\nccc", Overrides{}, nil, nil, false, nil)
	if out.Status != "success" {
		t.Fatalf("chain status = %q", out.Status)
	}
	// Must have seen at least one "thread k/3" running update.
	sink.mu.Lock()
	defer sink.mu.Unlock()
	var found bool
	for _, e := range sink.platforms {
		if e[0] == "bluesky" && e[1] == progress.StatusRunning && len(e[2]) > 0 {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected at least one thread-counter running emission; got %v", sink.platforms)
	}
}
