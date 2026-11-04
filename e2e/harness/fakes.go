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
	"time"

	gonostr "fiatjaf.com/nostr"

	"github.com/geofox/publisher/internal/dispatch"
	"github.com/geofox/publisher/internal/store"
)

// call is one request a fake platform received — what the real adapter would
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

func images(imgs []dispatch.Img) []imageRef {
	var out []imageRef
	for _, im := range imgs {
		out = append(out, imageRef{Mime: im.Mime, Alt: im.Alt, Size: len(im.Bytes)})
	}
	return out
}

func action(replyTo *dispatch.ReplyRef) (string, string) {
	if replyTo == nil {
		return "post", ""
	}
	return "reply", replyTo.ParentID
}

func result(platform, id, url string, err error) (dispatch.TargetResult, error) {
	if err != nil {
		return dispatch.TargetResult{Platform: platform, Status: "failed", Error: err.Error()}, err
	}
	return dispatch.TargetResult{Platform: platform, Status: "success", RemoteID: id, RemoteURL: url}, nil
}

// ─── Nostr ───────────────────────────────────────────────────────────────

type fakeNostr struct{ rec *recorder }

func (f *fakeNostr) PublishText(_ context.Context, text string, _ *int, imetas []gonostr.Tag, replyTo *dispatch.ReplyRef) (dispatch.TargetResult, error) {
	act, parent := action(replyTo)
	var imgs []imageRef
	for _, t := range imetas {
		if len(t) > 0 {
			imgs = append(imgs, imageRef{Mime: imetaField(t, "m"), Alt: imetaField(t, "alt")})
		}
	}
	id, err := f.rec.record(call{Platform: "nostr", Action: act, Text: text, Images: imgs, ReplyTo: parent})
	r, err := result("nostr", id, "https://njump.e2e.invalid/"+id, err)
	if err == nil {
		r.Relays = []store.RelayState{{URL: "wss://relay.e2e.invalid", Status: "ok", AttemptedAt: time.Now().UTC()}}
	}
	return r, err
}

func imetaField(t gonostr.Tag, key string) string {
	for _, kv := range t[1:] {
		if v, ok := strings.CutPrefix(kv, key+" "); ok {
			return v
		}
	}
	return ""
}

func (f *fakeNostr) RebroadcastToRelay(context.Context, string, string) (bool, string) { return true, "" }

func (f *fakeNostr) Repost(_ context.Context, eventID, _ string, _ int, _ string) (dispatch.TargetResult, error) {
	id, err := f.rec.record(call{Platform: "nostr", Action: "repost", ReplyTo: eventID})
	return result("nostr", id, "https://njump.e2e.invalid/"+id, err)
}

func (f *fakeNostr) Quote(_ context.Context, text, eventID, _, _ string, _ []gonostr.Tag) (dispatch.TargetResult, error) {
	id, err := f.rec.record(call{Platform: "nostr", Action: "quote", Text: text, ReplyTo: eventID})
	return result("nostr", id, "https://njump.e2e.invalid/"+id, err)
}

// ─── Mastodon ────────────────────────────────────────────────────────────

type fakeMastodon struct{ rec *recorder }

func (f *fakeMastodon) PostText(_ context.Context, text string, _ dispatch.Overrides, imgs []dispatch.Img, replyTo *dispatch.ReplyRef) (dispatch.TargetResult, error) {
	act, parent := action(replyTo)
	id, err := f.rec.record(call{Platform: "mastodon", Action: act, Text: text, Images: images(imgs), ReplyTo: parent})
	return result("mastodon", id, "https://mastodon.e2e.invalid/@me/"+id, err)
}

func (f *fakeMastodon) Reblog(_ context.Context, statusID string) (dispatch.TargetResult, error) {
	id, err := f.rec.record(call{Platform: "mastodon", Action: "repost", ReplyTo: statusID})
	return result("mastodon", id, "https://mastodon.e2e.invalid/@me/"+id, err)
}

func (f *fakeMastodon) QuoteStatus(_ context.Context, text, quotedID string, imgs []dispatch.Img) (dispatch.TargetResult, error) {
	id, err := f.rec.record(call{Platform: "mastodon", Action: "quote", Text: text, Images: images(imgs), ReplyTo: quotedID})
	return result("mastodon", id, "https://mastodon.e2e.invalid/@me/"+id, err)
}

// ─── Bluesky ─────────────────────────────────────────────────────────────

type fakeBluesky struct{ rec *recorder }

func (f *fakeBluesky) PostBsky(_ context.Context, text string, _ dispatch.Overrides, imgs []dispatch.Img, replyTo *dispatch.ReplyRef) (dispatch.TargetResult, error) {
	act, parent := action(replyTo)
	id, err := f.rec.record(call{Platform: "bluesky", Action: act, Text: text, Images: images(imgs), ReplyTo: parent})
	r, err := result("bluesky", "at://did:plc:e2e/app.bsky.feed.post/"+id, "https://bsky.e2e.invalid/post/"+id, err)
	if err == nil {
		r.CID = "cid-" + id
	}
	return r, err
}

func (f *fakeBluesky) RepostBsky(_ context.Context, subjectURI, _ string) (dispatch.TargetResult, error) {
	id, err := f.rec.record(call{Platform: "bluesky", Action: "repost", ReplyTo: subjectURI})
	return result("bluesky", "at://did:plc:e2e/app.bsky.feed.repost/"+id, "", err)
}

func (f *fakeBluesky) QuoteBsky(_ context.Context, text string, _ dispatch.Overrides, imgs []dispatch.Img, quoteURI, _ string) (dispatch.TargetResult, error) {
	id, err := f.rec.record(call{Platform: "bluesky", Action: "quote", Text: text, Images: images(imgs), ReplyTo: quoteURI})
	return result("bluesky", "at://did:plc:e2e/app.bsky.feed.post/"+id, "https://bsky.e2e.invalid/post/"+id, err)
}

// ─── Threads ─────────────────────────────────────────────────────────────

type fakeThreads struct{ rec *recorder }

func (f *fakeThreads) PostThreads(_ context.Context, text string, _ dispatch.Overrides, imgs []dispatch.Img, replyTo *dispatch.ReplyRef) (dispatch.TargetResult, error) {
	act, parent := action(replyTo)
	id, err := f.rec.record(call{Platform: "threads", Action: act, Text: text, Images: images(imgs), ReplyTo: parent})
	return result("threads", id, "https://threads.e2e.invalid/t/"+id, err)
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
