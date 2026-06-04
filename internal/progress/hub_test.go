package progress

import "testing"

func newTestHub() *Hub {
	return newHub("p1", []PlatformState{
		{Platform: "bluesky", Status: StatusQueued},
		{Platform: "nostr", Status: StatusQueued},
	})
}

func TestHubSubscribeReceivesUpdates(t *testing.T) {
	h := newTestHub()
	cur, ch, cancel := h.Subscribe()
	defer cancel()
	if cur.Status != StatusRunning || len(cur.Platforms) != 2 {
		t.Fatalf("initial snapshot wrong: %+v", cur)
	}
	h.Platform("bluesky", StatusSuccess, "posted", "https://bsky/x")
	got := <-ch
	if got.Platforms[0].Status != StatusSuccess || got.Platforms[0].URL != "https://bsky/x" {
		t.Fatalf("update not applied: %+v", got.Platforms[0])
	}
}

func TestHubRelays(t *testing.T) {
	h := newTestHub()
	_, ch, cancel := h.Subscribe()
	defer cancel()
	h.RelaysQueued("nostr", []string{"wss://a", "wss://b"})
	<-ch
	h.Relay("nostr", "wss://a", RelayOK, "")
	got := <-ch
	np := got.Platforms[1]
	if len(np.Relays) != 2 || np.Relays[0].Status != RelayOK {
		t.Fatalf("relay state wrong: %+v", np.Relays)
	}
}

func TestHubCoalesces(t *testing.T) {
	h := newTestHub()
	_, ch, cancel := h.Subscribe()
	defer cancel()
	h.Platform("bluesky", StatusRunning, "", "")
	h.Platform("bluesky", StatusSuccess, "", "") // subscriber slow; only latest must survive
	got := <-ch
	if got.Platforms[0].Status != StatusSuccess {
		t.Fatalf("expected coalesced latest, got %q", got.Platforms[0].Status)
	}
}

func TestHubCloseClosesChannel(t *testing.T) {
	h := newTestHub()
	_, ch, _ := h.Subscribe()
	h.Close(StatusSuccess)
	for range ch {
	}
	if h.snap.Status != StatusSuccess {
		t.Fatalf("close did not set final status: %q", h.snap.Status)
	}
}

func TestHubLateSubscribeAfterClose(t *testing.T) {
	h := newTestHub()
	h.Close(StatusSuccess)
	cur, ch, _ := h.Subscribe()
	if cur.Status != StatusSuccess {
		t.Fatalf("late snapshot wrong: %q", cur.Status)
	}
	if _, ok := <-ch; ok {
		t.Fatalf("late channel should be closed")
	}
}
