package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	gonostr "fiatjaf.com/nostr"

	"github.com/geofox/publisher/internal/bluesky"
	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/mastodon"
	"github.com/geofox/publisher/internal/nostr"
	"github.com/geofox/publisher/internal/threads"
)

// call is one request a fake platform received — what the real driver would
// have sent upstream.
type call struct {
	Platform string     `json:"platform"`
	Action   string     `json:"action"` // post | reply | repost | quote
	Text     string     `json:"text"`
	Images   []imageRef `json:"images,omitempty"`
	ReplyTo  string     `json:"reply_to,omitempty"` // parent remote id for chained posts
	RemoteID string     `json:"remote_id,omitempty"`
}

type imageRef struct {
	Mime string `json:"mime"`
	Alt  string `json:"alt,omitempty"`
	Size int    `json:"size"`
}

// recorder holds every call plus the set of platforms the tests marked down.
type recorder struct {
	mu    sync.Mutex
	calls []call
	down  map[string]bool
	seq   int
}

func newRecorder() *recorder { return &recorder{down: map[string]bool{}} }

// record stores a call and returns its fake remote id, or an error when the
// platform is marked down.
func (r *recorder) record(c call) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.down[c.Platform] {
		return "", fmt.Errorf("e2e: %s is down", c.Platform)
	}
	r.seq++
	c.RemoteID = fmt.Sprintf("%s-%d", c.Platform, r.seq)
	r.calls = append(r.calls, c)
	return c.RemoteID, nil
}

// handler serves the test-control API:
//
//	GET  /__e2e/calls             → every recorded call, oldest first
//	POST /__e2e/reset             → forget calls, bring every platform up
//	POST /__e2e/down?platform=X   → make X fail until /__e2e/up or reset
//	POST /__e2e/up?platform=X
func (r *recorder) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /__e2e/calls", func(w http.ResponseWriter, _ *http.Request) {
		r.mu.Lock()
		out := append([]call{}, r.calls...)
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("POST /__e2e/reset", func(w http.ResponseWriter, _ *http.Request) {
		r.mu.Lock()
		r.calls, r.down = nil, map[string]bool{}
		r.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /__e2e/{state}", func(w http.ResponseWriter, req *http.Request) {
		p := req.URL.Query().Get("platform")
		state := req.PathValue("state")
		if p == "" || (state != "down" && state != "up") {
			http.Error(w, "want POST /__e2e/down|up?platform=X", http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		r.down[p] = state == "down"
		r.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

// fakeAccount is a destination account that records what it was asked to
// publish instead of calling the network. It speaks the real driver's
// manifest, so the core splits, fits and gates media exactly as in
// production. It implements only destination.Account; fakeReposter and
// fakeRelayAccount add the optional interfaces the real account has.
type fakeAccount struct {
	rec    *recorder
	drv    destination.Driver
	id     func(seq string) string // remote id for a recorded call id
	url    func(seq string) string
	data   func(seq string) json.RawMessage // the driver's Ref.Data; nil = none
	images func(media []destination.MediaItem) []imageRef
}

// fakeReposter is a fakeAccount that can repost (Bluesky, Mastodon).
type fakeReposter struct{ *fakeAccount }

// fakeRelayAccount reposts and redelivers to relays (Nostr).
type fakeRelayAccount struct{ fakeReposter }

var (
	_ destination.Account     = (*fakeAccount)(nil)
	_ destination.Reposter    = fakeReposter{}
	_ destination.Redeliverer = fakeRelayAccount{}
)

func (f *fakeAccount) Manifest() destination.Manifest { return f.drv.Manifest() }

func (f *fakeAccount) Capabilities(context.Context) destination.Capabilities {
	return f.drv.Manifest().Defaults
}

func (f *fakeAccount) Publish(_ context.Context, req destination.PublishRequest) (destination.PublishResult, error) {
	c := call{Platform: f.drv.Manifest().Type, Action: "post", Text: req.Text, Images: f.images(req.Media)}
	switch {
	case req.Quote != nil:
		c.Action, c.ReplyTo = "quote", sourceID(*req.Quote)
	case req.ReplyTo != nil:
		c.Action, c.ReplyTo = "reply", sourceID(*req.ReplyTo)
	case req.InReplyTo != nil:
		c.Action, c.ReplyTo = "reply", req.InReplyTo.Parent.ID
	}
	return f.result(c)
}

func (f fakeReposter) Repost(_ context.Context, src destination.SourceRef) (destination.PublishResult, error) {
	return f.result(call{Platform: f.drv.Manifest().Type, Action: "repost", ReplyTo: sourceID(src)})
}

func (f fakeRelayAccount) Redeliver(_ context.Context, _ destination.ItemRef, endpoint string) destination.Delivery {
	return destination.Delivery{Endpoint: endpoint, Status: "ok"}
}

func (f *fakeAccount) result(c call) (destination.PublishResult, error) {
	seq, err := f.rec.record(c)
	if err != nil {
		return destination.PublishResult{Status: destination.StatusFailed},
			&destination.Error{Class: destination.ClassTransient, Err: err}
	}
	out := destination.PublishResult{Status: destination.StatusPublished,
		Ref: destination.ItemRef{ID: f.id(seq), URL: f.url(seq)}}
	if f.data != nil {
		out.Ref.Data = f.data(seq)
	}
	if c.Platform == nostr.Type {
		out.Deliveries = []destination.Delivery{{Endpoint: "wss://relay.e2e.invalid", Status: "ok"}}
	}
	return out, nil
}

// sourceID is the id a fake records for an external post.
func sourceID(s destination.SourceRef) string {
	for _, v := range []string{s.URI, s.LocalID, s.EventID} {
		if v != "" {
			return v
		}
	}
	return ""
}

// attachments records media as a byte-upload network receives it.
func attachments(media []destination.MediaItem) []imageRef {
	var out []imageRef
	for _, m := range media {
		out = append(out, imageRef{Mime: m.Mime, Alt: m.Alt, Size: len(m.Bytes)})
	}
	return out
}

// imetaImages records media as Nostr receives it: the driver's own NIP-92
// imeta tags.
func imetaImages(media []destination.MediaItem) []imageRef {
	var out []imageRef
	for _, t := range nostr.Imetas(media) {
		out = append(out, imageRef{Mime: imetaField(t, "m"), Alt: imetaField(t, "alt")})
	}
	return out
}

func imetaField(t gonostr.Tag, key string) string {
	for _, kv := range t[1:] {
		if v, ok := strings.CutPrefix(kv, key+" "); ok {
			return v
		}
	}
	return ""
}

// fakeAccounts returns one recording account per built-in destination.
func fakeAccounts(rec *recorder) map[string]destination.Account {
	same := func(prefix string) func(string) string { return func(s string) string { return prefix + s } }
	// Ref.Data as the real drivers fill it: the record CID that Bluesky
	// replies need, and the signed event Nostr relay retries re-send.
	cid := func(s string) json.RawMessage { return bluesky.RefData("bafye2e" + s) }
	event := func(s string) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"id": s, "kind": 1, "content": "", "sig": "e2e"})
		return b
	}
	return map[string]destination.Account{
		nostr.Type: fakeRelayAccount{fakeReposter{&fakeAccount{rec: rec, drv: nostr.Driver{}, id: same(""),
			url: same("https://njump.e2e.invalid/"), data: event, images: imetaImages}}},
		mastodon.Type: fakeReposter{&fakeAccount{rec: rec, drv: mastodon.Driver{}, id: same(""),
			url: same("https://mastodon.e2e.invalid/@me/"), images: attachments}},
		bluesky.Type: fakeReposter{&fakeAccount{rec: rec, drv: bluesky.Driver{}, id: same("at://did:plc:e2e/app.bsky.feed.post/"),
			url: same("https://bsky.e2e.invalid/post/"), data: cid, images: attachments}},
		threads.Type: &fakeAccount{rec: rec, drv: threads.Driver{}, id: same(""),
			url: same("https://threads.e2e.invalid/t/"), images: attachments},
	}
}

// ─── Blossom ─────────────────────────────────────────────────────────────

// fakeBlossom implements the two Blossom calls the media pipeline makes:
// PUT /upload (returns a descriptor with the blob URL) and GET /<sha256>.
type fakeBlossom struct {
	base  string
	mu    sync.Mutex
	blobs map[string]blob
}

type blob struct {
	mime string
	data []byte
}

func newFakeBlossom(base string) *fakeBlossom {
	return &fakeBlossom{base: base, blobs: map[string]blob{}}
}

func (b *fakeBlossom) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPut && r.URL.Path == "/upload":
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Nostr ") {
			http.Error(w, "missing Nostr auth", http.StatusUnauthorized)
			return
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, 256<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		sum := sha256.Sum256(data)
		sha := hex.EncodeToString(sum[:])
		b.mu.Lock()
		b.blobs[sha] = blob{mime: r.Header.Get("Content-Type"), data: data}
		b.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"url": b.base + "/" + sha, "sha256": sha, "size": len(data), "type": r.Header.Get("Content-Type"),
		})
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		sha := strings.TrimPrefix(r.URL.Path, "/")
		b.mu.Lock()
		bl, ok := b.blobs[sha]
		b.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", bl.mime)
		_, _ = w.Write(bl.data)
	default:
		http.Error(w, "unsupported", http.StatusMethodNotAllowed)
	}
}
