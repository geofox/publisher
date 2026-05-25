# Quote/Reply/Repost — Plan B1: Action Primitives Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add the per-platform client primitives needed to repost and quote an existing post, and to reply to an *external* author on Nostr — all unit-tested in their client packages, with no orchestration/API/UI yet.

**Architecture:** Each platform client gains the minimal capability the dispatch layer (Plan B2) will call: Nostr gets extra-tags pass-through + external-author reply tags; Bluesky gets a `Repost` record method, a quote embed on `Post`, and a reply-root field on `GetPost`; Mastodon gets `Reblog` + native `QuotePost`. Reply *posting* primitives already exist (B1-threading: `bluesky.Post.Reply`, `mastodon.Post.InReplyToID`/`PostText` replyTo, `threads.Post.ReplyToID`); Threads needs nothing new (its fan-out is a normal post).

**Tech Stack:** Go 1.26; `internal/bluesky` (hand-rolled XRPC), `internal/mastodon` (`github.com/mattn/go-mastodon` + raw HTTP added in Plan A), `internal/nostr` (`fiatjaf.com/nostr`).

**Spec:** `docs/superpowers/specs/2026-05-25-quote-reply-repost-design.md` (§Dispatch reuses…, NIP-18/10 details, Mastodon reblog/native-quote). This plan is the client-primitives half; the dispatch orchestration, `/api/interact`, store action descriptor, quote fan-out, override, and Interact-tab actions are **Plan B2**.

**Builds on:** Plan A (`internal/resolve`, the `GetPost`/`ResolveStatus`/`ResolveSource` read methods) and B1-threading reply primitives (`bluesky.ReplyRef`, `mastodon` replyTo, `nostr.NostrReply`+`replyTags`).

---

## File Structure

| File | Responsibility (B1) |
|---|---|
| `internal/nostr/nostr.go` | `PublishInput.Tags` pass-through; `NostrReply.AuthorPubkey`; `replyTags` external-author p-tag + 5th e-tag author element |
| `internal/nostr/reply_test.go` | external-author reply-tag tests |
| `internal/nostr/nostr_test.go` (or existing) | `PublishInput.Tags` appended-before-signing test |
| `internal/bluesky/bluesky.go` | `Post.Quote *QuoteRef` → embed.record / recordWithMedia; `Client.Repost` |
| `internal/bluesky/source.go` | `SourcePost.ReplyRootURI/CID` from the record's `reply.root` |
| `internal/bluesky/quote_repost_test.go` | quote-embed record shape + repost record + reply-root decode |
| `internal/mastodon/mastodon.go` | (no change beyond Plan A fields) |
| `internal/mastodon/source.go` | `Client.Reblog`, `Client.QuotePost` |
| `internal/mastodon/action_test.go` | reblog + native-quote request tests (fake HTTP) |

---

## Task 1: Nostr — extra tags + external-author reply

**Files:**
- Modify: `internal/nostr/nostr.go`
- Test: `internal/nostr/reply_test.go`, `internal/nostr/nostr_test.go`

Context: `PublishInput{Text, Kind, Imetas, POW, ReplyTo *NostrReply}`; `NostrReply{RootID, ParentID, RelayHint}`; `Publish` builds `event.Tags` from imetas + `replyTags(in.ReplyTo, ownerPubkeyHex)` + a PoW nonce, sets `event.Content = in.Text`, `event.Kind = in.Kind`, signs, publishes. `replyTags` currently emits the OWNER pubkey in the `p` tag and 4-element `e` tags. For an external reply the `p` tag must be the replied-to author and the `e` tags should carry the author as a 5th element.

- [ ] **Step 1: Write the failing tests**

Append to `internal/nostr/reply_test.go`:

```go
func TestReplyTagsExternalAuthor(t *testing.T) {
	// Replying to an external author: p-tag is the replied-to author, and the
	// root e-tag carries that author as the 5th element (NIP-10).
	r := &NostrReply{RootID: "root1", ParentID: "root1", RelayHint: "wss://r", AuthorPubkey: "extpub"}
	tags := replyTags(r, "ownerpub")
	var eTag, pTag []string
	for _, tg := range tags {
		switch tg[0] {
		case "e":
			eTag = tg
		case "p":
			pTag = tg
		}
	}
	if pTag == nil || pTag[1] != "extpub" {
		t.Fatalf("p-tag should be the external author, got %v", pTag)
	}
	if len(eTag) < 5 || eTag[4] != "extpub" {
		t.Errorf("root e-tag should carry author as 5th element: %v", eTag)
	}
}

func TestReplyTagsFallsBackToOwnerWhenNoAuthor(t *testing.T) {
	// Self-thread (no AuthorPubkey): p-tag stays the owner, e-tags need no 5th elem.
	r := &NostrReply{RootID: "x", ParentID: "x", RelayHint: "wss://r"}
	tags := replyTags(r, "ownerpub")
	for _, tg := range tags {
		if tg[0] == "p" && tg[1] != "ownerpub" {
			t.Errorf("self-thread p-tag should be owner, got %v", tg)
		}
	}
}
```

Append to `internal/nostr/nostr_test.go` (if that file doesn't exist, create it `package nostr`):

```go
func TestReplyTagsCarriesAuthorOnDistinctParent(t *testing.T) {
	// Distinct root and parent: both e-tags carry the author 5th element.
	r := &NostrReply{RootID: "root", ParentID: "parent", RelayHint: "wss://r", AuthorPubkey: "auth"}
	tags := replyTags(r, "owner")
	count := 0
	for _, tg := range tags {
		if tg[0] == "e" {
			if len(tg) < 5 || tg[4] != "auth" {
				t.Errorf("e-tag missing author 5th elem: %v", tg)
			}
			count++
		}
	}
	if count != 2 {
		t.Fatalf("want 2 e-tags for distinct root/parent, got %d", count)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/nostr/ -run 'TestReplyTags' -v`
Expected: FAIL — `AuthorPubkey` undefined / no 5th element.

- [ ] **Step 3: Implement** (`internal/nostr/nostr.go`)

Add `AuthorPubkey` to `NostrReply`:
```go
type NostrReply struct {
	RootID, ParentID, RelayHint string
	AuthorPubkey                string // replied-to author (hex); empty → self-thread, p-tag the owner
}
```

Rewrite `replyTags` to honor the external author (the `authorPubkeyHex` param is the OWNER fallback for self-threads):
```go
func replyTags(r *NostrReply, ownerPubkeyHex string) []gonostr.Tag {
	if r == nil {
		return nil
	}
	pAuthor := r.AuthorPubkey // replied-to author for external replies
	if pAuthor == "" {
		pAuthor = ownerPubkeyHex // self-thread: notify the owner (self)
	}
	rootE := gonostr.Tag{"e", r.RootID, r.RelayHint, "root"}
	if r.AuthorPubkey != "" {
		rootE = append(rootE, r.AuthorPubkey) // NIP-10 author hint (5th element)
	}
	if r.ParentID == "" || r.ParentID == r.RootID {
		return []gonostr.Tag{rootE, {"p", pAuthor}}
	}
	replyE := gonostr.Tag{"e", r.ParentID, r.RelayHint, "reply"}
	if r.AuthorPubkey != "" {
		replyE = append(replyE, r.AuthorPubkey)
	}
	return []gonostr.Tag{rootE, replyE, {"p", pAuthor}}
}
```

Add a generic `Tags` pass-through to `PublishInput` (for B2 to build NIP-18 repost/quote tags):
```go
type PublishInput struct {
	Text    string
	Kind    int
	Imetas  []gonostr.Tag
	POW     *int
	ReplyTo *NostrReply
	Tags    []gonostr.Tag // extra tags appended before signing (e.g. NIP-18 q/e/p/k)
}
```

In `Publish`, append `in.Tags` to `event.Tags` (right after the imeta/replyTags appends, before the PoW nonce). Find the block that does `event.Tags = append(event.Tags, im)` for imetas and the `replyTags` append, and add after them:
```go
	for _, tg := range in.Tags {
		event.Tags = append(event.Tags, tg)
	}
```
(Confirm exact placement by reading `Publish`; the only requirement is the extra tags are present on the event before it's signed.)

- [ ] **Step 4: Run tests**

Run: `go test ./internal/nostr/ -v` then `go build ./...`
Expected: PASS — including the EXISTING `replyTags` tests (self-thread behavior unchanged: no `AuthorPubkey` → owner p-tag, 4-element e-tags). Build clean.

- [ ] **Step 5: Commit**

```bash
git add internal/nostr/nostr.go internal/nostr/reply_test.go internal/nostr/nostr_test.go
git commit -m "nostr: external-author reply tags + extra-tags pass-through"
```

---

## Task 2: Bluesky — repost record, quote embed, reply-root

**Files:**
- Modify: `internal/bluesky/bluesky.go`, `internal/bluesky/source.go`
- Test: `internal/bluesky/quote_repost_test.go`

Context: `Post{Text,Langs,Images,ReplyGate,DisableQuotes,Reply *ReplyRef}`; `buildPostRecord(p)` returns the record map; `Post()` adds `record["embed"] = {images}` when images present, then `createRecord(ctx, s, "app.bsky.feed.post", "", record)`. `Result{RemoteID(at uri), RemoteURL, CID}`. `createRecord(ctx, s, collection, rkey, record) (uri, cid, error)`. `webURL(handle, uri)`, `rkeyOf(uri)`. Plan A's `GetPost` returns `*SourcePost` and decodes the record's `text`/`createdAt`.

- [ ] **Step 1: Write the failing test** (`internal/bluesky/quote_repost_test.go`)

```go
package bluesky

import (
	"encoding/json"
	"testing"
)

func TestBuildPostRecordQuoteEmbed(t *testing.T) {
	rec := buildPostRecord(Post{
		Text:  "my take",
		Quote: &QuoteRef{URI: "at://did/app.bsky.feed.post/x", CID: "cidq"},
	})
	embed, ok := rec["embed"].(map[string]any)
	if !ok || embed["$type"] != "app.bsky.embed.record" {
		t.Fatalf("expected embed.record, got %#v", rec["embed"])
	}
	r, _ := embed["record"].(map[string]any)
	if r["uri"] != "at://did/app.bsky.feed.post/x" || r["cid"] != "cidq" {
		t.Errorf("quote strongRef wrong: %#v", r)
	}
	// round-trips as JSON (no non-serializable values)
	if _, err := json.Marshal(rec); err != nil {
		t.Fatalf("record not JSON-serializable: %v", err)
	}
}

func TestRepostRecordShape(t *testing.T) {
	rec := repostRecord("at://did/app.bsky.feed.post/x", "cidr")
	if rec["$type"] != "app.bsky.feed.repost" {
		t.Fatalf("wrong type: %#v", rec)
	}
	subj, _ := rec["subject"].(map[string]any)
	if subj["uri"] != "at://did/app.bsky.feed.post/x" || subj["cid"] != "cidr" {
		t.Errorf("repost subject wrong: %#v", subj)
	}
	if _, ok := rec["createdAt"]; !ok {
		t.Error("repost record needs createdAt")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/bluesky/ -run 'TestBuildPostRecordQuote|TestRepostRecordShape' -v`
Expected: FAIL — `QuoteRef`/`repostRecord` undefined.

- [ ] **Step 3: Implement quote embed + repost** (`internal/bluesky/bluesky.go`)

Add the quote ref type and field on `Post`:
```go
// QuoteRef, when set on a Post, embeds a quoted post (app.bsky.embed.record).
type QuoteRef struct{ URI, CID string }
```
Add to the `Post` struct (after `Reply *ReplyRef`):
```go
	Quote *QuoteRef // when set, embeds the quoted post
```
In `buildPostRecord`, after the `p.Reply` block, add the quote embed:
```go
	if p.Quote != nil {
		record["embed"] = map[string]any{
			"$type":  "app.bsky.embed.record",
			"record": map[string]any{"uri": p.Quote.URI, "cid": p.Quote.CID},
		}
	}
```
In `Post()`, the image-embed code sets `record["embed"] = {images}`. Reconcile quote + images into `recordWithMedia` (a quote post can also carry images). Replace the existing `if len(images) > 0 { record["embed"] = … }` block with:
```go
	if len(images) > 0 {
		imageEmbed := map[string]any{"$type": "app.bsky.embed.images", "images": images}
		if p.Quote != nil {
			// quote + media → recordWithMedia (buildPostRecord already set embed.record)
			record["embed"] = map[string]any{
				"$type":  "app.bsky.embed.recordWithMedia",
				"record": map[string]any{"$type": "app.bsky.embed.record", "record": map[string]any{"uri": p.Quote.URI, "cid": p.Quote.CID}},
				"media":  imageEmbed,
			}
		} else {
			record["embed"] = imageEmbed
		}
	}
```
(With no images and a quote, `buildPostRecord`'s `embed.record` stands. With images and no quote, plain images. With both, recordWithMedia.)

Add a `repostRecord` helper and a `Repost` method:
```go
// repostRecord builds an app.bsky.feed.repost record for the given subject.
func repostRecord(subjectURI, subjectCID string) map[string]any {
	return map[string]any{
		"$type":     "app.bsky.feed.repost",
		"subject":   map[string]any{"uri": subjectURI, "cid": subjectCID},
		"createdAt": time.Now().UTC().Format(time.RFC3339),
	}
}

// Repost creates an app.bsky.feed.repost of the subject post. RemoteURL is the
// owner's repost record URL (best-effort); the important value is the record uri.
func (c *Client) Repost(ctx context.Context, subjectURI, subjectCID string) (Result, error) {
	s, err := c.createSession(ctx)
	if err != nil {
		return Result{}, err
	}
	uri, cid, err := c.createRecord(ctx, s, "app.bsky.feed.repost", "", repostRecord(subjectURI, subjectCID))
	if err != nil {
		return Result{}, fmt.Errorf("repost: %w", err)
	}
	return Result{RemoteID: uri, RemoteURL: webURL(s.Handle, uri), CID: cid}, nil
}
```
(Confirm `fmt` and `time` are imported in bluesky.go — they are, used elsewhere.)

- [ ] **Step 4: Add reply-root to `GetPost`** (`internal/bluesky/source.go`)

Add fields to `SourcePost`:
```go
	ReplyRootURI string
	ReplyRootCID string
```
In `GetPost`, extend the `Record` decode struct to capture the reply root, and copy it:
```go
			Record struct {
				Text      string    `json:"text"`
				CreatedAt time.Time `json:"createdAt"`
				Reply     *struct {
					Root struct {
						URI string `json:"uri"`
						Cid string `json:"cid"`
					} `json:"root"`
				} `json:"reply"`
			} `json:"record"`
```
After building `sp`, set:
```go
	if p.Record.Reply != nil {
		sp.ReplyRootURI = p.Record.Reply.Root.URI
		sp.ReplyRootCID = p.Record.Reply.Root.Cid
	}
```

Add a real fake-HTTP test of the reply-root decode in `quote_repost_test.go`. `GetPost` hits `c.PDS` for createSession + getPosts, and SKIPS resolveHandle when the actor is already a `did:` — so a `did:` URL needs only two stubbed endpoints:
```go
func TestGetPostReplyRoot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/xrpc/com.atproto.server.createSession":
			w.Write([]byte(`{"accessJwt":"jwt","did":"did:plc:me","handle":"me.bsky.social"}`))
		case "/xrpc/app.bsky.feed.getPosts":
			w.Write([]byte(`{"posts":[{
				"uri":"at://did:plc:a/app.bsky.feed.post/x","cid":"cidx",
				"author":{"handle":"a.bsky.social","displayName":"A"},
				"record":{"text":"hi","createdAt":"2026-05-25T10:00:00Z","reply":{"root":{"uri":"at://root","cid":"cidroot"}}},
				"viewer":{}
			}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "id", "pw")
	sp, err := c.GetPost(context.Background(), "https://bsky.app/profile/did:plc:a/post/x")
	if err != nil {
		t.Fatal(err)
	}
	if sp.ReplyRootURI != "at://root" || sp.ReplyRootCID != "cidroot" {
		t.Fatalf("reply root not decoded: %+v", sp)
	}
	// a top-level post (no record.reply) leaves the root fields empty — covered by
	// the empty-viewer post above having a reply, so also assert the non-reply case:
}

func TestGetPostNoReplyRootForTopLevel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/xrpc/com.atproto.server.createSession":
			w.Write([]byte(`{"accessJwt":"jwt","did":"did:plc:me","handle":"me"}`))
		case "/xrpc/app.bsky.feed.getPosts":
			w.Write([]byte(`{"posts":[{"uri":"at://did:plc:a/app.bsky.feed.post/x","cid":"cidx","author":{"handle":"a"},"record":{"text":"hi"},"viewer":{}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "id", "pw")
	sp, _ := c.GetPost(context.Background(), "https://bsky.app/profile/did:plc:a/post/x")
	if sp.ReplyRootURI != "" || sp.ReplyRootCID != "" {
		t.Fatalf("top-level post should have empty reply root: %+v", sp)
	}
}
```
Add imports to `quote_repost_test.go`: `"context"`, `"net/http"`, `"net/http/httptest"` (plus the `encoding/json`/`testing` already used by the earlier tests).

- [ ] **Step 5: Run tests**

Run: `go test ./internal/bluesky/ -v` then `go build ./...`
Expected: PASS (new quote/repost/reply-root + existing). Clean build.

- [ ] **Step 6: Commit**

```bash
git add internal/bluesky/bluesky.go internal/bluesky/source.go internal/bluesky/quote_repost_test.go
git commit -m "bluesky: repost record, quote embed, source reply-root"
```

---

## Task 3: Mastodon — reblog + native quote

**Files:**
- Modify: `internal/mastodon/source.go`
- Test: `internal/mastodon/action_test.go`

Context: Plan A added raw-HTTP fields to `mastodon.Client` (`baseURL`, `token`, `http`) and a `getJSON` GET helper in `source.go`. `Result{RemoteID, RemoteURL}` exists in `mastodon.go`. We add a `postForm` POST helper (mirrors `getJSON`), `Reblog`, and `QuotePost` (native quote via `quoted_status_id`).

- [ ] **Step 1: Write the failing test** (`internal/mastodon/action_test.go`)

```go
package mastodon

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReblog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/statuses/99/reblog" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		// reblog returns a wrapper whose `reblog` is the original; we surface the wrapper.
		w.Write([]byte(`{"id":"100","url":"https://x/@me/100","reblog":{"id":"99","url":"https://x/@a/99"}}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "tok")
	res, err := c.Reblog(context.Background(), "99")
	if err != nil {
		t.Fatal(err)
	}
	if res.RemoteID != "100" {
		t.Errorf("reblog wrapper id wrong: %+v", res)
	}
}

func TestQuotePostSendsQuotedStatusID(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/statuses" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Write([]byte(`{"id":"200","url":"https://x/@me/200"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "tok")
	res, err := c.QuotePost(context.Background(), "my take", "99")
	if err != nil {
		t.Fatal(err)
	}
	if res.RemoteID != "200" {
		t.Errorf("quote id wrong: %+v", res)
	}
	if !strings.Contains(gotBody, "quoted_status_id=99") || !strings.Contains(gotBody, "status=my+take") {
		t.Errorf("quote form missing fields: %q", gotBody)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mastodon/ -run 'TestReblog|TestQuotePost' -v`
Expected: FAIL — `Reblog`/`QuotePost` undefined.

- [ ] **Step 3: Implement** (`internal/mastodon/source.go`)

Add a form-POST helper (mirrors `getJSON`) and the two methods:
```go
import "net/url" // already imported in source.go

// postForm POSTs application/x-www-form-urlencoded and decodes JSON into out.
func (c *Client) postForm(ctx context.Context, path string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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

// Reblog boosts a status. Returns the reblog wrapper (its `reblog` is the original).
func (c *Client) Reblog(ctx context.Context, id string) (Result, error) {
	var st struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if err := c.postForm(ctx, "/api/v1/statuses/"+id+"/reblog", nil, &st); err != nil {
		return Result{}, fmt.Errorf("reblog: %w", err)
	}
	return Result{RemoteID: st.ID, RemoteURL: st.URL}, nil
}

// QuotePost creates a native quote post (server 4.5+). text is the commentary;
// quotedID is the LOCAL status id to quote.
func (c *Client) QuotePost(ctx context.Context, text, quotedID string) (Result, error) {
	var st struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	form := url.Values{"status": {text}, "quoted_status_id": {quotedID}}
	if err := c.postForm(ctx, "/api/v1/statuses", form, &st); err != nil {
		return Result{}, fmt.Errorf("quote: %w", err)
	}
	return Result{RemoteID: st.ID, RemoteURL: st.URL}, nil
}
```
(Confirm `io`, `fmt`, `encoding/json`, `net/http`, `strings`, `net/url` are imported in source.go — `getJSON` already uses most; add any missing.)

- [ ] **Step 4: Run tests**

Run: `go test ./internal/mastodon/ -v` then `go test ./... && go build ./...`
Expected: PASS, clean.

- [ ] **Step 5: Commit**

```bash
git add internal/mastodon/source.go internal/mastodon/action_test.go
git commit -m "mastodon: reblog + native quote-post primitives"
```

---

## Task 4: Verify the whole B1 surface builds + tests green

**Files:** none (verification only)

- [ ] **Step 1: Full verification**

Run:
```bash
go test ./...
go vet ./...
go build ./cmd/publisher
```
Expected: ALL pass, vet clean, build OK. In particular confirm the EXISTING nostr threading tests (`reply_test.go` self-thread cases) still pass after the `replyTags` change, and `dispatch` (which calls the nostr/bluesky/mastodon clients via adapters) still builds — B1 only ADDS methods/fields, so the dispatch adapters and their `*Poster` interfaces are unaffected.

- [ ] **Step 2: Commit (if any incidental fixes were needed; otherwise skip)**

If verification surfaced a needed tweak, commit it:
```bash
git add -A && git commit -m "b1: fixups from full verification"
```

---

## Self-Review notes (for the implementer)

- **Spec coverage (B1 portion):** NIP-10 external-author reply (Task 1); NIP-18 repost/quote *tag pass-through* enabling B2 to build kind 6/16 + q-tag (Task 1 `PublishInput.Tags`); Bluesky `embed.record` quote + `feed.repost` + reply-root for thread derivation (Task 2); Mastodon reblog + native `quoted_status_id` quote (Task 3). **Deferred to B2:** the dispatch interaction orchestration (which builds the actual NIP-18 tags/content, derives the Bluesky reply root, chooses native-vs-link quote, fans out), the store action descriptor, `/api/interact`, override, and the Interact-tab actions + history rendering. Threads quote/repost are intentionally absent (no URL→id resolver; its fan-out is a normal post).
- **Backward compatibility:** every change is additive. `replyTags`'s new behavior is gated on `AuthorPubkey != ""`, so the self-thread path (threading feature) is byte-identical (owner p-tag, 4-element e-tags). `Post.Quote`/`Post` image reconciliation only changes behavior when `Quote != nil`. New client methods don't touch existing `Post`/`Publish` call sites.
- **Type names (do not rename, B2 depends on them):** `nostr.NostrReply.AuthorPubkey`, `nostr.PublishInput.Tags`, `bluesky.QuoteRef{URI,CID}`, `bluesky.Post.Quote`, `bluesky.Client.Repost`, `bluesky.SourcePost.ReplyRootURI/ReplyRootCID`, `mastodon.Client.Reblog`, `mastodon.Client.QuotePost`.
- **Test strategy:** pure record/tag shape tests (no network) for Bluesky/Nostr; fake-HTTP tests for Mastodon reblog/quote. Live posting is exercised manually in B2.
