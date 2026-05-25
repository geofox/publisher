# Quote/Reply/Repost — Plan A: Resolve + Preview (read-only) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Paste a post URL (or a Nostr identifier) and see the source post previewed with per-action availability (reply/quote/repost), with no actions performed yet.

**Architecture:** A new `internal/resolve` package mirrors `internal/dispatch`: it defines `SourceRef`/`Caps` value types and per-platform `…Source` interfaces, with adapters that wrap the existing platform clients. The clients gain *read* methods (Bluesky `getPosts`, Mastodon search/status, Nostr relay fetch). `resolve.Service.Resolve` detects the platform from the input and delegates. A new `POST /api/resolve` returns the `SourceRef` as JSON, and a new **Interact** tab renders the preview + capabilities.

**Tech Stack:** Go 1.26; existing `internal/bluesky` (hand-rolled XRPC), `internal/mastodon` (`github.com/mattn/go-mastodon`), `internal/nostr` (`fiatjaf.com/nostr` + relay pool); vanilla-JS SPA.

**Spec:** `docs/superpowers/specs/2026-05-25-quote-reply-repost-design.md` (this plan implements the read-only half: `internal/resolve`, client read methods, capability model, `/api/resolve`, Interact tab in preview mode. Actions are Plan B.)

**Builds on / mirrors:** the `dispatch.Dispatcher{Nostr,Mastodon,Bluesky,Threads}` adapter pattern (`internal/dispatch/dispatch.go`, `adapters.go`) and the `verify.Service` injected-interface pattern (`internal/api/api.go:69`, `cmd/publisher/main.go:79`).

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/resolve/resolve.go` | `SourceRef`/`Preview`/`Media`/`Caps`/`Cap`/`PlatformRef` types; `Service` + `…Source` interfaces; `Resolve` orchestration; `detect()`; sentinel errors |
| `internal/resolve/resolve_test.go` | detection + orchestration tests with fakes |
| `internal/resolve/adapters.go` | `BlueskyAdapter`/`MastodonAdapter`/`NostrAdapter` mapping client reads → `SourceRef`+`Caps` |
| `internal/resolve/adapters_test.go` | capability-mapping tests with fake clients |
| `internal/nostr/source.go` | `Publisher.ResolveSource` (decode NIP-19/hex, fetch event + author profile) |
| `internal/nostr/source_test.go` | identifier decode + fetch tests |
| `internal/bluesky/source.go` | `Client.GetPost` (parse URL, resolveHandle, getPosts) + `get` query helper |
| `internal/bluesky/source_test.go` | URL parse + getPosts mapping tests (fake HTTP) |
| `internal/mastodon/source.go` | `Client` raw-HTTP fields; `ResolveStatus`, `QuoteSupported` |
| `internal/mastodon/source_test.go` | search/status/version tests (fake HTTP) |
| `internal/api/api.go` | `Resolver` interface, `Resolve` field, `POST /api/resolve` handler |
| `internal/api/resolve_test.go` | endpoint test with fake resolver |
| `cmd/publisher/main.go` | construct clients once; wire `a.Resolve` |
| `internal/web/assets/index.html` | Interact tab nav + section |
| `internal/web/assets/interact.js` | smart input → `/api/resolve` → preview card + caps |
| `internal/web/assets/app.css` | preview-card + caps styles |
| `README.md` | document `/api/resolve` + the Interact tab |

---

## Task 1: `internal/resolve` core — types, detection, orchestration

**Files:**
- Create: `internal/resolve/resolve.go`
- Test: `internal/resolve/resolve_test.go`

- [ ] **Step 1: Write the failing test** (`internal/resolve/resolve_test.go`)

```go
package resolve

import (
	"context"
	"errors"
	"testing"
)

type fakeSource struct {
	gotInput string
	ref      *SourceRef
	err      error
}

func (f *fakeSource) ResolveSource(_ context.Context, input string) (*SourceRef, error) {
	f.gotInput = input
	return f.ref, f.err
}

func TestDetect(t *testing.T) {
	cases := map[string]string{
		"https://bsky.app/profile/alice.bsky.social/post/3kabc": "bluesky",
		"https://mastodon.social/@alice/112233445566":           "mastodon",
		"https://example.town/users/bob/statuses/9":             "mastodon",
		"https://www.threads.net/@alice/post/Cabc":              "threads",
		"https://threads.com/@alice/post/Cabc":                  "threads",
		"nostr:nevent1qqsxyz":                                   "nostr",
		"nevent1qqsxyz":                                         "nostr",
		"note1qqsxyz":                                           "nostr",
		"naddr1qqsxyz":                                          "nostr",
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef": "nostr",
		"not a url or id": "",
	}
	for in, want := range cases {
		if got := detect(in); got != want {
			t.Errorf("detect(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveDelegatesByPlatform(t *testing.T) {
	bsky := &fakeSource{ref: &SourceRef{Platform: "bluesky"}}
	s := &Service{Bluesky: bsky}
	ref, err := s.Resolve(context.Background(), "https://bsky.app/profile/a/post/x")
	if err != nil || ref.Platform != "bluesky" {
		t.Fatalf("ref=%v err=%v", ref, err)
	}
	if bsky.gotInput == "" {
		t.Errorf("input not forwarded to bluesky source")
	}
}

func TestResolveThreadsUnsupported(t *testing.T) {
	s := &Service{}
	_, err := s.Resolve(context.Background(), "https://www.threads.net/@a/post/x")
	if !errors.Is(err, ErrThreadsUnsupported) {
		t.Fatalf("want ErrThreadsUnsupported, got %v", err)
	}
}

func TestResolveUnrecognized(t *testing.T) {
	s := &Service{}
	_, err := s.Resolve(context.Background(), "hello world")
	if !errors.Is(err, ErrUnrecognized) {
		t.Fatalf("want ErrUnrecognized, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/resolve/ -run 'TestDetect|TestResolve' -v`
Expected: FAIL — package/types undefined.

- [ ] **Step 3: Implement the core** (`internal/resolve/resolve.go`)

```go
// Package resolve turns a pasted post URL or Nostr identifier into a SourceRef:
// the platform-native handle, a preview, and what the owner may do with it. It is
// the inbound counterpart to internal/dispatch and wraps the platform clients via
// adapters (see adapters.go).
package resolve

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Sentinel errors surfaced to the UI.
var (
	ErrThreadsUnsupported = errors.New("threads posts can't be resolved: the API has no URL→id lookup")
	ErrUnrecognized       = errors.New("unrecognized post URL or identifier")
	ErrNotConfigured      = errors.New("that platform is not configured")
)

// SourceRef is a resolved external post.
type SourceRef struct {
	Platform string      `json:"platform"`
	Ref      PlatformRef `json:"ref"`
	Preview  Preview     `json:"preview"`
	Caps     Caps        `json:"caps"`
}

// PlatformRef is the platform-native identity needed to act later (Plan B). It is
// a flat, JSON-serializable union; only the fields relevant to Platform are set.
type PlatformRef struct {
	// Bluesky
	URI string `json:"uri,omitempty"`
	CID string `json:"cid,omitempty"`
	// Mastodon
	LocalID string `json:"local_id,omitempty"`
	// Nostr
	EventID    string   `json:"event_id,omitempty"` // hex
	Author     string   `json:"author,omitempty"`   // hex pubkey
	RelayHints []string `json:"relay_hints,omitempty"`
	Kind       int      `json:"kind,omitempty"`
}

type Preview struct {
	AuthorName   string    `json:"author_name"`
	AuthorHandle string    `json:"author_handle"`
	Text         string    `json:"text"`
	Media        []Media   `json:"media,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	WebURL       string    `json:"web_url"`
}

type Media struct {
	URL string `json:"url"`
	Alt string `json:"alt,omitempty"`
}

// Caps reports per-action availability; Reason explains a false Allowed.
type Caps struct {
	Reply  Cap `json:"reply"`
	Quote  Cap `json:"quote"`
	Repost Cap `json:"repost"`
}

type Cap struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
}

// Source resolves one platform's input into a SourceRef.
type Source interface {
	ResolveSource(ctx context.Context, input string) (*SourceRef, error)
}

// Service detects the platform and delegates. Each field is nil when that
// platform isn't configured.
type Service struct {
	Bluesky  Source
	Mastodon Source
	Nostr    Source
}

func (s *Service) Resolve(ctx context.Context, input string) (*SourceRef, error) {
	input = strings.TrimSpace(input)
	switch detect(input) {
	case "bluesky":
		if s.Bluesky == nil {
			return nil, ErrNotConfigured
		}
		return s.Bluesky.ResolveSource(ctx, input)
	case "mastodon":
		if s.Mastodon == nil {
			return nil, ErrNotConfigured
		}
		return s.Mastodon.ResolveSource(ctx, input)
	case "nostr":
		if s.Nostr == nil {
			return nil, ErrNotConfigured
		}
		return s.Nostr.ResolveSource(ctx, input)
	case "threads":
		return nil, ErrThreadsUnsupported
	default:
		return nil, ErrUnrecognized
	}
}

var (
	reNostrID = regexp.MustCompile(`^(?:nostr:|web\+nostr:)?(?:nevent|note|naddr)1[0-9a-z]+$`)
	reHex64   = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
)

// detect classifies an input as "bluesky" | "mastodon" | "nostr" | "threads" |
// "" (unrecognized). Mastodon is the fallthrough for any non-Bluesky/Threads
// http(s) URL, since Mastodon instances use arbitrary domains.
func detect(input string) string {
	if reNostrID.MatchString(input) || reHex64.MatchString(input) {
		return "nostr"
	}
	u, err := url.Parse(input)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	host := strings.ToLower(strings.TrimPrefix(u.Host, "www."))
	switch {
	case host == "bsky.app":
		return "bluesky"
	case host == "threads.net" || host == "threads.com":
		return "threads"
	default:
		return "mastodon"
	}
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/resolve/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/resolve/resolve.go internal/resolve/resolve_test.go
git commit -m "resolve: core types, platform detection, delegation"
```

---

## Task 2: Nostr source read + adapter

**Files:**
- Create: `internal/nostr/source.go`, `internal/nostr/source_test.go`
- Modify: `internal/resolve/adapters.go` (create), `internal/resolve/adapters_test.go` (create)

The Nostr read lives on `pubnostr.Publisher` (it owns the relay pool + relay resolution). The adapter maps it to a `SourceRef`. Nostr is an open protocol → all actions allowed; if the event is NIP-70 protected (`["-"]` tag) note that repost won't embed the JSON.

- [ ] **Step 1: Write the failing test** (`internal/nostr/source_test.go`)

First read `internal/nostr/nostr.go` to confirm the exact relay-pool API (`p.pool`, the `QuerySingle`/`QueryEvents` usage near `resolveRelays`, `internal/nostr/nostr.go:286-337`) and the `nip19` import path (`fiatjaf.com/nostr/nip19`). The test below exercises identifier decoding, which is pure and library-backed; adapt symbol names to the actual `fiatjaf.com/nostr` API as you implement.

```go
package nostr

import "testing"

func TestParseEventInput(t *testing.T) {
	hex := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cases := []struct {
		in      string
		wantHex string
	}{
		{hex, hex},
		{"nostr:" + hex, hex}, // tolerate a nostr: prefix on hex too
	}
	for _, c := range cases {
		ptr, err := parseEventInput(c.in)
		if err != nil {
			t.Fatalf("parseEventInput(%q): %v", c.in, err)
		}
		if ptr.IDHex != c.wantHex {
			t.Errorf("parseEventInput(%q).IDHex = %q, want %q", c.in, ptr.IDHex, c.wantHex)
		}
	}
}

func TestParseEventInputRejectsGarbage(t *testing.T) {
	if _, err := parseEventInput("not-an-id"); err == nil {
		t.Fatal("expected error for garbage input")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/nostr/ -run TestParseEventInput -v`
Expected: FAIL — `parseEventInput` undefined.

- [ ] **Step 3: Implement `ResolveSource` + helpers** (`internal/nostr/source.go`)

```go
package nostr

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	gonostr "fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"
)

// SourceEvent is a fetched external event plus the decoded pointer metadata.
type SourceEvent struct {
	IDHex      string
	Author     string // hex pubkey
	Kind       int
	Content    string
	CreatedAt  time.Time
	RelayHints []string
	Protected  bool   // NIP-70 ["-"] tag present
	AuthorName string // best-effort, from kind-0
}

// eventPointer is the decoded reference (before fetching).
type eventPointer struct {
	IDHex      string
	Author     string
	Kind       int
	RelayHints []string
}

// parseEventInput decodes a NIP-19/NIP-21 reference or a raw 64-char hex id into
// an eventPointer. naddr (addressable) is out of scope for v1 fetch-by-id and
// returns an error directing the user to paste an nevent/note/hex.
func parseEventInput(in string) (eventPointer, error) {
	in = strings.TrimSpace(in)
	in = strings.TrimPrefix(in, "web+nostr:")
	in = strings.TrimPrefix(in, "nostr:")
	if len(in) == 64 {
		if b, err := hex.DecodeString(in); err == nil {
			return eventPointer{IDHex: hex.EncodeToString(b)}, nil
		}
	}
	prefix, value, err := nip19.Decode(in)
	if err != nil {
		return eventPointer{}, fmt.Errorf("decode %q: %w", in, err)
	}
	switch prefix {
	case "note", "nevent":
		ep, ok := value.(gonostr.EventPointer) // {ID, Relays, Author, Kind}
		if !ok {
			return eventPointer{}, fmt.Errorf("unexpected pointer type for %s", prefix)
		}
		return eventPointer{
			IDHex:      ep.ID.Hex(),
			Author:     pubkeyHexOrEmpty(ep.Author),
			Kind:       int(ep.Kind),
			RelayHints: ep.Relays,
		}, nil
	default:
		return eventPointer{}, fmt.Errorf("unsupported reference %q (paste an nevent/note/hex event id)", prefix)
	}
}

// ResolveSource fetches the referenced event (and best-effort the author's
// display name) for preview. It queries the pointer's relay hints unioned with
// the owner's write relays, skipping overlay relays, bounded by ctx.
func (p *Publisher) ResolveSource(ctx context.Context, input string) (*SourceEvent, error) {
	ptr, err := parseEventInput(input)
	if err != nil {
		return nil, err
	}
	urls := p.sourceRelays(ctx, ptr.RelayHints)
	id, err := gonostr.IDFromHex(ptr.IDHex)
	if err != nil {
		return nil, fmt.Errorf("bad event id: %w", err)
	}
	ev := p.fetchOne(ctx, urls, gonostr.Filter{IDs: []gonostr.ID{id}, Limit: 1})
	if ev == nil {
		return nil, fmt.Errorf("event %s not found on %d relays", ptr.IDHex[:8], len(urls))
	}
	src := &SourceEvent{
		IDHex:      ptr.IDHex,
		Author:     ev.PubKey.Hex(),
		Kind:       int(ev.Kind),
		Content:    ev.Content,
		CreatedAt:  ev.CreatedAt.Time(),
		RelayHints: ptr.RelayHints,
		Protected:  hasProtectedTag(ev.Tags),
	}
	// Best-effort display name (kind 0). Never fails the resolve.
	if prof := p.fetchOne(ctx, urls, gonostr.Filter{Authors: []gonostr.PubKey{ev.PubKey}, Kinds: []gonostr.Kind{0}, Limit: 1}); prof != nil {
		var meta struct {
			Name        string `json:"name"`
			DisplayName string `json:"display_name"`
		}
		if json.Unmarshal([]byte(prof.Content), &meta) == nil {
			src.AuthorName = firstNonEmpty(meta.DisplayName, meta.Name)
		}
	}
	return src, nil
}

func hasProtectedTag(tags gonostr.Tags) bool {
	for _, t := range tags {
		if len(t) >= 1 && t[0] == "-" {
			return true
		}
	}
	return false
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func pubkeyHexOrEmpty(pk gonostr.PubKey) string {
	var zero gonostr.PubKey
	if pk == zero {
		return ""
	}
	return pk.Hex()
}
```

Add two small private helpers reusing the existing pool/relay code. Implement them in `source.go` by mirroring the relay selection in `internal/nostr/nostr.go` (`ResolveWriteRelays`, `FallbackRelays`, `IsOverlayRelay`) and the pool query idiom used by `resolveRelays`:

```go
// sourceRelays unions the pointer hints with the owner's write relays, dropping
// overlay (.onion/.i2p) relays and de-duping.
func (p *Publisher) sourceRelays(ctx context.Context, hints []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(u string) {
		if u == "" || seen[u] || IsOverlayRelay(u) {
			return
		}
		seen[u] = true
		out = append(out, u)
	}
	for _, h := range hints {
		add(h)
	}
	if write, err := p.ResolveWriteRelays(ctx); err == nil {
		for _, u := range write {
			add(u)
		}
	}
	for _, u := range p.FallbackRelays {
		add(u)
	}
	return out
}

// fetchOne returns the first event matching filter across urls, or nil. Mirror
// the pool query used in resolveRelays (internal/nostr/nostr.go) — use
// p.pool.QuerySingle(ctx, urls, filter, ...) if available, else QueryEvents on a
// short-lived context.
func (p *Publisher) fetchOne(ctx context.Context, urls []string, filter gonostr.Filter) *gonostr.Event {
	if re := p.pool.QuerySingle(ctx, urls, filter); re != nil {
		ev := re.Event
		return &ev
	}
	return nil
}
```

NOTE for the implementer: `IsOverlayRelay`, `p.pool`, `ResolveWriteRelays`, `FallbackRelays` already exist; confirm the exact `QuerySingle` signature/return against the installed `fiatjaf.com/nostr` (the research found `pool.QuerySingle(ctx, urls, filter, opts) *gonostr.RelayEvent`). Adjust the `fetchOne` body to the real signature; keep its behavior (first match or nil). Confirm `gonostr.IDFromHex`, `PubKey.Hex()`, `ID.Hex()`, `CreatedAt.Time()`, and `nip19.Decode` return shapes; the research verified these against the vendored version.

- [ ] **Step 4: Add the Nostr adapter** (`internal/resolve/adapters.go` — create) and its test

Create `internal/resolve/adapters.go`:

```go
package resolve

import (
	"context"
	"fmt"

	pubnostr "github.com/geofox/publisher/internal/nostr"
	"fiatjaf.com/nostr/nip19"
)

// NostrSourceClient is the read surface resolve needs from the nostr publisher.
type NostrSourceClient interface {
	ResolveSource(ctx context.Context, input string) (*pubnostr.SourceEvent, error)
}

type NostrAdapter struct{ P NostrSourceClient }

func (a NostrAdapter) ResolveSource(ctx context.Context, input string) (*SourceRef, error) {
	ev, err := a.P.ResolveSource(ctx, input)
	if err != nil {
		return nil, err
	}
	repostReason := ""
	if ev.Protected {
		repostReason = "protected event (NIP-70): repost won't embed it — quote instead"
	}
	name := ev.AuthorName
	if name == "" {
		name = "npub:" + ev.Author[:8]
	}
	return &SourceRef{
		Platform: "nostr",
		Ref: PlatformRef{
			EventID: ev.IDHex, Author: ev.Author, Kind: ev.Kind, RelayHints: ev.RelayHints,
		},
		Preview: Preview{
			AuthorName: name, AuthorHandle: "",
			Text: ev.Content, CreatedAt: ev.CreatedAt,
			WebURL: njumpURL(ev.IDHex, ev.Author, ev.RelayHints),
		},
		// Open protocol: everything allowed.
		Caps: Caps{
			Reply:  Cap{Allowed: true},
			Quote:  Cap{Allowed: true},
			Repost: Cap{Allowed: true, Reason: repostReason},
		},
	}, nil
}

// njumpURL builds an njump.me web link so non-Nostr platforms get a clickable URL.
func njumpURL(idHex, authorHex string, relays []string) string {
	nevent, err := nip19.EncodeNevent(mustID(idHex), relays, mustPub(authorHex))
	if err != nil {
		return "https://njump.me/" + idHex
	}
	return "https://njump.me/" + nevent
}
```

(Implement `mustID`/`mustPub` as tiny hex→type helpers, or inline; `EncodeNevent` is from `fiatjaf.com/nostr/nip19` per the research — verify the exact signature and fall back to the hex form on error.)

Create `internal/resolve/adapters_test.go`:

```go
package resolve

import (
	"context"
	"testing"
	"time"

	pubnostr "github.com/geofox/publisher/internal/nostr"
)

type fakeNostr struct{ ev *pubnostr.SourceEvent }

func (f fakeNostr) ResolveSource(context.Context, string) (*pubnostr.SourceEvent, error) {
	return f.ev, nil
}

func TestNostrAdapterAllowsEverything(t *testing.T) {
	ev := &pubnostr.SourceEvent{
		IDHex: "ab12", Author: "f00dbabef00dbabef00dbabef00dbabef00dbabef00dbabef00dbabef00dbabe",
		Kind: 1, Content: "hello", CreatedAt: time.Now(),
	}
	a := NostrAdapter{P: fakeNostr{ev: ev}}
	ref, err := a.ResolveSource(context.Background(), "nevent1x")
	if err != nil {
		t.Fatal(err)
	}
	if !ref.Caps.Reply.Allowed || !ref.Caps.Quote.Allowed || !ref.Caps.Repost.Allowed {
		t.Errorf("nostr caps should all be allowed: %+v", ref.Caps)
	}
	if ref.Preview.WebURL == "" || ref.Ref.EventID != "ab12" {
		t.Errorf("ref/web url wrong: %+v", ref)
	}
}

func TestNostrAdapterProtectedRepostReason(t *testing.T) {
	ev := &pubnostr.SourceEvent{IDHex: "ab12", Author: "f00d", Kind: 1, Protected: true}
	a := NostrAdapter{P: fakeNostr{ev: ev}}
	ref, _ := a.ResolveSource(context.Background(), "x")
	if ref.Caps.Repost.Reason == "" {
		t.Error("protected event should annotate the repost reason")
	}
}
```

(The protected-event test uses a short Author; guard the `ev.Author[:8]` slice in the adapter against short strings — use a helper `shortPub` that slices min(8,len).)

- [ ] **Step 5: Run tests**

Run: `go test ./internal/nostr/ ./internal/resolve/ -v` then `go build ./...`
Expected: PASS, build clean.

- [ ] **Step 6: Commit**

```bash
git add internal/nostr/source.go internal/nostr/source_test.go internal/resolve/adapters.go internal/resolve/adapters_test.go
git commit -m "resolve+nostr: fetch a referenced event and map to SourceRef"
```

---

## Task 3: Bluesky source read + adapter

**Files:**
- Create: `internal/bluesky/source.go`, `internal/bluesky/source_test.go`
- Modify: `internal/resolve/adapters.go`, `internal/resolve/adapters_test.go`

Add a `get` query helper (the existing `do` is POST-only, `internal/bluesky/bluesky.go:256`), `GetPost` (parse URL → at-URI via `resolveHandle`, then `getPosts` for cid/author/text/embed/viewer flags), and the Bluesky adapter mapping viewer flags → caps.

- [ ] **Step 1: Write the failing test** (`internal/bluesky/source_test.go`)

First read `internal/bluesky/bluesky.go` lines 1-150 for the `Client` struct fields (`PDS`, `Identifier`, `AppPassword`, `HTTP`), the `session` type (`AccessJwt`, `Did`, `Handle`), `createSession` (`:219`), and `webURL`/`rkeyOf` helpers.

```go
package bluesky

import "testing"

func TestParsePostURL(t *testing.T) {
	cases := []struct{ url, wantActor, wantRkey string }{
		{"https://bsky.app/profile/alice.bsky.social/post/3kabc", "alice.bsky.social", "3kabc"},
		{"https://bsky.app/profile/did:plc:xyz/post/3kdef", "did:plc:xyz", "3kdef"},
	}
	for _, c := range cases {
		actor, rkey, err := parsePostURL(c.url)
		if err != nil || actor != c.wantActor || rkey != c.wantRkey {
			t.Errorf("parsePostURL(%q) = %q,%q,%v", c.url, actor, rkey, err)
		}
	}
	if _, _, err := parsePostURL("https://bsky.app/profile/alice"); err == nil {
		t.Error("expected error for non-post URL")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/bluesky/ -run TestParsePostURL -v`
Expected: FAIL — `parsePostURL` undefined.

- [ ] **Step 3: Implement `parsePostURL`, `get`, `GetPost`** (`internal/bluesky/source.go`)

```go
package bluesky

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SourcePost is a fetched external post for preview + capability detection.
type SourcePost struct {
	URI, CID         string
	AuthorHandle     string
	AuthorName       string
	Text             string
	Media            []SourceMedia
	CreatedAt        time.Time
	WebURL           string
	ReplyDisabled    bool
	EmbeddingDisabled bool
	ThreadgateReason string // e.g. "replies limited to followers"
	NotFoundOrBlocked bool
}

type SourceMedia struct{ URL, Alt string }

// parsePostURL splits a bsky.app post URL into the actor (handle or DID) and rkey.
func parsePostURL(raw string) (actor, rkey string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", err
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	// expect: profile/<actor>/post/<rkey>
	if len(parts) != 4 || parts[0] != "profile" || parts[2] != "post" {
		return "", "", fmt.Errorf("not a bluesky post URL: %s", raw)
	}
	return parts[1], parts[3], nil
}

// get performs an authed XRPC GET query and decodes JSON into out (the existing
// do() is POST-only). Mirrors do() at bluesky.go:256.
func (c *Client) get(ctx context.Context, path string, params url.Values, accessJwt string, out any) error {
	full := c.PDS + path
	if len(params) > 0 {
		full += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return err
	}
	if accessJwt != "" {
		req.Header.Set("Authorization", "Bearer "+accessJwt)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("%s: %s: %s", path, resp.Status, string(rb))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// GetPost resolves a bsky.app URL to a SourcePost using an authed session (viewer
// flags are only meaningful when authed).
func (c *Client) GetPost(ctx context.Context, postURL string) (*SourcePost, error) {
	actor, rkey, err := parsePostURL(postURL)
	if err != nil {
		return nil, err
	}
	s, err := c.createSession(ctx)
	if err != nil {
		return nil, err
	}
	did := actor
	if !strings.HasPrefix(actor, "did:") {
		var rh struct{ Did string `json:"did"` }
		if err := c.get(ctx, "/xrpc/com.atproto.identity.resolveHandle",
			url.Values{"handle": {actor}}, s.AccessJwt, &rh); err != nil {
			return nil, fmt.Errorf("resolveHandle: %w", err)
		}
		did = rh.Did
	}
	uri := "at://" + did + "/app.bsky.feed.post/" + rkey

	var out struct {
		Posts []struct {
			URI    string `json:"uri"`
			Cid    string `json:"cid"`
			Author struct {
				Handle      string `json:"handle"`
				DisplayName string `json:"displayName"`
			} `json:"author"`
			Record struct {
				Text      string    `json:"text"`
				CreatedAt time.Time `json:"createdAt"`
			} `json:"record"`
			Embed  json.RawMessage `json:"embed"`
			Viewer struct {
				ReplyDisabled     bool `json:"replyDisabled"`
				EmbeddingDisabled bool `json:"embeddingDisabled"`
			} `json:"viewer"`
			Threadgate *struct{} `json:"threadgate"`
		} `json:"posts"`
	}
	if err := c.get(ctx, "/xrpc/app.bsky.feed.getPosts",
		url.Values{"uris": {uri}}, s.AccessJwt, &out); err != nil {
		return nil, fmt.Errorf("getPosts: %w", err)
	}
	if len(out.Posts) == 0 {
		return &SourcePost{URI: uri, NotFoundOrBlocked: true}, nil
	}
	p := out.Posts[0]
	sp := &SourcePost{
		URI: p.URI, CID: p.Cid,
		AuthorHandle: p.Author.Handle, AuthorName: p.Author.DisplayName,
		Text: p.Record.Text, CreatedAt: p.Record.CreatedAt,
		WebURL:            webURL(p.Author.Handle, p.URI),
		ReplyDisabled:     p.Viewer.ReplyDisabled,
		EmbeddingDisabled: p.Viewer.EmbeddingDisabled,
	}
	if p.Threadgate != nil {
		sp.ThreadgateReason = "replies are limited by the author"
	}
	sp.Media = extractImages(p.Embed)
	return sp, nil
}

// extractImages pulls image URLs+alts from the hydrated embed view (best-effort;
// unknown embed shapes yield no media).
func extractImages(embed json.RawMessage) []SourceMedia {
	if len(embed) == 0 {
		return nil
	}
	var e struct {
		Type   string `json:"$type"`
		Images []struct {
			Thumb string `json:"thumb"`
			Fullsize string `json:"fullsize"`
			Alt   string `json:"alt"`
		} `json:"images"`
	}
	if json.Unmarshal(embed, &e) != nil {
		return nil
	}
	var out []SourceMedia
	for _, im := range e.Images {
		url := im.Fullsize
		if url == "" {
			url = im.Thumb
		}
		if url != "" {
			out = append(out, SourceMedia{URL: url, Alt: im.Alt})
		}
	}
	return out
}
```

NOTE: confirm `Client` has an exported `PDS` and `HTTP` field and `webURL(handle, uri)` exists (used by `Post`, `bluesky.go:187`). If `HTTP` is unexported, use the same accessor `Post`/`do` use.

- [ ] **Step 4: Add the Bluesky adapter + test**

Append to `internal/resolve/adapters.go`:

```go
import bsky "github.com/geofox/publisher/internal/bluesky" // add to the import block

type BlueskySourceClient interface {
	GetPost(ctx context.Context, url string) (*bsky.SourcePost, error)
}

type BlueskyAdapter struct{ C BlueskySourceClient }

func (a BlueskyAdapter) ResolveSource(ctx context.Context, input string) (*SourceRef, error) {
	p, err := a.C.GetPost(ctx, input)
	if err != nil {
		return nil, err
	}
	if p.NotFoundOrBlocked {
		return nil, fmt.Errorf("post not found or blocked")
	}
	replyReason := ""
	if p.ReplyDisabled {
		replyReason = "replies are restricted by the author"
		if p.ThreadgateReason != "" {
			replyReason = p.ThreadgateReason
		}
	}
	quoteReason := ""
	if p.EmbeddingDisabled {
		quoteReason = "the author disabled quotes for this post"
	}
	media := make([]Media, 0, len(p.Media))
	for _, m := range p.Media {
		media = append(media, Media{URL: m.URL, Alt: m.Alt})
	}
	name := p.AuthorName
	if name == "" {
		name = p.AuthorHandle
	}
	return &SourceRef{
		Platform: "bluesky",
		Ref:      PlatformRef{URI: p.URI, CID: p.CID},
		Preview: Preview{
			AuthorName: name, AuthorHandle: "@" + p.AuthorHandle,
			Text: p.Text, Media: media, CreatedAt: p.CreatedAt, WebURL: p.WebURL,
		},
		Caps: Caps{
			Reply:  Cap{Allowed: !p.ReplyDisabled, Reason: replyReason},
			Quote:  Cap{Allowed: !p.EmbeddingDisabled, Reason: quoteReason},
			Repost: Cap{Allowed: true},
		},
	}, nil
}
```

Append to `internal/resolve/adapters_test.go`:

```go
import bsky "github.com/geofox/publisher/internal/bluesky" // add to imports

type fakeBskySource struct{ p *bsky.SourcePost }

func (f fakeBskySource) GetPost(context.Context, string) (*bsky.SourcePost, error) {
	return f.p, nil
}

func TestBlueskyAdapterMapsViewerFlags(t *testing.T) {
	a := BlueskyAdapter{C: fakeBskySource{p: &bsky.SourcePost{
		URI: "at://x", CID: "cid1", AuthorHandle: "alice.bsky.social",
		Text: "hi", EmbeddingDisabled: true, ReplyDisabled: false,
	}}}
	ref, err := a.ResolveSource(context.Background(), "https://bsky.app/...")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Caps.Quote.Allowed || ref.Caps.Quote.Reason == "" {
		t.Errorf("embeddingDisabled should block quote with a reason: %+v", ref.Caps.Quote)
	}
	if !ref.Caps.Reply.Allowed || !ref.Caps.Repost.Allowed {
		t.Errorf("reply/repost should be allowed: %+v", ref.Caps)
	}
	if ref.Ref.URI != "at://x" || ref.Ref.CID != "cid1" {
		t.Errorf("ref not carried: %+v", ref.Ref)
	}
}
```

- [ ] **Step 5: Run tests**

Run: `go test ./internal/bluesky/ ./internal/resolve/ -v` then `go build ./...`
Expected: PASS, clean.

- [ ] **Step 6: Commit**

```bash
git add internal/bluesky/source.go internal/bluesky/source_test.go internal/resolve/adapters.go internal/resolve/adapters_test.go
git commit -m "resolve+bluesky: getPosts read with viewer-flag capabilities"
```

---

## Task 4: Mastodon source read + adapter

**Files:**
- Create: `internal/mastodon/source.go`, `internal/mastodon/source_test.go`
- Modify: `internal/mastodon/mastodon.go` (Client fields + `New`), `internal/resolve/adapters.go`, `internal/resolve/adapters_test.go`

The `mastodon.Client` currently only wraps `go-mastodon` and stores `c *gomast.Client` (`internal/mastodon/mastodon.go`). Add raw-HTTP fields so we can read `quote_approval`/`visibility` (newer than the lib) and resolve cross-instance URLs.

- [ ] **Step 1: Write the failing test** (`internal/mastodon/source_test.go`)

```go
package mastodon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolveStatusMapsFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/search":
			w.Write([]byte(`{"statuses":[{"id":"local99"}]}`))
		case "/api/v1/statuses/local99":
			w.Write([]byte(`{
				"id":"local99","content":"<p>hello</p>","visibility":"public","url":"https://x/@a/9",
				"created_at":"2026-05-25T10:00:00Z",
				"account":{"display_name":"Alice","acct":"alice@x"},
				"media_attachments":[{"type":"image","url":"https://x/i.png","description":"alt"}],
				"quote_approval":{"current_user":"automatic"}
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	st, err := c.ResolveStatus(context.Background(), "https://x/@a/9")
	if err != nil {
		t.Fatal(err)
	}
	if st.LocalID != "local99" || st.AuthorName != "Alice" || st.Visibility != "public" {
		t.Fatalf("status mapped wrong: %+v", st)
	}
	if st.QuoteCurrentUser != "automatic" || len(st.Media) != 1 {
		t.Fatalf("quote/media mapped wrong: %+v", st)
	}
	if st.TextPlain != "hello" {
		t.Errorf("content should be de-HTMLed: %q", st.TextPlain)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mastodon/ -run TestResolveStatus -v`
Expected: FAIL — `ResolveStatus`/`New` signature.

- [ ] **Step 3: Extend `Client` for raw HTTP** (`internal/mastodon/mastodon.go`)

Change the struct and `New`:

```go
type Client struct {
	c       *gomast.Client
	baseURL string
	token   string
	http    *http.Client
}

func New(baseURL, token string) *Client {
	return &Client{
		c:       gomast.NewClient(&gomast.Config{Server: baseURL, AccessToken: token}),
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}
```

(Add `"net/http"`, `"strings"`, `"time"` to the imports.)

- [ ] **Step 4: Implement `ResolveStatus` + `QuoteSupported`** (`internal/mastodon/source.go`)

```go
package mastodon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type SourceStatus struct {
	LocalID          string
	AuthorName       string
	AuthorAcct       string
	TextPlain        string
	Media            []SourceMedia
	CreatedAt        time.Time
	WebURL           string
	Visibility       string // public|unlisted|private|direct
	QuoteCurrentUser string // automatic|manual|denied|unknown|"" (no quote info)
}

type SourceMedia struct{ URL, Alt string }

var reTag = regexp.MustCompile(`<[^>]+>`)

// ResolveStatus resolves any instance's post URL to a local status (via search
// resolve) and returns its preview + capability fields.
func (c *Client) ResolveStatus(ctx context.Context, postURL string) (*SourceStatus, error) {
	var search struct {
		Statuses []struct {
			ID string `json:"id"`
		} `json:"statuses"`
	}
	if err := c.getJSON(ctx, "/api/v2/search", url.Values{
		"q": {postURL}, "type": {"statuses"}, "resolve": {"true"}, "limit": {"1"},
	}, &search); err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	if len(search.Statuses) == 0 {
		return nil, fmt.Errorf("post not found or not federated to this instance yet")
	}
	id := search.Statuses[0].ID

	var st struct {
		ID         string `json:"id"`
		Content    string `json:"content"`
		Visibility string `json:"visibility"`
		URL        string `json:"url"`
		CreatedAt  time.Time `json:"created_at"`
		Account    struct {
			DisplayName string `json:"display_name"`
			Acct        string `json:"acct"`
		} `json:"account"`
		MediaAttachments []struct {
			URL         string `json:"url"`
			Description string `json:"description"`
		} `json:"media_attachments"`
		QuoteApproval struct {
			CurrentUser string `json:"current_user"`
		} `json:"quote_approval"`
	}
	if err := c.getJSON(ctx, "/api/v1/statuses/"+id, nil, &st); err != nil {
		return nil, fmt.Errorf("get status: %w", err)
	}
	out := &SourceStatus{
		LocalID: st.ID, AuthorName: st.Account.DisplayName, AuthorAcct: st.Account.Acct,
		TextPlain: deHTML(st.Content), CreatedAt: st.CreatedAt, WebURL: st.URL,
		Visibility: st.Visibility, QuoteCurrentUser: st.QuoteApproval.CurrentUser,
	}
	for _, m := range st.MediaAttachments {
		out.Media = append(out.Media, SourceMedia{URL: m.URL, Alt: m.Description})
	}
	return out, nil
}

func deHTML(s string) string {
	s = strings.ReplaceAll(s, "</p><p>", "\n\n")
	s = reTag.ReplaceAllString(s, "")
	return strings.TrimSpace(html.UnescapeString(s))
}

func (c *Client) getJSON(ctx context.Context, path string, params url.Values, out any) error {
	full := c.baseURL + path
	if len(params) > 0 {
		full += "?" + params.Encode()
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("%s: %s: %s", path, resp.Status, string(rb))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
```

(Add `"html"` to the imports for `html.UnescapeString`.)

- [ ] **Step 5: Add the Mastodon adapter + test**

Append to `internal/resolve/adapters.go`:

```go
import mast "github.com/geofox/publisher/internal/mastodon" // add to imports

type MastodonSourceClient interface {
	ResolveStatus(ctx context.Context, url string) (*mast.SourceStatus, error)
}

type MastodonAdapter struct{ C MastodonSourceClient }

func (a MastodonAdapter) ResolveSource(ctx context.Context, input string) (*SourceRef, error) {
	st, err := a.C.ResolveStatus(ctx, input)
	if err != nil {
		return nil, err
	}
	// Quote: automatic→allowed; manual→allowed w/ note; denied/unknown/""→link-only.
	quote := Cap{}
	switch st.QuoteCurrentUser {
	case "automatic":
		quote = Cap{Allowed: true}
	case "manual":
		quote = Cap{Allowed: true, Reason: "needs the author's approval (lands pending)"}
	default:
		quote = Cap{Allowed: false, Reason: "native quote not available — will link instead"}
	}
	// Repost: only public/unlisted can be reblogged.
	repost := Cap{Allowed: true}
	if st.Visibility == "private" || st.Visibility == "direct" {
		repost = Cap{Allowed: false, Reason: "this post's visibility can't be boosted"}
	}
	media := make([]Media, 0, len(st.Media))
	for _, m := range st.Media {
		media = append(media, Media{URL: m.URL, Alt: m.Alt})
	}
	name := st.AuthorName
	if name == "" {
		name = st.AuthorAcct
	}
	return &SourceRef{
		Platform: "mastodon",
		Ref:      PlatformRef{LocalID: st.LocalID},
		Preview: Preview{
			AuthorName: name, AuthorHandle: "@" + st.AuthorAcct,
			Text: st.TextPlain, Media: media, CreatedAt: st.CreatedAt, WebURL: st.WebURL,
		},
		Caps: Caps{Reply: Cap{Allowed: true}, Quote: quote, Repost: repost},
	}, nil
}
```

Append to `internal/resolve/adapters_test.go`:

```go
import mast "github.com/geofox/publisher/internal/mastodon" // add to imports

type fakeMastSource struct{ st *mast.SourceStatus }

func (f fakeMastSource) ResolveStatus(context.Context, string) (*mast.SourceStatus, error) {
	return f.st, nil
}

func TestMastodonAdapterCaps(t *testing.T) {
	// private + denied quote → no boost, no native quote.
	a := MastodonAdapter{C: fakeMastSource{st: &mast.SourceStatus{
		LocalID: "9", AuthorAcct: "a@x", Visibility: "private", QuoteCurrentUser: "denied",
	}}}
	ref, _ := a.ResolveSource(context.Background(), "https://x/@a/9")
	if ref.Caps.Repost.Allowed || ref.Caps.Quote.Allowed {
		t.Errorf("private+denied should block boost and native quote: %+v", ref.Caps)
	}
	if !ref.Caps.Reply.Allowed {
		t.Error("reply should be allowed")
	}

	// public + automatic → all allowed.
	a2 := MastodonAdapter{C: fakeMastSource{st: &mast.SourceStatus{
		LocalID: "9", Visibility: "public", QuoteCurrentUser: "automatic",
	}}}
	ref2, _ := a2.ResolveSource(context.Background(), "x")
	if !ref2.Caps.Quote.Allowed || !ref2.Caps.Repost.Allowed {
		t.Errorf("public+automatic should allow quote+boost: %+v", ref2.Caps)
	}
}
```

- [ ] **Step 6: Run tests**

Run: `go test ./internal/mastodon/ ./internal/resolve/ -v` then `go test ./... && go build ./...`
Expected: PASS, clean (the `mastodon.New` signature is unchanged, so dispatch wiring still builds).

- [ ] **Step 7: Commit**

```bash
git add internal/mastodon/mastodon.go internal/mastodon/source.go internal/mastodon/source_test.go internal/resolve/adapters.go internal/resolve/adapters_test.go
git commit -m "resolve+mastodon: search-resolve a status with quote/boost capabilities"
```

---

## Task 5: API — `POST /api/resolve`

**Files:**
- Modify: `internal/api/api.go`, `cmd/publisher/main.go`
- Test: `internal/api/resolve_test.go`

- [ ] **Step 1: Write the failing test** (`internal/api/resolve_test.go`)

```go
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/geofox/publisher/internal/resolve"
)

type fakeResolver struct {
	ref *resolve.SourceRef
	err error
}

func (f fakeResolver) Resolve(context.Context, string) (*resolve.SourceRef, error) {
	return f.ref, f.err
}

func TestAPIResolveReturnsSourceRef(t *testing.T) {
	a := &API{Resolve: fakeResolver{ref: &resolve.SourceRef{
		Platform: "bluesky",
		Preview:  resolve.Preview{AuthorName: "Alice", Text: "hi"},
		Caps:     resolve.Caps{Quote: resolve.Cap{Allowed: false, Reason: "disabled"}},
	}}}
	body, _ := json.Marshal(map[string]string{"input": "https://bsky.app/x"})
	req := httptest.NewRequest(http.MethodPost, "/api/resolve", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var out resolve.SourceRef
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Platform != "bluesky" || out.Caps.Quote.Allowed {
		t.Fatalf("unexpected: %s", rec.Body.String())
	}
}

func TestAPIResolveErrorIsClientError(t *testing.T) {
	a := &API{Resolve: fakeResolver{err: resolve.ErrThreadsUnsupported}}
	body, _ := json.Marshal(map[string]string{"input": "https://threads.net/x"})
	req := httptest.NewRequest(http.MethodPost, "/api/resolve", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api/ -run TestAPIResolve -v`
Expected: FAIL — `Resolve` field / route undefined.

- [ ] **Step 3: Add the interface, field, route, handler** (`internal/api/api.go`)

Add the interface near `Verifier` (`:69`):

```go
// Resolver is implemented by *resolve.Service; resolves a pasted URL/identifier.
type Resolver interface {
	Resolve(ctx context.Context, input string) (*resolve.SourceRef, error)
}
```

Add the field to `API` (after `Verify`):

```go
	Resolve Resolver // set by cmd/publisher; resolves pasted post URLs/ids
```

Register the route in `Routes()` (after the thread-preview line, `:111`):

```go
	mux.HandleFunc("POST /api/resolve", a.handleResolve)
```

Add the handler (near `handleThreadPreview`), reusing the body-cap + error helpers used elsewhere (`httpx.WriteError`, `maxThreadPreviewBytes`-style cap; import `resolve` and `errors`):

```go
func (a *API) handleResolve(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxVerifyRequestBytes)
	var req struct {
		Input string `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Input) == "" {
		httpx.WriteError(w, http.StatusBadRequest, "input is required")
		return
	}
	ref, err := a.Resolve.Resolve(r.Context(), req.Input)
	if err != nil {
		// Resolution problems (unsupported platform, not found, bad input) are
		// client-facing 400s with the reason; never a 500.
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ref)
}
```

(Confirm `httpx.WriteError` exists — it's used throughout api.go, e.g. `:386`. Confirm `strings` is imported.)

- [ ] **Step 4: Wire it in** (`cmd/publisher/main.go`)

Hoist the platform-client construction so both dispatch and resolve share instances, then set `a.Resolve`. Replace the inline client construction at `main.go:62-67` and add the resolve wiring after `a.Verify` (`:79-83`):

```go
	tc := threads.New(cfg.ThreadsToken, cfg.ThreadsUserID)
	mc := mastodon.New(cfg.MastodonBaseURL, cfg.MastodonToken)
	bc := bluesky.New(cfg.BlueskyPDSURL, cfg.BlueskyIdentifier, cfg.BlueskyAppPassword)
	d := &dispatch.Dispatcher{
		Nostr:    dispatch.NostrAdapter{P: np},
		Mastodon: dispatch.MastodonAdapter{C: mc},
		Bluesky:  dispatch.BlueskyAdapter{C: bc},
		Threads:  dispatch.ThreadsAdapter{C: tc},
		// ... keep existing remaining fields (Store, etc.) unchanged ...
	}
```

and:

```go
	a.Resolve = &resolve.Service{
		Bluesky:  resolve.BlueskyAdapter{C: bc},
		Mastodon: resolve.MastodonAdapter{C: mc},
		Nostr:    resolve.NostrAdapter{P: np},
	}
```

(Add `"github.com/geofox/publisher/internal/resolve"` to main.go imports. `np` is the existing `*pubnostr.Publisher`.)

- [ ] **Step 5: Run tests**

Run: `go test ./... && go vet ./... && go build ./cmd/publisher`
Expected: PASS, vet clean, build OK.

- [ ] **Step 6: Commit**

```bash
git add internal/api/api.go internal/api/resolve_test.go cmd/publisher/main.go
git commit -m "api: POST /api/resolve; wire resolve.Service"
```

---

## Task 6: Interact tab (read-only preview)

**Files:**
- Modify: `internal/web/assets/index.html`, `internal/web/assets/app.css`
- Create: `internal/web/assets/interact.js`

- [ ] **Step 1: Study the SPA structure**

Read `internal/web/assets/index.html` (the tab nav + `<section>` per view and the `data-view` wiring used by `main.js:29`), `internal/web/assets/main.js` (`switchTab`, `init`), and `common.js` (`el`, `$`, `api`, `flash`). Note how existing tabs (compose/history/tools/verify) register: a nav button with `data-view="<name>"` and a matching `<section id="<name>">`, plus a per-view init called from `main.js`.

- [ ] **Step 2: Add the Interact tab markup** (`index.html`)

Add a nav button alongside the others:

```html
<button data-view="interact">Interact</button>
```

and a section (mirror the structure/classes of the existing sections):

```html
<section id="interact" hidden>
  <label class="fl" for="srcinput">Paste a post URL (or a Nostr nevent/note id)</label>
  <input id="srcinput" type="text" placeholder="https://bsky.app/… · https://mastodon… · nevent1…" autocomplete="off" />
  <div id="srcstatus" class="muted"></div>
  <div id="srccard"></div>
</section>
```

- [ ] **Step 3: Implement the resolver UI** (`internal/web/assets/interact.js`)

```js
"use strict";
import { el, $, api, flash } from "./common.js";

const PLAT_LABEL = { bluesky: "Bluesky", mastodon: "Mastodon", nostr: "Nostr", threads: "Threads" };

let _seq = 0;

// resolveInput fetches the source post for the pasted URL/id and renders it.
async function resolveInput(input) {
  const card = $("#srccard"), status = $("#srcstatus");
  card.innerHTML = "";
  if (!input.trim()) { status.textContent = ""; return; }
  status.textContent = "Resolving…";
  const seq = ++_seq;
  let data;
  try {
    data = await api("/api/resolve", {
      method: "POST", headers: { "content-type": "application/json" },
      body: JSON.stringify({ input }),
    });
  } catch (e) {
    if (seq !== _seq) return;
    status.textContent = "✗ " + e.message;
    return;
  }
  if (seq !== _seq) return; // a newer input superseded this response
  status.textContent = "";
  card.append(renderSource(data));
}

// renderSource builds the preview card + capability row (read-only in Plan A).
function renderSource(s) {
  const card = el("div", { class: "src-card p-" + s.platform });
  const p = s.preview;
  card.append(el("div", { class: "src-head" },
    el("span", { class: "src-plat", text: PLAT_LABEL[s.platform] || s.platform }),
    el("span", { class: "src-author", text: p.author_name || "" }),
    el("span", { class: "src-handle muted", text: p.author_handle || "" }),
  ));
  card.append(el("div", { class: "src-text", text: p.text || "" }));
  if (p.media && p.media.length) {
    const g = el("div", { class: "src-media" });
    for (const m of p.media) g.append(el("img", { src: m.url, alt: m.alt || "" }));
    card.append(g);
  }
  if (p.web_url) card.append(el("a", { class: "src-link", href: p.web_url, target: "_blank", rel: "noopener", text: "open original ↗" }));
  card.append(capRow(s.caps));
  return card;
}

function capRow(caps) {
  const row = el("div", { class: "src-caps" });
  for (const [action, cap] of [["Reply", caps.reply], ["Repost", caps.repost], ["Quote", caps.quote]]) {
    const c = el("span", { class: "src-cap " + (cap.allowed ? "ok" : "no"), title: cap.reason || "" },
      `${cap.allowed ? "✓" : "✗"} ${action}${cap.reason ? " — " + cap.reason : ""}`);
    row.append(c);
  }
  return row;
}

// interactInit wires the smart input (debounced).
export function interactInit() {
  const inp = $("#srcinput");
  if (!inp) return;
  let t = null;
  inp.addEventListener("input", e => {
    clearTimeout(t);
    const v = e.target.value;
    t = setTimeout(() => resolveInput(v), 350);
  });
}
```

Wire `interactInit()` into `main.js` alongside the other per-view inits (import it and call it in `init`, matching how `composeInit`/etc. are called).

- [ ] **Step 4: Add CSS** (`internal/web/assets/app.css`)

```css
/* Interact tab — source preview */
#srcinput { width:100%; }
.src-card { border:1px solid var(--line, #2a2a2a); border-radius:8px; padding:10px; margin-top:10px; }
.src-head { display:flex; gap:8px; align-items:baseline; flex-wrap:wrap; }
.src-plat { font-weight:600; }
.src-text { white-space:pre-wrap; margin:8px 0; color:var(--ink); }
.src-media { display:flex; gap:6px; flex-wrap:wrap; }
.src-media img { max-width:120px; max-height:120px; border-radius:6px; }
.src-link { font-size:13px; }
.src-caps { display:flex; gap:10px; margin-top:10px; flex-wrap:wrap; font-size:13px; }
.src-cap.ok { color:var(--ok, #4caf50); }
.src-cap.no { color:var(--muted); }
```

(Confirm the actual CSS variable names at the top of app.css — reuse `--ink`/`--muted`/`--bad`/`--line` as they exist; fix the `--ok` fallback to a real value if there's no `--ok`.)

- [ ] **Step 5: Build + manual sanity**

Run: `go build ./cmd/publisher && go test ./internal/web/`
Expected: clean + pass. (Live check happens in Task 7.)

- [ ] **Step 6: Commit**

```bash
git add internal/web/assets/index.html internal/web/assets/interact.js internal/web/assets/main.js internal/web/assets/app.css
git commit -m "web: Interact tab — resolve & preview a source post (read-only)"
```

---

## Task 7: Docs + full verification

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Document the endpoint + tab** (`README.md`)

Add near the other `/api/*` docs:

```markdown
### `POST /api/resolve`

Resolve a pasted post URL (Bluesky/Mastodon) or a Nostr identifier
(`nevent`/`note`/`nostr:`/hex) into a preview + capability report (read-only;
no posting). Used by the **Interact** tab.

Body: `{ "input": "<url-or-nostr-id>" }`. Returns `{ platform, ref, preview,
caps }` where `caps.{reply,quote,repost}` each carry `{allowed, reason}`.
Threads URLs return 400 (the API has no URL→id lookup). Resolution failures
(not found, unfederated, bad input) return 400 with a reason.
```

- [ ] **Step 2: Full verification**

Run:
```bash
go test ./...
go vet ./...
go build ./cmd/publisher
```
Expected: ALL pass, vet clean, build OK. If any fail, STOP and report.

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs: document /api/resolve and the Interact tab"
```

---

## Self-Review notes (for the implementer)

- **Spec coverage (Plan A portion):** §Architecture `internal/resolve` + read methods (Tasks 1-4); §Capability model — Bluesky viewer flags (T3), Mastodon quote_approval/visibility (T4), Nostr always-allowed + NIP-70 (T2); §Data flow `POST /api/resolve` (T5); §UI Interact tab in preview mode (T6). **Deferred to Plan B:** the actions themselves (`/api/interact`, dispatch builders, quote fan-out, store action descriptor, override) and the Mastodon instance-version probe for *native quote creation* (Plan A only needs `quote_approval` for the capability display).
- **Type consistency:** `SourceRef`/`Preview`/`Media`/`Caps`/`Cap`/`PlatformRef` (T1) are used unchanged by every adapter (T2-4) and the API/JS (T5-6). Client-native types (`pubnostr.SourceEvent`, `bsky.SourcePost`, `mast.SourceStatus`) live in their packages; resolve adapters depend on small per-platform interfaces (`NostrSourceClient`/`BlueskySourceClient`/`MastodonSourceClient`) so tests use fakes — mirroring `dispatch`.
- **Library-binding caveats (verify while implementing):** the exact `fiatjaf.com/nostr` symbols (`nip19.Decode`/`ToPointer`, `pool.QuerySingle`, `EncodeNevent`, `IDFromHex`) — adapt to the installed version, keeping the documented behavior; the research report in the spec's Sources is the reference.
- **No new deps.** Bluesky uses the existing hand-rolled XRPC client (+a `get` helper); Mastodon adds raw HTTP next to the `go-mastodon` wrapper; Nostr reuses the relay pool.
- **Backward compatibility:** `mastodon.New(baseURL, token)` keeps its signature (internals extended), so dispatch wiring is untouched; hoisting client construction in main.go produces identical dispatch behavior.
