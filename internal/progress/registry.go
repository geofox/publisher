package progress

import (
	"context"
	"sync"
)

// Sink is the emit surface the dispatch path uses. Hub implements it; a no-op
// implementation is returned when no sink is in context.
type Sink interface {
	Platform(plat, status, detail, url string)
	RelaysQueued(plat string, urls []string)
	Relay(plat, url, status, msg string)
}

type noopSink struct{}

func (noopSink) Platform(string, string, string, string) {}
func (noopSink) RelaysQueued(string, []string)           {}
func (noopSink) Relay(string, string, string, string)    {}

type sinkKey struct{}

// WithSink returns a context carrying s, so deep callees (e.g. the Nostr
// publisher) can emit without new function-signature plumbing.
func WithSink(ctx context.Context, s Sink) context.Context {
	return context.WithValue(ctx, sinkKey{}, s)
}

// SinkFrom returns the context's sink, or a no-op sink when absent.
func SinkFrom(ctx context.Context) Sink {
	if s, ok := ctx.Value(sinkKey{}).(Sink); ok && s != nil {
		return s
	}
	return noopSink{}
}

// Registry maps post IDs to live hubs.
type Registry struct {
	mu   sync.Mutex
	hubs map[string]*Hub
}

func NewRegistry() *Registry { return &Registry{hubs: map[string]*Hub{}} }

// Create registers a hub for postID, seeding one queued platform row per
// platform (native marks the reply/quote target for interactions; "" for plain
// posts).
func (r *Registry) Create(postID string, platforms []string, native string) *Hub {
	ps := make([]PlatformState, len(platforms))
	for i, p := range platforms {
		ps[i] = PlatformState{Platform: p, Status: StatusQueued, Native: p == native && native != ""}
	}
	h := newHub(postID, ps)
	r.mu.Lock()
	r.hubs[postID] = h
	r.mu.Unlock()
	return h
}

// Get returns the hub for postID if present.
func (r *Registry) Get(postID string) (*Hub, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.hubs[postID]
	return h, ok
}

// Remove deletes the hub for postID.
func (r *Registry) Remove(postID string) {
	r.mu.Lock()
	delete(r.hubs, postID)
	r.mu.Unlock()
}
