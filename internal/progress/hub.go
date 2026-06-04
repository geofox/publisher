package progress

import "sync"

// Hub holds the live snapshot for one post and fans coalesced snapshots out to
// subscribers. All mutations go through the sink methods (Platform/RelaysQueued/
// Relay) and Close. Safe for concurrent use.
type Hub struct {
	mu   sync.Mutex
	snap Snapshot
	subs map[chan Snapshot]struct{}
	done bool
}

func newHub(postID string, platforms []PlatformState) *Hub {
	return &Hub{
		snap: Snapshot{PostID: postID, Status: StatusRunning, Platforms: platforms},
		subs: map[chan Snapshot]struct{}{},
	}
}

func (h *Hub) platform(name string) *PlatformState {
	for i := range h.snap.Platforms {
		if h.snap.Platforms[i].Platform == name {
			return &h.snap.Platforms[i]
		}
	}
	return nil
}

func (h *Hub) relay(p *PlatformState, url string) *RelayState {
	for i := range p.Relays {
		if p.Relays[i].URL == url {
			return &p.Relays[i]
		}
	}
	return nil
}

// Platform sets a platform row's status (and detail/url when non-empty), then
// recomputes the overall status and broadcasts.
func (h *Hub) Platform(plat, status, detail, url string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if p := h.platform(plat); p != nil {
		p.Status = status
		if detail != "" {
			p.Detail = detail
		}
		if url != "" {
			p.URL = url
		}
	}
	h.snap.recomputeStatus()
	h.broadcastLocked()
}

// RelaysQueued registers the full attempted relay set as queued rows (so the UI
// can show pending relays before any resolve).
func (h *Hub) RelaysQueued(plat string, urls []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if p := h.platform(plat); p != nil {
		for _, u := range urls {
			if h.relay(p, u) == nil {
				p.Relays = append(p.Relays, RelayState{URL: u, Status: StatusQueued})
			}
		}
	}
	h.broadcastLocked()
}

// Relay sets one relay row's status (and message when non-empty).
func (h *Hub) Relay(plat, url, status, msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if p := h.platform(plat); p != nil {
		r := h.relay(p, url)
		if r == nil {
			p.Relays = append(p.Relays, RelayState{URL: url})
			r = &p.Relays[len(p.Relays)-1]
		}
		r.Status = status
		if msg != "" {
			r.Message = msg
		}
	}
	h.broadcastLocked()
}

// Subscribe returns the current snapshot, a channel of subsequent snapshots, and
// an unsubscribe func. If the hub is already done, the channel is returned
// closed (the caller uses the returned snapshot as the terminal value).
func (h *Hub) Subscribe() (Snapshot, <-chan Snapshot, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan Snapshot, 1)
	if h.done {
		close(ch)
		return h.snap.clone(), ch, func() {}
	}
	h.subs[ch] = struct{}{}
	cancel := func() {
		h.mu.Lock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
		}
		h.mu.Unlock()
	}
	return h.snap.clone(), ch, cancel
}

// Close marks the hub terminal (final overrides the computed status when set),
// sends a last snapshot to every subscriber, and closes their channels.
func (h *Hub) Close(final string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.done {
		return
	}
	h.done = true
	if final != "" {
		h.snap.Status = final
	} else {
		h.snap.recomputeStatus()
	}
	snap := h.snap.clone()
	for ch := range h.subs {
		drain(ch)
		ch <- snap
		close(ch)
		delete(h.subs, ch)
	}
}

// broadcastLocked sends the latest snapshot to all subscribers, coalescing: a
// subscriber that hasn't read its buffered value gets it replaced by the newer
// one. Caller holds h.mu.
func (h *Hub) broadcastLocked() {
	snap := h.snap.clone()
	for ch := range h.subs {
		drain(ch)
		ch <- snap
	}
}

func drain(ch chan Snapshot) {
	select {
	case <-ch:
	default:
	}
}
