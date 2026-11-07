package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	gonostr "fiatjaf.com/nostr"

	"github.com/geofox/publisher/internal/bluesky"
	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/mastodon"
	"github.com/geofox/publisher/internal/nostr"
	"github.com/geofox/publisher/internal/relaysync"
	"github.com/geofox/publisher/internal/resolve"
	"github.com/geofox/publisher/internal/threads"
)

// call is one request a fake platform received — what the real driver would
// have sent upstream.
type call struct {
	Platform string     `json:"platform"`
	Account  string     `json:"account"` // the account's id ("acc_…")
	Action   string     `json:"action"`  // post | reply | repost | quote
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

// recorder holds every call plus how the tests set each platform or
// account up: down (a transient failure), invalid (a refusal that would
// repeat), or held after n more calls (the next one never returns, as a
// send in flight when the process is killed). Modes are keyed by platform
// ("mastodon") or account id ("acc_…"); an account's own mode wins.
type recorder struct {
	mu       sync.Mutex
	calls    []call
	down     map[string]bool
	invalid  map[string]bool
	hold     map[string]int
	attempts map[string]int // every call a platform and an account received, failed ones too
	seq      int
	restart  func()        // replaces the dispatcher (engine.restart)
	never    chan struct{} // held calls wait on it; it is never closed
}

func newRecorder() *recorder {
	r := &recorder{never: make(chan struct{})}
	r.clear()
	return r
}

// clear forgets calls and modes. Calls already held stay held: their
// dispatcher is gone, and releasing them would record over what the new one
// recovered.
func (r *recorder) clear() {
	r.calls = nil
	r.down, r.invalid, r.hold, r.attempts = map[string]bool{}, map[string]bool{}, map[string]int{}, map[string]int{}
}

// errInvalid is a fake's refusal of the content (destination.ClassInvalid).
var errInvalid = errors.New("e2e: the platform refuses this post")

// record stores a call and returns its fake remote id, or an error when the
// platform is marked down or invalid. A held platform blocks the call for
// good once its count runs out.
func (r *recorder) record(c call) (string, error) {
	r.mu.Lock()
	r.attempts[c.Platform]++
	r.attempts[c.Account]++
	for _, k := range []string{c.Account, c.Platform} {
		if n, ok := r.hold[k]; ok {
			if n == 0 {
				r.mu.Unlock()
				<-r.never
			}
			r.hold[k] = n - 1
			break
		}
	}
	defer r.mu.Unlock()
	if r.down[c.Account] || r.down[c.Platform] {
		return "", fmt.Errorf("e2e: %s is down", c.Platform)
	}
	if r.invalid[c.Account] || r.invalid[c.Platform] {
		return "", errInvalid
	}
	r.seq++
	c.RemoteID = fmt.Sprintf("%s-%d", c.Platform, r.seq)
	r.calls = append(r.calls, c)
	return c.RemoteID, nil
}

// handler serves the test-control API:
//
//	GET  /__e2e/calls                  → every recorded call, oldest first
//	GET  /__e2e/attempts               → calls received per platform and per account, failed ones too
//	POST /__e2e/reset                  → forget calls, bring every platform up
//	POST /__e2e/down?platform=X        → make X fail (transient) until up or reset
//	POST /__e2e/invalid?platform=X     → make X refuse posts (invalid) until up or reset
//	POST /__e2e/hold?platform=X&after=N → X takes N more calls, then the next never returns
//	POST /__e2e/up?platform=X          → undo down, invalid and hold
//	POST /__e2e/restart                → abandon the dispatcher and start a new one
//
// Each control takes account=acc_… instead of platform=X for one account.
func (r *recorder) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /__e2e/calls", func(w http.ResponseWriter, _ *http.Request) {
		r.mu.Lock()
		out := append([]call{}, r.calls...)
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("GET /__e2e/attempts", func(w http.ResponseWriter, _ *http.Request) {
		r.mu.Lock()
		out := maps.Clone(r.attempts)
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("POST /__e2e/reset", func(w http.ResponseWriter, _ *http.Request) {
		r.mu.Lock()
		r.clear()
		r.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /__e2e/restart", func(w http.ResponseWriter, _ *http.Request) {
		r.mu.Lock()
		clear(r.hold) // the new process's sends go through
		r.mu.Unlock()
		r.restart()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /__e2e/{state}", func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		platform, acct, state := q.Get("platform"), q.Get("account"), req.PathValue("state")
		p := platform + acct // the one that is set
		after, err := strconv.Atoi(q.Get("after"))
		switch {
		case (platform == "") == (acct == ""), state != "down" && state != "up" && state != "invalid" && state != "hold",
			state == "hold" && (err != nil || after < 0):
			http.Error(w, "want POST /__e2e/down|up|invalid?platform=X (or account=acc_…) or /__e2e/hold?…&after=N", http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		r.down[p], r.invalid[p] = state == "down", state == "invalid"
		delete(r.hold, p)
		if state == "hold" {
			r.hold[p] = after
		}
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
	rec     *recorder
	drv     destination.Driver
	account string // the account's id
	// maxLength replaces the driver's text limit (an instance with a
	// higher one); 0 keeps it.
	maxLength int
	id        func(seq string) string // remote id for a recorded call id
	url       func(seq string) string
	data      func(seq string) json.RawMessage // the driver's Ref.Data; nil = none
	images    func(media []destination.MediaItem) []imageRef
}

// fakeReposter is a fakeAccount that can repost (Bluesky, Mastodon).
type fakeReposter struct{ *fakeAccount }

// fakeRelayAccount reposts and redelivers to relays (Nostr).
type fakeRelayAccount struct{ fakeReposter }

var (
	_ destination.Account        = (*fakeAccount)(nil)
	_ destination.Reposter       = fakeReposter{}
	_ destination.Redeliverer    = fakeRelayAccount{}
	_ nostr.RelayLister          = fakeRelayAccount{}
	_ destination.SourceResolver = fakeReposter{}
)

// e2eNostrKey is the harness Nostr account's public key.
var e2eNostrKey = gonostr.GetPublicKey(gonostr.Generate())

// HomeRelay, OwnerPubkey and ResolveWriteRelays give the Tools tab a relay
// list to show, as the real Nostr account does: a home relay that is never
// dialled (the harness sets no relay sync) and no NIP-65 relays.
func (f fakeRelayAccount) HomeRelay() string                                    { return "wss://relay.e2e.invalid" }
func (f fakeRelayAccount) OwnerPubkey() gonostr.PubKey                          { return e2eNostrKey }
func (f fakeRelayAccount) ResolveWriteRelays(context.Context) ([]string, error) { return nil, nil }
func (f fakeRelayAccount) RelayClient() *http.Client                            { return nil }

func (f *fakeAccount) Manifest() destination.Manifest { return f.drv.Manifest() }

func (f *fakeAccount) Capabilities(context.Context) destination.Capabilities {
	caps := f.drv.Manifest().Defaults
	if f.maxLength > 0 {
		caps.Text.MaxLength = f.maxLength
	}
	return caps
}

func (f *fakeAccount) Publish(ctx context.Context, req destination.PublishRequest) (destination.PublishResult, error) {
	c := call{Platform: f.drv.Manifest().Type, Account: f.account, Action: "post", Text: req.Text, Images: f.images(req.Media)}
	switch {
	case req.Quote != nil:
		c.Action, c.ReplyTo = "quote", sourceID(*req.Quote)
	case req.ReplyTo != nil:
		c.Action, c.ReplyTo = "reply", sourceID(*req.ReplyTo)
	case req.InReplyTo != nil:
		c.Action, c.ReplyTo = "reply", req.InReplyTo.Parent.ID
	}
	return f.result(ctx, c)
}

func (f fakeReposter) Repost(ctx context.Context, src destination.SourceRef) (destination.PublishResult, error) {
	return f.result(ctx, call{Platform: f.drv.Manifest().Type, Account: f.account, Action: "repost", ReplyTo: sourceID(src)})
}

func (f fakeRelayAccount) Redeliver(_ context.Context, _ destination.ItemRef, endpoint string) destination.Delivery {
	return destination.Delivery{Endpoint: endpoint, Status: "ok"}
}

func (f *fakeAccount) result(ctx context.Context, c call) (destination.PublishResult, error) {
	// As the real drivers do (their manifests say CommitMarks): mark the
	// item right before the request that publishes it.
	if err := destination.BeforeCommit(ctx); err != nil {
		return destination.PublishResult{Status: destination.StatusFailed}, err
	}
	seq, err := f.rec.record(c)
	if err != nil {
		class := destination.ClassTransient
		if errors.Is(err, errInvalid) {
			class = destination.ClassInvalid
		}
		return destination.PublishResult{Status: destination.StatusFailed}, &destination.Error{Class: class, Err: err}
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

// configMaxLength is the harness's account config key for a text limit
// that differs from the driver's (fakeAccount.maxLength).
const configMaxLength = "e2e_max_length"

// newFake returns the recording account for one stored account of typ.
func newFake(rec *recorder, typ, accountID string, config map[string]any) (destination.Account, error) {
	same := func(prefix string) func(string) string { return func(s string) string { return prefix + s } }
	// Ref.Data as the real drivers fill it: the record CID that Bluesky
	// replies need, and the signed event Nostr relay retries re-send.
	cid := func(s string) json.RawMessage { return bluesky.RefData("bafye2e" + s) }
	event := func(s string) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"id": s, "kind": 1, "content": "", "sig": "e2e"})
		return b
	}
	maxLength, _, err := destination.ConfigInt(config, configMaxLength)
	if err != nil {
		return nil, destination.Invalidf("%s: %v", configMaxLength, err)
	}
	base := fakeAccount{rec: rec, account: accountID, maxLength: maxLength}
	with := func(drv destination.Driver, id, url func(string) string, data func(string) json.RawMessage,
		images func([]destination.MediaItem) []imageRef) *fakeAccount {
		f := base
		f.drv, f.id, f.url, f.data, f.images = drv, id, url, data, images
		return &f
	}
	switch typ {
	case nostr.Type:
		return fakeRelayAccount{fakeReposter{with(nostr.Driver{}, same(""), same("https://njump.e2e.invalid/"), event, imetaImages)}}, nil
	case mastodon.Type:
		return fakeReposter{with(mastodon.Driver{}, same(""), same("https://mastodon.e2e.invalid/@me/"), nil, attachments)}, nil
	case bluesky.Type:
		return fakeReposter{with(bluesky.Driver{}, same("at://did:plc:e2e/app.bsky.feed.post/"),
			same("https://bsky.e2e.invalid/post/"), cid, attachments)}, nil
	case threads.Type:
		return with(threads.Driver{}, same(""), same("https://threads.e2e.invalid/t/"), nil, attachments), nil
	}
	return nil, destination.Invalidf("no fake for %s", typ)
}

// ResolveSource looks up an external post, as the real account does
// through its own server, from a URL resolve.Detect recognises (Threads
// has no lookup, as its API has none):
//
//	nostr     a 64-hex event id
//	mastodon  https://<host>/@<user>/<id>
//	bluesky   https://bsky.app/profile/<handle>/post/<rkey>
//
// The ids it returns carry the account's id (a Mastodon status id is only
// valid on its own server), and the preview says which account looked the
// post up. A last path segment "missing" is ErrSourceNotFound.
func (f fakeReposter) ResolveSource(_ context.Context, input string) (destination.ResolvedSource, error) {
	typ := f.drv.Manifest().Type
	if resolve.Detect(input) != typ {
		return destination.ResolvedSource{}, destination.ErrSourceUnsupported
	}
	ref := destination.SourceRef{Type: typ}
	key, handle := input, "someone"
	if typ != nostr.Type {
		u, err := url.Parse(input)
		if err != nil {
			return destination.ResolvedSource{}, destination.ErrSourceUnsupported
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		ok := false
		switch typ {
		case mastodon.Type:
			ok = len(parts) == 2 && strings.HasPrefix(parts[0], "@")
		case bluesky.Type:
			ok = len(parts) == 4 && parts[0] == "profile" && parts[2] == "post"
		}
		if !ok {
			return destination.ResolvedSource{}, destination.ErrSourceUnsupported
		}
		key, handle = parts[len(parts)-1], strings.TrimPrefix(parts[0], "@")
		if typ == bluesky.Type {
			handle = parts[1]
		}
	}
	if key == "missing" {
		return destination.ResolvedSource{}, destination.ErrSourceNotFound
	}
	switch typ {
	case nostr.Type:
		ref.EventID, ref.Author, ref.EventKind = key, e2eNostrKey.Hex(), 1
	case bluesky.Type:
		ref.URI, ref.CID = "at://did:plc:"+handle+"/app.bsky.feed.post/"+key, "bafysrc"+key
	default:
		ref.LocalID = f.account + "/" + key
	}
	caps := f.Capabilities(context.Background()).Interactions
	return destination.ResolvedSource{
		Ref: ref,
		Preview: destination.SourcePreview{AuthorName: "Someone", AuthorHandle: handle,
			Text: "A post looked up by " + f.account, WebURL: input,
			CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
		Actions: destination.SourceActions{Reply: destination.ActionAllowed{Allowed: caps.Reply},
			Quote: destination.ActionAllowed{Allowed: caps.Quote}, Repost: destination.ActionAllowed{Allowed: caps.Repost}},
	}, nil
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

// fakeSyncer stands in for relay sync (production always has one), so
// GET /v1/instance says relay sync is available while a Nostr account is
// connected. It reaches no relay: every relay is in step, nothing copied.
type fakeSyncer struct{}

func (fakeSyncer) Scan(_ context.Context, _ relaysync.Owner, targets []relaysync.Target) []relaysync.RelayDiff {
	out := make([]relaysync.RelayDiff, 0, len(targets))
	for _, t := range targets {
		out = append(out, relaysync.RelayDiff{URL: t.URL, Group: t.Group, Status: "ok"})
	}
	return out
}

func (fakeSyncer) Apply(_ context.Context, _ relaysync.Owner, targets []relaysync.Target, direction string) []relaysync.ApplyResult {
	out := make([]relaysync.ApplyResult, 0, len(targets))
	for _, t := range targets {
		out = append(out, relaysync.ApplyResult{URL: t.URL, Direction: direction, Status: "ok"})
	}
	return out
}
