# Quote/Reply/Repost — Plan B2: Orchestration + API + UI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn the Plan-A resolve layer and the Plan-B1 client primitives into working reply/repost/quote actions: a `Dispatcher.Interact` entrypoint, a `POST /api/interact` endpoint, a store action descriptor + history rendering, and the Interact-tab action UI (with quote fan-out and the restriction override).

**Architecture:** `Interact` reuses the existing post/target/history model — an interaction is a `store.Post` carrying an action descriptor; quote fan-out maps to multiple targets exactly like a multi-platform post. Reply reuses `runPlatform` with a `ReplyRef` (extended with the Nostr external author); repost/quote use new adapter methods that call the B1 client primitives. The Interact tab gains action buttons → compose box (reply/quote) + fan-out toggles + override.

**Tech Stack:** Go 1.26; `internal/dispatch`, `internal/store`, `internal/api`; vanilla-JS SPA.

**Spec:** `docs/superpowers/specs/2026-05-25-quote-reply-repost-design.md` (§Data flow, §Capability model + override, §UI, §Storage, §Universal fallback rule).

**Builds on:** Plan A (`internal/resolve`, `/api/resolve`, Interact tab preview) and Plan B1 primitives: `nostr.PublishInput.Tags`, `nostr.NostrReply.AuthorPubkey`, `bluesky.Client.Repost`, `bluesky.Post.Quote`(`QuoteRef`), `bluesky.SourcePost.ReplyRootURI/CID`, `mastodon.Client.Reblog`, `mastodon.Client.QuotePost`. The existing `Dispatcher.Post`/`runPlatform`/`runChain`, `store.Post/Target/Attempt`, and `handleAPIPost` are the patterns to mirror.

---

## File Structure

| File | Responsibility (B2) |
|---|---|
| `internal/dispatch/dispatch.go` | `ReplyRef.AuthorPubkey`; new poster methods (repost/quote) on the interfaces; `InteractSpec`/`InteractRef`; `Interact`; `runAction` |
| `internal/dispatch/adapters.go` | adapters map the new methods → B1 client primitives; NostrAdapter passes `AuthorPubkey` |
| `internal/dispatch/interact_test.go` | fakes gain the new methods; reply/repost/quote + fan-out + force tests |
| `internal/store/models.go` | `Post.Interaction *Interaction` + `interaction_json` persist/load |
| `internal/store/store.go` | `interaction_json` column migration |
| `internal/store/models_test.go` | interaction round-trip test |
| `internal/api/api.go` | `Dispatcher` interface gains `Interact`; `handleInteract`; `POST /api/interact` |
| `internal/api/interact_post_test.go` | endpoint test with a fake dispatcher |
| `internal/web/assets/interact.js` | action buttons → compose + fan-out + override + submit |
| `internal/web/assets/history.js` | render the interaction descriptor |
| `internal/web/assets/app.css` | action/fan-out styles |
| `README.md` | document `/api/interact` |

---

## Task 1: dispatch.ReplyRef external author (Nostr)

**Files:** Modify `internal/dispatch/dispatch.go`, `internal/dispatch/adapters.go`; Test `internal/dispatch/interact_test.go` (create).

Context: `ReplyRef{RootID,RootCID,ParentID,ParentCID}`; `NostrAdapter.PublishText` builds `pubnostr.NostrReply{RootID, ParentID}` from `replyTo`. B1 added `NostrReply.AuthorPubkey`.

- [ ] **Step 1: Write the failing test** (`internal/dispatch/interact_test.go`)

```go
package dispatch

import (
	"context"
	"testing"

	gonostr "fiatjaf.com/nostr"
	pubnostr "github.com/geofox/publisher/internal/nostr"
)

// fakeNostrActor records PublishText input and (later) repost/quote calls.
type fakeNostrActor struct {
	lastReply *ReplyRef
}

func (f *fakeNostrActor) PublishText(_ context.Context, _ string, _ *int, _ []gonostr.Tag, replyTo *ReplyRef) (TargetResult, error) {
	f.lastReply = replyTo
	return TargetResult{Platform: "nostr", Status: "success", RemoteID: "ev1"}, nil
}
func (f *fakeNostrActor) RebroadcastToRelay(context.Context, string, string) (bool, string) { return true, "" }

func TestReplyRefCarriesAuthorToNostr(t *testing.T) {
	// The adapter must pass ReplyRef.AuthorPubkey into NostrReply so an external
	// reply p-tags the replied-to author. We assert via a captured NostrReply by
	// using the real adapter over a fake nostr publisher is heavy; instead assert
	// the dispatch ReplyRef has the field and runPlatform forwards it.
	f := &fakeNostrActor{}
	d := &Dispatcher{Nostr: f}
	ref := &ReplyRef{RootID: "r", ParentID: "r", AuthorPubkey: "extpub"}
	d.runPlatform(context.Background(), "nostr", "hi", Overrides{}, nil, nil, ref)
	if f.lastReply == nil || f.lastReply.AuthorPubkey != "extpub" {
		t.Fatalf("AuthorPubkey not forwarded to nostr poster: %+v", f.lastReply)
	}
}

var _ = pubnostr.NostrReply{} // keep import while B2 builds out
```

- [ ] **Step 2: Run test to verify it fails** — `go test ./internal/dispatch/ -run TestReplyRefCarriesAuthor -v` → FAIL (`AuthorPubkey` undefined on ReplyRef).

- [ ] **Step 3: Implement** — add the field to `ReplyRef` in `dispatch.go`:
```go
type ReplyRef struct {
	RootID, RootCID, ParentID, ParentCID string
	AuthorPubkey                         string // nostr: replied-to author hex (external replies)
}
```
In `adapters.go`, `NostrAdapter.PublishText`, set the author on the built reply:
```go
		in.ReplyTo = &pubnostr.NostrReply{RootID: replyTo.RootID, ParentID: replyTo.ParentID, AuthorPubkey: replyTo.AuthorPubkey}
```

- [ ] **Step 4: Run tests** — `go test ./internal/dispatch/ -v` then `go build ./...`. Expected PASS (existing dispatch tests unaffected — the new field defaults to "" so self-threading is unchanged).

- [ ] **Step 5: Commit**
```bash
git add internal/dispatch/dispatch.go internal/dispatch/adapters.go internal/dispatch/interact_test.go
git commit -m "dispatch: ReplyRef carries nostr external-author pubkey"
```

---

## Task 2: Repost/quote adapter methods

**Files:** Modify `internal/dispatch/dispatch.go` (interfaces + `runAction`), `internal/dispatch/adapters.go`; Test `internal/dispatch/interact_test.go`.

Add repost/quote to the poster interfaces and a `runAction` dispatcher that returns a normalized `TargetResult` (mirroring `runPlatform`'s normalization). Adapters call the B1 client primitives. Nostr repost emits kind-6 (kind-1 source) / kind-16 (other) with empty content (NIP-18 allows empty); quote emits kind-1 with a `q` tag + `nostr:` mention.

- [ ] **Step 1: Write the failing test** (append to `interact_test.go`)

```go
type fakeBskyActor struct {
	reposted   [2]string // uri,cid
	quoted     string    // text
	quoteRef   [2]string // uri,cid
}

func (f *fakeBskyActor) PostBsky(_ context.Context, text string, _ Overrides, _ []Img, _ *ReplyRef) (TargetResult, error) {
	return TargetResult{Platform: "bluesky", Status: "success", RemoteID: "p1"}, nil
}
func (f *fakeBskyActor) RepostBsky(_ context.Context, uri, cid string) (TargetResult, error) {
	f.reposted = [2]string{uri, cid}
	return TargetResult{Platform: "bluesky", Status: "success", RemoteID: "rp1"}, nil
}
func (f *fakeBskyActor) QuoteBsky(_ context.Context, text string, _ Overrides, _ []Img, uri, cid string) (TargetResult, error) {
	f.quoted, f.quoteRef = text, [2]string{uri, cid}
	return TargetResult{Platform: "bluesky", Status: "success", RemoteID: "q1"}, nil
}

func TestRunActionRepostBsky(t *testing.T) {
	f := &fakeBskyActor{}
	d := &Dispatcher{Bluesky: f}
	r := d.runAction(context.Background(), actionRepost, "bluesky", "", Overrides{}, nil,
		InteractRef{URI: "at://x", CID: "cidx"})
	if r.Status != "success" || f.reposted != [2]string{"at://x", "cidx"} {
		t.Fatalf("repost not wired: %+v / %v", r, f.reposted)
	}
}

func TestRunActionQuoteBsky(t *testing.T) {
	f := &fakeBskyActor{}
	d := &Dispatcher{Bluesky: f}
	r := d.runAction(context.Background(), actionQuote, "bluesky", "my take", Overrides{}, nil,
		InteractRef{URI: "at://x", CID: "cidx"})
	if r.Status != "success" || f.quoted != "my take" || f.quoteRef != [2]string{"at://x", "cidx"} {
		t.Fatalf("quote not wired: %+v / %q / %v", r, f.quoted, f.quoteRef)
	}
}
```

- [ ] **Step 2: Run test to verify it fails** — `go test ./internal/dispatch/ -run TestRunAction -v` → FAIL (`runAction`/`actionRepost`/`InteractRef`/interface methods undefined).

- [ ] **Step 3: Implement interfaces + types + runAction** (`dispatch.go`)

Add action constants + the interaction ref + extend the interfaces:
```go
const (
	actionReply  = "reply"
	actionRepost = "repost"
	actionQuote  = "quote"
)

// InteractRef is the platform-native identity of the source post being acted on
// (mirrors resolve.PlatformRef, decoupled so dispatch doesn't import resolve).
type InteractRef struct {
	URI, CID         string   // bluesky
	ReplyRootURI     string   // bluesky: thread root for replies (empty → source is root)
	ReplyRootCID     string
	LocalID          string   // mastodon
	EventID, Author  string   // nostr (hex)
	RelayHints       []string // nostr
	Kind             int      // nostr
}

type NostrActor interface {
	Repost(ctx context.Context, eventID, author string, kind int, relayHint string) (TargetResult, error)
	Quote(ctx context.Context, text, eventID, author, relayHint string) (TargetResult, error)
}
type BlueskyActor interface {
	RepostBsky(ctx context.Context, subjectURI, subjectCID string) (TargetResult, error)
	QuoteBsky(ctx context.Context, text string, o Overrides, imgs []Img, quoteURI, quoteCID string) (TargetResult, error)
}
type MastodonActor interface {
	Reblog(ctx context.Context, statusID string) (TargetResult, error)
	QuoteStatus(ctx context.Context, text, quotedID string) (TargetResult, error)
}
type ThreadsActor interface{} // threads has no native repost/quote of external posts
```
Embed the actor interfaces into the existing poster interfaces so the same adapter values satisfy both:
```go
type NostrPoster interface {
	PublishText(ctx context.Context, text string, pow *int, imetas []gonostr.Tag, replyTo *ReplyRef) (TargetResult, error)
	RebroadcastToRelay(ctx context.Context, signedEventJSON, relayURL string) (ok bool, message string)
	NostrActor
}
type MastodonPoster interface {
	PostText(ctx context.Context, text string, o Overrides, imgs []Img, replyTo *ReplyRef) (TargetResult, error)
	MastodonActor
}
type BlueskyPoster interface {
	PostBsky(ctx context.Context, text string, o Overrides, imgs []Img, replyTo *ReplyRef) (TargetResult, error)
	BlueskyActor
}
```
NOTE: the existing dispatch test fakes (in `chain_test.go`, `dispatch_test.go`, `retry_test.go`) implement the OLD interfaces. Embedding new methods will break their compilation. Add the new methods to those fakes as no-op stubs returning `TargetResult{Status:"success"}` (or wire them where a test needs them). Search for types implementing `PostBsky`/`PostText`/`PublishText` and add the actor methods. (The `interact_test.go` fakes above already include them.)

Add `runAction` (normalizes like `runPlatform`):
```go
// runAction performs a repost or quote on one platform and returns a normalized
// TargetResult. text is the commentary (quote only). ref identifies the source.
func (d *Dispatcher) runAction(ctx context.Context, action, plat, text string, ov Overrides, imgs []Img, ref InteractRef) TargetResult {
	start := time.Now()
	var r TargetResult
	var err error
	switch {
	case action == actionRepost && plat == "bluesky":
		if d.Bluesky != nil { r, err = d.Bluesky.RepostBsky(ctx, ref.URI, ref.CID) } else { err = errors.New("bluesky not configured") }
	case action == actionQuote && plat == "bluesky":
		if d.Bluesky != nil { r, err = d.Bluesky.QuoteBsky(ctx, text, ov, imgs, ref.URI, ref.CID) } else { err = errors.New("bluesky not configured") }
	case action == actionRepost && plat == "mastodon":
		if d.Mastodon != nil { r, err = d.Mastodon.Reblog(ctx, ref.LocalID) } else { err = errors.New("mastodon not configured") }
	case action == actionQuote && plat == "mastodon":
		if d.Mastodon != nil { r, err = d.Mastodon.QuoteStatus(ctx, text, ref.LocalID) } else { err = errors.New("mastodon not configured") }
	case action == actionRepost && plat == "nostr":
		if d.Nostr != nil { r, err = d.Nostr.Repost(ctx, ref.EventID, ref.Author, ref.Kind, relayHint(ref.RelayHints)) } else { err = errors.New("nostr not configured") }
	case action == actionQuote && plat == "nostr":
		if d.Nostr != nil { r, err = d.Nostr.Quote(ctx, text, ref.EventID, ref.Author, relayHint(ref.RelayHints)) } else { err = errors.New("nostr not configured") }
	default:
		err = fmt.Errorf("unsupported action %q for %q", action, plat)
	}
	r.Platform = plat
	r.LatencyMS = int(time.Since(start).Milliseconds())
	if err != nil {
		r.Status, r.Error = "failed", err.Error()
	} else if r.Status == "" {
		r.Status = "failed"
		if r.Error == "" { r.Error = "adapter returned empty status" }
	}
	return r
}

func relayHint(hints []string) string {
	if len(hints) > 0 { return hints[0] }
	return ""
}
```
(Confirm `errors`, `fmt`, `time` are imported in dispatch.go — they are.)

- [ ] **Step 4: Implement the adapters** (`adapters.go`) — map to B1 primitives. Add methods to each adapter:
```go
func (a BlueskyAdapter) RepostBsky(ctx context.Context, uri, cid string) (TargetResult, error) {
	res, err := a.C.Repost(ctx, uri, cid)
	if err != nil { return TargetResult{Platform: "bluesky"}, err }
	return TargetResult{Platform: "bluesky", Status: "success", RemoteID: res.RemoteID, RemoteURL: res.RemoteURL, CID: res.CID}, nil
}
func (a BlueskyAdapter) QuoteBsky(ctx context.Context, text string, o Overrides, _ []Img, uri, cid string) (TargetResult, error) {
	// v1: native quote carries the commentary + embed only, no attached images
	// (Mastodon native quote can't carry media either — kept symmetric). Quote+media
	// is a future enhancement (recordWithMedia is already supported by Post).
	bp := bluesky.Post{Text: text, Langs: o.Langs, Quote: &bluesky.QuoteRef{URI: uri, CID: cid},
		ReplyGate: bluesky.ParseReplyGate(o.BlueskyReply), DisableQuotes: o.BlueskyDisableQuotes}
	res, err := a.C.Post(ctx, bp)
	if err != nil { return TargetResult{Platform: "bluesky"}, err }
	return TargetResult{Platform: "bluesky", Status: "success", RemoteID: res.RemoteID, RemoteURL: res.RemoteURL, CID: res.CID}, nil
}
func (a MastodonAdapter) Reblog(ctx context.Context, id string) (TargetResult, error) {
	res, err := a.C.Reblog(ctx, id)
	if err != nil { return TargetResult{Platform: "mastodon"}, err }
	return TargetResult{Platform: "mastodon", Status: "success", RemoteID: res.RemoteID, RemoteURL: res.RemoteURL}, nil
}
func (a MastodonAdapter) QuoteStatus(ctx context.Context, text, quotedID string) (TargetResult, error) {
	res, err := a.C.QuotePost(ctx, text, quotedID)
	if err != nil { return TargetResult{Platform: "mastodon"}, err }
	return TargetResult{Platform: "mastodon", Status: "success", RemoteID: res.RemoteID, RemoteURL: res.RemoteURL}, nil
}
func (a NostrAdapter) Repost(ctx context.Context, eventID, author string, kind int, relayHint string) (TargetResult, error) {
	k := 6
	tags := []gonostr.Tag{{"e", eventID, relayHint}, {"p", author}}
	if kind != 1 {
		k = 16
		tags = append(tags, gonostr.Tag{"k", strconv.Itoa(kind)})
	}
	res, err := a.P.Publish(ctx, pubnostr.PublishInput{Kind: k, Text: "", Tags: tags})
	return nostrResult(res, err)
}
func (a NostrAdapter) Quote(ctx context.Context, text, eventID, author, relayHint string) (TargetResult, error) {
	nevent, _ := nip19.EncodeNevent(/* id */, []string{relayHint}, /* author */) // best-effort; see Task note
	content := text
	if nevent != "" { content = strings.TrimSpace(text) + "\nnostr:" + nevent }
	tags := []gonostr.Tag{{"q", eventID, relayHint, author}}
	res, err := a.P.Publish(ctx, pubnostr.PublishInput{Kind: 1, Text: content, Tags: tags})
	return nostrResult(res, err)
}
```
Add the existing nostr-result mapping helper `nostrResult(res pubnostr.PublishResult, err error) (TargetResult, error)` if one isn't already factored out of `NostrAdapter.PublishText` — extract it so Repost/Quote reuse the relays/signed-event/status mapping. For `nip19.EncodeNevent`, convert `eventID`/`author` hex via `gonostr.IDFromHex`/`gonostr.PubKeyFromHex` (mirror Plan A's `njumpURL`); on error, skip the `nostr:` mention (the `q` tag still records the quote). Confirm `a.P` (the nostr publisher interface in adapters.go) exposes `Publish`; if the adapter only holds a narrow interface, widen it to include `Publish(ctx, PublishInput) (PublishResult, error)`.

- [ ] **Step 5: Update the existing fakes** so the suite compiles — add the new actor methods to EVERY fake implementing the bluesky/mastodon/nostr poster interfaces across `chain_test.go`, `dispatch_test.go`, `retry_test.go`, AND `interact_test.go` (the Task-1 `fakeNostrActor` needs `Repost`/`Quote` once `NostrPoster` embeds `NostrActor`). No-op stubs return `TargetResult{Platform: <p>, Status: "success"}` (or record args where a test needs it). Run `go vet ./internal/dispatch/` (it reports every type that no longer satisfies the interface) until clean.

- [ ] **Step 6: Run tests** — `go test ./internal/dispatch/ -v` then `go test ./... && go build ./...`. Expected PASS.

- [ ] **Step 7: Commit**
```bash
git add internal/dispatch/dispatch.go internal/dispatch/adapters.go internal/dispatch/interact_test.go internal/dispatch/chain_test.go internal/dispatch/dispatch_test.go internal/dispatch/retry_test.go
git commit -m "dispatch: repost/quote adapter methods + runAction"
```

---

## Task 3: store action descriptor

**Files:** Modify `internal/store/models.go`, `internal/store/store.go`; Test `internal/store/models_test.go`.

- [ ] **Step 1: Write the failing test** (append to `models_test.go`)

```go
func TestPostInteractionRoundTrip(t *testing.T) {
	db, _ := Open(filepath.Join(t.TempDir(), "i.db"))
	defer db.Close()
	rec := &Post{
		ID: "i1", CreatedAt: time.Now().UTC().Truncate(time.Second),
		Platforms: []string{"bluesky"}, Source: "web", Status: "success",
		Interaction: &Interaction{Action: "quote", SourcePlatform: "bluesky",
			SourceURL: "https://bsky.app/x", SourceAuthor: "@alice"},
		Targets: []Target{{Platform: "bluesky", Status: "success"}},
	}
	if err := db.SavePost(rec); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetPost("i1")
	if got.Interaction == nil || got.Interaction.Action != "quote" ||
		got.Interaction.SourceAuthor != "@alice" || got.Interaction.SourceURL != "https://bsky.app/x" {
		t.Fatalf("interaction not round-tripped: %+v", got.Interaction)
	}
}
```

- [ ] **Step 2: Run test to verify it fails** — `go test ./internal/store/ -run TestPostInteraction -v` → FAIL (`Interaction` undefined / not persisted).

- [ ] **Step 3: Implement** — add the type + field (`models.go`):
```go
// Interaction records that a Post is a reply/repost/quote of an external source.
type Interaction struct {
	Action         string `json:"action"`          // reply|repost|quote
	SourcePlatform string `json:"source_platform"`
	SourceURL      string `json:"source_url"`
	SourceAuthor   string `json:"source_author"`
}
```
Add to `Post`:
```go
	Interaction *Interaction `json:"interaction,omitempty"`
```
Add the column migration in `store.go`'s `migrate()` (mirror the `segments_json` addColumnIfMissing call):
```go
	addColumnIfMissing("posts", "interaction_json", "TEXT")
```
In `SavePost`, marshal `p.Interaction` to the `interaction_json` column (mirror how a nullable JSON column is written; write `""`/NULL when nil). In `GetPost`, scan `interaction_json` and unmarshal into `p.Interaction` when non-empty. (Read the existing `SavePost`/`GetPost` to match the INSERT column list and the row scan exactly — add `interaction_json` to both.)

- [ ] **Step 4: Run tests** — `go test ./internal/store/ -v` then `go build ./...`. Expected PASS (existing posts: `interaction_json` empty → `Interaction` nil).

- [ ] **Step 5: Commit**
```bash
git add internal/store/models.go internal/store/store.go internal/store/models_test.go
git commit -m "store: post interaction descriptor (reply/repost/quote source)"
```

---

## Task 4: Dispatcher.Interact orchestration

**Files:** Modify `internal/dispatch/dispatch.go`; Test `internal/dispatch/interact_test.go`.

`Interact` builds a `store.Post` (with the interaction descriptor) and its targets, then SavePosts. Reply → 1 target via `runPlatform` + a built `ReplyRef`. Repost → 1 target via `runAction`. Quote → native target on the source (via `runAction`) + a link-quote target per fan-out platform (via `runPlatform`, text = commentary + source URL). The `force` flag is informational here (the API/UI gate; dispatch attempts whatever it's given).

- [ ] **Step 1: Write the failing test** (append to `interact_test.go`)

```go
func TestInteractQuoteFansOut(t *testing.T) {
	bsky := &fakeBskyActor{}
	masto := &fakeMastoActor{}
	d := &Dispatcher{Bluesky: bsky, Mastodon: masto}
	post := d.Interact(context.Background(), InteractSpec{
		Action: actionQuote, SourcePlatform: "bluesky",
		Ref:       InteractRef{URI: "at://x", CID: "cidx"},
		SourceURL: "https://bsky.app/x", SourceAuthor: "@alice",
		Text:      "great point",
		Fanout:    []string{"mastodon"},
	})
	if post.Interaction == nil || post.Interaction.Action != "quote" {
		t.Fatalf("missing interaction descriptor: %+v", post.Interaction)
	}
	if len(post.Targets) != 2 {
		t.Fatalf("quote+fanout should make 2 targets, got %d", len(post.Targets))
	}
	// native bluesky quote used QuoteBsky; mastodon got a link-quote (normal post via PostText).
	if bsky.quoted != "great point" {
		t.Errorf("bluesky native quote text wrong: %q", bsky.quoted)
	}
	if !masto.lastPostText.contains("https://bsky.app/x") {
		t.Errorf("mastodon link-quote should include the source URL: %q", masto.lastPostText.s)
	}
}

func TestInteractReplySingleTarget(t *testing.T) {
	bsky := &fakeBskyActor{}
	d := &Dispatcher{Bluesky: bsky}
	post := d.Interact(context.Background(), InteractSpec{
		Action: actionReply, SourcePlatform: "bluesky",
		Ref:  InteractRef{URI: "at://x", CID: "cidx", ReplyRootURI: "at://root", ReplyRootCID: "cidroot"},
		Text: "agreed",
	})
	if len(post.Targets) != 1 || post.Targets[0].Platform != "bluesky" {
		t.Fatalf("reply should make 1 source-platform target: %+v", post.Targets)
	}
	if post.Interaction.Action != "reply" {
		t.Errorf("interaction action wrong")
	}
}
```
Add a tiny helper for the mastodon fake to capture posted text (extend `fakeMastoActor` with a `lastPostText` holder exposing `.s` and `.contains`):
```go
type strHolder struct{ s string }
func (h strHolder) contains(sub string) bool { return strings.Contains(h.s, sub) }

type fakeMastoActor struct{ lastPostText strHolder }
func (f *fakeMastoActor) PostText(_ context.Context, text string, _ Overrides, _ []Img, _ *ReplyRef) (TargetResult, error) {
	f.lastPostText = strHolder{s: text}
	return TargetResult{Platform: "mastodon", Status: "success", RemoteID: "m1"}, nil
}
func (f *fakeMastoActor) Reblog(context.Context, string) (TargetResult, error) { return TargetResult{Platform: "mastodon", Status: "success"}, nil }
func (f *fakeMastoActor) QuoteStatus(_ context.Context, text, _ string) (TargetResult, error) { f.lastPostText = strHolder{s: text}; return TargetResult{Platform: "mastodon", Status: "success", RemoteID: "mq1"}, nil }
```
(Import `strings` in the test.)

- [ ] **Step 2: Run test to verify it fails** — `go test ./internal/dispatch/ -run TestInteract -v` → FAIL (`InteractSpec`/`Interact` undefined).

- [ ] **Step 3: Implement** (`dispatch.go`)

```go
type InteractSpec struct {
	Action         string // reply|repost|quote
	SourcePlatform string
	Ref            InteractRef
	SourceURL      string
	SourceAuthor   string
	Text           string
	Overrides      map[string]Overrides // keyed by platform (commentary platform overrides)
	Fanout         []string             // quote only: other platforms for link-quotes
	Force          bool
	Images         []Img
	MediaRecords   []store.Media
}

// Interact performs reply/repost/quote and records it as a store.Post with an
// interaction descriptor. Reply/repost are source-platform only; quote fans out
// to Fanout platforms as link-quotes (commentary + source URL).
func (d *Dispatcher) Interact(ctx context.Context, spec InteractSpec) *store.Post {
	rec := &store.Post{
		ID: newID(), CreatedAt: time.Now().UTC(), MasterText: spec.Text,
		Source: "web", Media: spec.MediaRecords,
		Interaction: &store.Interaction{
			Action: spec.Action, SourcePlatform: spec.SourcePlatform,
			SourceURL: spec.SourceURL, SourceAuthor: spec.SourceAuthor,
		},
	}
	imetas := buildImetas(spec.MediaRecords)
	ov := spec.Overrides[spec.SourcePlatform]

	var results []TargetResult
	switch spec.Action {
	case actionReply:
		results = append(results, d.runPlatform(ctx, spec.SourcePlatform, spec.Text, ov, spec.Images, imetas, buildReplyRef(spec)))
	case actionRepost:
		results = append(results, d.runAction(ctx, actionRepost, spec.SourcePlatform, "", ov, nil, spec.Ref))
	case actionQuote:
		// native quote on the source platform
		results = append(results, d.runAction(ctx, actionQuote, spec.SourcePlatform, spec.Text, ov, spec.Images, spec.Ref))
		// link-quote fan-out on the other selected platforms (a normal post)
		for _, p := range spec.Fanout {
			if p == spec.SourcePlatform {
				continue
			}
			lov := spec.Overrides[p]
			results = append(results, d.runPlatform(ctx, p, linkQuoteText(spec.Text, spec.SourceURL), lov, spec.Images, imetas, nil))
		}
	}

	rec.Platforms = nil
	succ, failed := 0, 0
	for _, r := range results {
		rec.Platforms = append(rec.Platforms, r.Platform)
		rec.Targets = append(rec.Targets, store.Target{
			Platform: r.Platform, FinalText: spec.Text, Status: r.Status,
			RemoteID: r.RemoteID, RemoteURL: r.RemoteURL, LatencyMS: r.LatencyMS,
			Relays: r.Relays, SignedEventJSON: r.SignedEventJSON,
			Attempts: []store.Attempt{{AttemptNo: 1, Status: r.Status, Error: r.Error, LatencyMS: r.LatencyMS,
				RemoteID: r.RemoteID, RequestJSON: r.RequestJSON, ResponseJSON: r.ResponseJSON, AttemptedAt: time.Now().UTC()}},
		})
		switch r.Status {
		case "success":
			succ++
		case "failed":
			failed++
		}
	}
	switch total := len(results); {
	case total == 0 || failed == total:
		rec.Status = "failed"
	case succ == total:
		rec.Status = "success"
	default:
		rec.Status = "partial"
	}
	if d.Store != nil {
		if err := d.Store.SavePost(rec); err != nil {
			slog.Error("savepost (interact) failed", "post_id", rec.ID, "err", err)
		}
	}
	return rec
}

// buildReplyRef derives the platform reply ref from the source. For Bluesky the
// parent is the source post and the root is its thread root (or the source
// itself if it is top-level). For Mastodon the parent id is the local id. For
// Nostr the parent is the event id with the external author for the p-tag.
func buildReplyRef(spec InteractSpec) *ReplyRef {
	ref := spec.Ref
	switch spec.SourcePlatform {
	case "bluesky":
		root, rootCID := ref.ReplyRootURI, ref.ReplyRootCID
		if root == "" {
			root, rootCID = ref.URI, ref.CID // source is the thread root
		}
		return &ReplyRef{RootID: root, RootCID: rootCID, ParentID: ref.URI, ParentCID: ref.CID}
	case "mastodon":
		return &ReplyRef{ParentID: ref.LocalID}
	case "nostr":
		return &ReplyRef{RootID: ref.EventID, ParentID: ref.EventID, AuthorPubkey: ref.Author}
	}
	return nil
}

// linkQuoteText appends the source URL to the commentary for a fan-out link-quote.
func linkQuoteText(text, url string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return url
	}
	return text + "\n\n" + url
}
```
(Confirm `strings`, `slog`, `newID`, `buildImetas` are available in dispatch.go — they are, used by `Post`.)

- [ ] **Step 4: Run tests** — `go test ./internal/dispatch/ -v` then `go test ./... && go build ./...`. Expected PASS.

- [ ] **Step 5: Commit**
```bash
git add internal/dispatch/dispatch.go internal/dispatch/interact_test.go
git commit -m "dispatch: Interact orchestration (reply/repost/quote + fan-out)"
```

---

## Task 5: API — POST /api/interact

**Files:** Modify `internal/api/api.go`, `cmd/publisher/main.go` (nothing new to wire — `a.Dispatch` already set; just the interface gains a method); Test `internal/api/interact_post_test.go`.

- [ ] **Step 1: Write the failing test** (`internal/api/interact_post_test.go`)

```go
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/dispatch"
	"github.com/geofox/publisher/internal/store"
)

type fakeInteractDispatcher struct{ got dispatch.InteractSpec }

func (f *fakeInteractDispatcher) Post(context.Context, dispatch.PostSpec) *store.Post { return nil }
func (f *fakeInteractDispatcher) Retry(context.Context, string, []string) (*store.Post, error) { return nil, nil }
func (f *fakeInteractDispatcher) RetryRelay(context.Context, string, string) (*store.Post, error) { return nil, nil }
func (f *fakeInteractDispatcher) Schedule(context.Context, dispatch.PostSpec, time.Time) (*store.Post, error) { return nil, nil }
func (f *fakeInteractDispatcher) Interact(_ context.Context, spec dispatch.InteractSpec) *store.Post {
	f.got = spec
	return &store.Post{ID: "x1", Status: "success", Interaction: &store.Interaction{Action: spec.Action},
		Targets: []store.Target{{Platform: spec.SourcePlatform, Status: "success", RemoteURL: "u"}}}
}

func TestAPIInteractForwardsSpec(t *testing.T) {
	fd := &fakeInteractDispatcher{}
	a := &API{Dispatch: fd}
	body, _ := json.Marshal(map[string]any{
		"action": "quote", "platform": "bluesky",
		"ref":    map[string]any{"uri": "at://x", "cid": "cidx"},
		"source_url": "https://bsky.app/x", "source_author": "@a",
		"text": "hi", "fanout": []string{"mastodon"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/interact", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if fd.got.Action != "quote" || fd.got.SourcePlatform != "bluesky" || fd.got.Ref.URI != "at://x" {
		t.Fatalf("spec not forwarded: %+v", fd.got)
	}
	if len(fd.got.Fanout) != 1 || fd.got.Fanout[0] != "mastodon" {
		t.Errorf("fanout not forwarded: %+v", fd.got.Fanout)
	}
}

func TestAPIInteractRejectsBadAction(t *testing.T) {
	a := &API{Dispatch: &fakeInteractDispatcher{}}
	body, _ := json.Marshal(map[string]any{"action": "bogus", "platform": "bluesky"})
	req := httptest.NewRequest(http.MethodPost, "/api/interact", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails** — `go test ./internal/api/ -run TestAPIInteract -v` → FAIL (Interact not on the interface / route missing).

- [ ] **Step 3: Implement** (`api.go`)

Add `Interact` to the `Dispatcher` interface:
```go
	Interact(ctx context.Context, spec dispatch.InteractSpec) *store.Post
```
Register the route in `Routes()` (after `/api/resolve`):
```go
	mux.HandleFunc("POST /api/interact", a.handleInteract)
```
Add the handler:
```go
func (a *API) handleInteract(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxPostRequestBytes)
	var req struct {
		Action       string                          `json:"action"`
		Platform     string                          `json:"platform"`
		Ref          dispatch.InteractRef            `json:"ref"`
		SourceURL    string                          `json:"source_url"`
		SourceAuthor string                          `json:"source_author"`
		Text         string                          `json:"text"`
		Overrides    map[string]dispatch.Overrides   `json:"overrides"`
		Fanout       []string                        `json:"fanout"`
		Force        bool                            `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	switch req.Action {
	case "reply", "repost", "quote":
	default:
		httpx.WriteError(w, http.StatusBadRequest, "action must be reply, repost, or quote")
		return
	}
	if req.Platform == "" {
		httpx.WriteError(w, http.StatusBadRequest, "platform is required")
		return
	}
	post := a.Dispatch.Interact(r.Context(), dispatch.InteractSpec{
		Action: req.Action, SourcePlatform: req.Platform, Ref: req.Ref,
		SourceURL: req.SourceURL, SourceAuthor: req.SourceAuthor, Text: req.Text,
		Overrides: req.Overrides, Fanout: req.Fanout, Force: req.Force,
	})
	httpx.WriteJSON(w, http.StatusOK, post)
}
```
NOTE: `dispatch.InteractRef`/`dispatch.Overrides` JSON tags — `Overrides` already has json tags; add json tags to `InteractRef` fields in dispatch.go so the request decodes (`uri`,`cid`,`reply_root_uri`,`reply_root_cid`,`local_id`,`event_id`,`author`,`relay_hints`,`kind`). The Plan A `/api/resolve` returns `resolve.PlatformRef` with tags `uri`,`cid`,`local_id`,`event_id`,`author`,`relay_hints`,`kind` — make `InteractRef`'s tags MATCH those so the JS can pass the resolved `ref` straight back (plus `reply_root_uri`/`reply_root_cid`, which the JS includes from a Plan-B2 resolve addition — see Task 6 note; for non-bluesky or top-level they're absent/empty).

- [ ] **Step 4: Run tests** — `go test ./... && go vet ./... && go build ./cmd/publisher`. Expected PASS. (Existing api fakes implementing `Dispatcher` — e.g. in `thread_post_test.go` — now need the `Interact` method; add it returning nil to those fakes.)

- [ ] **Step 5: Commit**
```bash
git add internal/api/api.go internal/api/interact_post_test.go internal/api/thread_post_test.go
git commit -m "api: POST /api/interact"
```

---

## Task 6: Interact tab — actions

**Files:** Modify `internal/web/assets/interact.js`, `internal/web/assets/app.css`. Also Modify `internal/resolve/resolve.go`/`bluesky` adapter so the resolved `ref` includes the bluesky reply-root (so replies thread correctly).

- [ ] **Step 1: Carry the bluesky reply-root into the resolved ref**

In `internal/resolve/resolve.go`, add to `PlatformRef`:
```go
	ReplyRootURI string `json:"reply_root_uri,omitempty"`
	ReplyRootCID string `json:"reply_root_cid,omitempty"`
```
In `internal/resolve/adapters.go` `BlueskyAdapter.ResolveSource`, set them from the B1 `SourcePost`:
```go
		Ref: PlatformRef{URI: p.URI, CID: p.CID, ReplyRootURI: p.ReplyRootURI, ReplyRootCID: p.ReplyRootCID},
```
Add/extend a resolve adapter test asserting the reply-root flows into `Ref` (mirror `TestBlueskyAdapterMapsViewerFlags`, setting `ReplyRootURI` on the fake `SourcePost` and asserting `ref.Ref.ReplyRootURI`).

- [ ] **Step 2: Implement the action UI** (`interact.js`)

Read the current `interact.js` (Plan A). Replace the read-only `capRow` with action controls. When a source is resolved, render, per action, a button that is enabled if `cap.allowed` and otherwise shown disabled with the reason + a "try anyway" affordance (sets `force:true`). Reply & Quote open a small composer (`<textarea>`); Quote also shows fan-out checkboxes for the OTHER platforms (`bluesky/mastodon/nostr/threads` minus the source). Repost posts immediately (confirm). On submit, `POST /api/interact` with `{action, platform, ref, source_url, source_author, text, fanout, force}` (pass the resolved `s.ref`, `s.preview.web_url`, `s.preview.author_handle`). Show the resulting target statuses (success/partial/failed) and links, reusing the existing toast/`flash` + the `el`/`api` helpers. Keep all post-derived text via `textContent`/`el({text})`.

Use this structure (adapt to the real `el`/`api`/`flash` signatures):
```js
function actionPanel(s) {
  const wrap = el("div", { class: "act-panel" });
  const text = el("textarea", { class: "act-text", placeholder: "add your comment…" });
  const status = el("div", { class: "act-status muted" });

  const fanout = el("div", { class: "act-fanout" });
  const fanBoxes = {};
  for (const p of ["bluesky", "mastodon", "nostr", "threads"]) {
    if (p === s.platform) continue;
    const cb = el("input", { type: "checkbox" });
    fanBoxes[p] = cb;
    fanout.append(el("label", { class: "act-fan" }, cb, el("span", { text: " " + p })));
  }

  async function send(action, force) {
    status.textContent = "Working…";
    const fan = action === "quote" ? Object.keys(fanBoxes).filter(p => fanBoxes[p].checked) : [];
    try {
      const post = await api("/api/interact", {
        method: "POST", headers: { "content-type": "application/json" },
        body: JSON.stringify({
          action, platform: s.platform, ref: s.ref,
          source_url: s.preview.web_url, source_author: s.preview.author_handle,
          text: text.value, fanout: fan, force: !!force,
        }),
      });
      status.textContent = `${post.status} — ${(post.targets || []).map(t => t.platform + ":" + t.status).join(", ")}`;
      flash(action + " " + post.status);
    } catch (e) {
      status.textContent = "✗ " + e.message;
    }
  }

  for (const [action, cap] of [["reply", s.caps.reply], ["repost", s.caps.repost], ["quote", s.caps.quote]]) {
    const btn = el("button", { class: "act-btn", type: "button", text: action[0].toUpperCase() + action.slice(1) });
    if (!cap.allowed) {
      btn.classList.add("blocked");
      btn.title = cap.reason || "not allowed";
    }
    btn.addEventListener("click", () => {
      const needText = action !== "repost";
      wrap.querySelector(".act-compose").hidden = !needText;
      wrap.querySelector(".act-fanout").hidden = action !== "quote";
      if (!cap.allowed) {
        if (!confirm(`${action}: ${cap.reason || "blocked"} — try anyway?`)) return; // eslint-disable-line no-alert
        send(action, true);
      } else if (action === "repost") {
        send("repost", false);
      } else {
        wrap._send = () => send(action, false);
      }
    });
    wrap.append(btn);
  }
  const compose = el("div", { class: "act-compose", hidden: true }, text, fanout,
    el("button", { class: "act-go", type: "button", text: "Send", onclick: () => wrap._send && wrap._send() }));
  wrap.append(compose, status);
  return wrap;
}
```
Replace the Plan-A `card.append(capRow(s.caps))` line with `card.append(actionPanel(s))` (you may keep a compact availability hint too). Match the real `el` children convention (nodes vs `text:`), and the `confirm()` use is acceptable for the override prompt (or reuse `confirmModal` from common.js if present — prefer it).

- [ ] **Step 3: Add CSS** (`app.css`) — minimal styling for `.act-panel/.act-btn/.act-btn.blocked/.act-compose/.act-text/.act-fanout/.act-status` using existing vars (mirror the `.src-*` styles added in Plan A).

- [ ] **Step 4: Build + sanity** — `go test ./internal/resolve/ ./internal/web/ && go build ./cmd/publisher`; if `node` present, `node --check internal/web/assets/interact.js`. Expected clean.

- [ ] **Step 5: Commit**
```bash
git add internal/resolve/resolve.go internal/resolve/adapters.go internal/resolve/adapters_test.go internal/web/assets/interact.js internal/web/assets/app.css
git commit -m "web: Interact tab actions (reply/repost/quote + fan-out + override)"
```

---

## Task 7: History — render interactions

**Files:** Modify `internal/web/assets/history.js`.

- [ ] **Step 1: Render the interaction descriptor**

Read `history.js`'s `renderDetail`/list rendering. The post JSON now includes `post.interaction = {action, source_platform, source_url, source_author}` for interactions. In the per-post header (list row and/or detail), when `post.interaction` is present, prepend a small badge: `↩ replied to`, `❝ quoted`, or `🔁 reposted` + `source_author`, linking to `source_url` (via `el("a", {href, target:"_blank", rel:"noopener"})` → safeURL). Build with `el({text})`; never innerHTML. Example helper:
```js
function interactionBadge(post) {
  const i = post.interaction;
  if (!i) return null;
  const verb = i.action === "reply" ? "↩ replied to" : i.action === "quote" ? "❝ quoted" : "🔁 reposted";
  const a = i.source_url
    ? el("a", { href: i.source_url, target: "_blank", rel: "noopener", text: i.source_author || i.source_platform })
    : el("span", { text: i.source_author || i.source_platform });
  return el("div", { class: "hist-interaction" }, el("span", { text: verb + " " }), a);
}
```
Call it where each post is rendered (list item and detail), guarded by `post.interaction`. Add a CSS line for `.hist-interaction` if useful.

- [ ] **Step 2: Build + test** — `go test ./internal/web/ && go build ./cmd/publisher`; `node --check internal/web/assets/history.js` if node present. Expected clean.

- [ ] **Step 3: Commit**
```bash
git add internal/web/assets/history.js internal/web/assets/app.css
git commit -m "web: render reply/quote/repost interactions in history"
```

---

## Task 8: Docs + full verification

**Files:** Modify `README.md`.

- [ ] **Step 1: Document `/api/interact`** — near the `/api/resolve` section, add:
```markdown
### `POST /api/interact`

Perform a reply, repost, or quote of a resolved source post. Body:
`{ action, platform, ref, source_url, source_author, text, fanout, force }` where
`ref` is the `ref` object returned by `/api/resolve`. Reply/repost act only on the
source `platform`; `quote` also link-quotes to each `fanout` platform (commentary +
source URL). `force:true` overrides a blocked capability. Returns the resulting
`store.Post` (targets with per-platform status). Used by the **Interact** tab.
```
Also update the Interact-tab paragraph to note it now performs the actions (not just preview).

- [ ] **Step 2: Full verification** — run:
```bash
go test ./...
go vet ./...
go build ./cmd/publisher
```
Expected: ALL pass, vet clean, build OK. STOP and report if any fail.

- [ ] **Step 3: Commit**
```bash
git add README.md
git commit -m "docs: document /api/interact and Interact-tab actions"
```

---

## Self-Review notes (for the implementer)

- **Spec coverage (B2):** §Data flow `/api/interact` + dispatch (Tasks 4-5); §Universal fallback rule — quote uses native on the source via `runAction` and link-quote on fan-out via `runPlatform` (Task 4); the source-platform native→link degrade when blocked is handled by the UI override + the orchestration always attempting what it's given (a blocked native quote the user doesn't override simply isn't sent for that platform — the UI controls this); §Capability override (`force`, Tasks 5-6); §Storage action descriptor (Task 3); §UI actions + fan-out (Task 6); §History (Task 7).
- **Reuse:** `Interact` mirrors `Post`'s target/attempt/status-aggregation + `SavePost`; reply reuses `runPlatform`; the store/history/retry machinery is unchanged (an interaction is just a `store.Post` with `Interaction` set).
- **Backward compatibility:** `ReplyRef.AuthorPubkey` and the new actor interface methods are additive; existing self-thread/post paths are untouched (the embedded actor methods require updating the existing dispatch test fakes — that's compile-only). `interaction_json` is an additive column; existing posts load with `Interaction == nil`.
- **Type names (consistent across tasks):** `dispatch.InteractSpec`, `dispatch.InteractRef` (json tags matching `resolve.PlatformRef` + `reply_root_uri/cid`), `dispatch.Interact`, `dispatch.runAction`, `actionReply/Repost/Quote`, `store.Interaction`, the actor interfaces (`NostrActor`/`BlueskyActor`/`MastodonActor`) and adapter methods (`RepostBsky`/`QuoteBsky`/`Reblog`/`QuoteStatus`/`Repost`/`Quote`).
- **Deferred / v1 simplifications:** Nostr repost emits empty kind-6/16 content (NIP-18 permits empty; avoids threading the raw source event JSON) — note it. Scheduling an interaction is out of scope (interactions post immediately). Threads is never a source; it only appears as a quote fan-out target (a normal post).
- **Test strategy:** dispatch `Interact`/`runAction` fully unit-tested with fakes (reply single-target, repost wiring, quote fan-out expansion, link-quote text); store round-trip; API forward + validation; UI/history verified at Task 8 live test (manual, real creds).
