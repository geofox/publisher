package progress

import (
	"context"
	"testing"
)

func TestRegistryCreateGetRemove(t *testing.T) {
	r := NewRegistry()
	h := r.Create("p1", []string{"bluesky", "nostr"}, "")
	if got, ok := r.Get("p1"); !ok || got != h {
		t.Fatalf("Get did not return created hub")
	}
	r.Remove("p1")
	if _, ok := r.Get("p1"); ok {
		t.Fatalf("hub still present after Remove")
	}
}

func TestCreateSeedsNativePlatform(t *testing.T) {
	r := NewRegistry()
	h := r.Create("p2", []string{"nostr", "bluesky"}, "nostr")
	cur, _, _ := h.Subscribe()
	if cur.Platforms[0].Platform != "nostr" || !cur.Platforms[0].Native {
		t.Fatalf("native platform not marked: %+v", cur.Platforms[0])
	}
	if cur.Platforms[1].Native {
		t.Fatalf("non-native platform wrongly marked native")
	}
}

func TestSinkFromContextNoopWhenAbsent(t *testing.T) {
	SinkFrom(context.Background()).Platform("nostr", StatusRunning, "", "")
}

func TestWithSinkRoundTrips(t *testing.T) {
	r := NewRegistry()
	h := r.Create("p3", []string{"nostr"}, "")
	ctx := WithSink(context.Background(), h)
	_, ch, cancel := h.Subscribe()
	defer cancel()
	SinkFrom(ctx).Platform("nostr", StatusSuccess, "", "")
	if got := <-ch; got.Platforms[0].Status != StatusSuccess {
		t.Fatalf("sink-from-context did not reach hub: %+v", got.Platforms[0])
	}
}
