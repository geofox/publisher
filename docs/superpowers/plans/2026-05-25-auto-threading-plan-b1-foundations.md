# Auto-Threading — Plan B1: Foundations (store chain + per-platform reply primitives)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Lay the purely-additive foundations for threaded posting — let the store persist an ordered segment chain per target, and let each of the four platform clients post a single segment *as a reply* — without changing how the dispatcher posts yet.

**Architecture:** Additive only. `store.Target` gains a `Segments []Segment` field persisted as JSON in a new `segments_json` column (idempotent migration). Each client's `Post`/`Publish` gains an optional reply reference (nil → today's top-level behavior, so existing callers and tests are unaffected). Reply-construction logic is extracted into pure helpers so it's unit-testable without network. The dispatcher/adapters are untouched here — they get wired in Plan B2.

**Tech Stack:** Go 1.26, `modernc.org/sqlite`, `fiatjaf.com/nostr` (NIP-10), `github.com/mattn/go-mastodon`, stdlib `net/http/httptest` for client tests.

**Spec:** `docs/superpowers/specs/2026-05-25-auto-threading-composer-design.md` (§3 reply threading, §4 store model — the data/primitive layer only).

**Scope note:** Plan B2 (the next plan) adds: `ReplyRef` + `TargetResult.CID`, the adapter signature changes, per-platform sequential chain dispatch, stop-&-resume, the `number` plumbing through `/api/post`, and history rendering. B1 does NOT change `internal/dispatch`, `internal/api`, or the web assets.

---

## File Structure

| File | Responsibility (B1) |
|---|---|
| `internal/store/models.go` | `Segment` struct; `Segments` field on `Target` |
| `internal/store/store.go` | `segments_json` column migration; persist/load segments |
| `internal/store/models_test.go` | chain round-trip test |
| `internal/bluesky/bluesky.go` | `ReplyRef` + `Post.Reply`; extract `buildPostRecord`; emit `reply` |
| `internal/mastodon/mastodon.go` | `Post.InReplyToID` → `Toot.InReplyToID` |
| `internal/nostr/nostr.go` | `PublishInput.ReplyTo` (`NostrReply`); `replyTags` helper → NIP-10 tags |
| `internal/threads/threads.go` | `Post.ReplyToID`; `addReplyTo` helper on all containers |
| `internal/{bluesky,nostr,threads,mastodon}/*_test.go` | reply-primitive unit tests |

---

## Task 1: Store — segment chain model + persistence

**Files:**
- Modify: `internal/store/models.go`
- Modify: `internal/store/store.go`
- Test: `internal/store/models_test.go`

- [ ] **Step 1: Write the failing test** (append to `internal/store/models_test.go`)

```go
func TestSavePostWithSegments(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "seg.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rec := &Post{
		ID: "seg1", CreatedAt: time.Now().UTC().Truncate(time.Second),
		MasterText: "long", Platforms: []string{"bluesky"}, Source: "web", Status: "partial",
		Targets: []Target{{
			Platform: "bluesky", FinalText: "long", Status: "partial",
			RemoteID: "at://seg0", RemoteURL: "https://bsky/0",
			Segments: []Segment{
				{Ordinal: 0, Text: "seg zero 1/2", RemoteID: "at://seg0", RemoteURL: "https://bsky/0", CID: "cid0", Status: "success"},
				{Ordinal: 1, Text: "seg one 2/2", Status: "failed", Error: "boom"},
			},
			Attempts: []Attempt{{AttemptNo: 1, Status: "partial", AttemptedAt: time.Now()}},
		}},
	}
	if err := db.SavePost(rec); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetPost("seg1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Targets) != 1 {
		t.Fatalf("targets: %+v", got.Targets)
	}
	segs := got.Targets[0].Segments
	if len(segs) != 2 {
		t.Fatalf("segments not round-tripped: %+v", segs)
	}
	if segs[0].CID != "cid0" || segs[0].Status != "success" || segs[1].Status != "failed" || segs[1].Error != "boom" {
		t.Errorf("segment fields wrong: %+v", segs)
	}
}

func TestSavePostNoSegmentsIsEmpty(t *testing.T) {
	db, _ := Open(filepath.Join(t.TempDir(), "noseg.db"))
	defer db.Close()
	rec := &Post{
		ID: "ns1", CreatedAt: time.Now().UTC().Truncate(time.Second),
		Platforms: []string{"mastodon"}, Source: "web", Status: "success",
		Targets: []Target{{Platform: "mastodon", Status: "success", RemoteID: "m1",
			Attempts: []Attempt{{AttemptNo: 1, Status: "success", AttemptedAt: time.Now()}}}},
	}
	if err := db.SavePost(rec); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetPost("ns1")
	if len(got.Targets[0].Segments) != 0 {
		t.Fatalf("expected no segments, got %+v", got.Targets[0].Segments)
	}
}
```

(The test file already imports `filepath`, `time`, `testing`.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestSavePostWithSegments -v`
Expected: FAIL — `Segment` undefined / `Segments` field missing.

- [ ] **Step 3: Add the `Segment` type and `Segments` field** (`internal/store/models.go`)

Add the struct (e.g. after `RelayState`):
```go
// Segment is one post in a platform's reply-chain. A non-threaded target has no
// segments; a threaded one has an ordered slice (ordinal 0 = the chain head).
type Segment struct {
	Ordinal   int    `json:"ordinal"`
	Text      string `json:"text"`
	RemoteID  string `json:"remote_id,omitempty"`
	RemoteURL string `json:"remote_url,omitempty"`
	CID       string `json:"cid,omitempty"` // bluesky only
	Status    string `json:"status"`        // success | failed | pending
	Error     string `json:"error,omitempty"`
}
```
Add the field to `Target` (after `Relays`):
```go
	Segments []Segment `json:"segments,omitempty"`
```

- [ ] **Step 4: Migrate + persist + load** (`internal/store/store.go`)

In `migrate()`, add another idempotent column (after the existing `addColumnIfMissing` calls, before the final `return`):
```go
	if err := s.addColumnIfMissing("post_targets", "segments_json", "TEXT"); err != nil {
		return err
	}
```
(Adjust so the function still returns the last call's error; e.g. keep the existing final `return s.addColumnIfMissing("posts", "hidden", ...)` as the last statement and put the segments column before it.)

In `SavePost`, the `post_targets` INSERT must also write `segments_json`. Change the INSERT to include the column and a marshaled value. Replace the existing target INSERT block:
```go
		segJSON := ""
		if len(tg.Segments) > 0 {
			b, mErr := json.Marshal(tg.Segments)
			if mErr != nil {
				return mErr
			}
			segJSON = string(b)
		}
		res, err := tx.Exec(
			`INSERT INTO post_targets(post_id,platform,final_text,fields_json,status,remote_id,remote_url,latency_ms,attempt_count,last_attempt_at,signed_event_json,segments_json)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			p.ID, tg.Platform, tg.FinalText, tg.FieldsJSON, tg.Status, tg.RemoteID, tg.RemoteURL,
			tg.LatencyMS, len(tg.Attempts), time.Now().UTC(), tg.SignedEventJSON, segJSON,
		)
		if err != nil {
			return err
		}
```

In `GetPost`, the `post_targets` SELECT must read `segments_json` and unmarshal it. Change the targets query + scan:
```go
	trows, err := s.sql.Query(`SELECT id,platform,final_text,fields_json,status,remote_id,remote_url,latency_ms,attempt_count,signed_event_json,segments_json FROM post_targets WHERE post_id=? ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer trows.Close()
	for trows.Next() {
		var tg Target
		var fields, rid, rurl, sej, segs sql.NullString
		if err := trows.Scan(&tg.ID, &tg.Platform, &tg.FinalText, &fields, &tg.Status, &rid, &rurl, &tg.LatencyMS, &tg.AttemptCount, &sej, &segs); err != nil {
			return nil, err
		}
		tg.FieldsJSON, tg.RemoteID, tg.RemoteURL, tg.SignedEventJSON = fields.String, rid.String, rurl.String, sej.String
		if segs.String != "" {
			if err := json.Unmarshal([]byte(segs.String), &tg.Segments); err != nil {
				return nil, err
			}
		}
		p.Targets = append(p.Targets, tg)
	}
```

(`json`, `time`, `database/sql` are already imported in store.go.)

- [ ] **Step 5: Run tests**

Run: `go test ./internal/store/ -v`
Expected: PASS (new segment tests + all existing store tests — the additive column doesn't disturb existing rows).

- [ ] **Step 6: Commit**

```bash
git add internal/store/models.go internal/store/store.go internal/store/models_test.go
git commit -m "store: persist per-target segment chain (segments_json column)"
```

---

## Task 2: Bluesky — reply primitive

**Files:**
- Modify: `internal/bluesky/bluesky.go`
- Test: `internal/bluesky/bluesky_record_test.go`

- [ ] **Step 1: Write the failing test** (`internal/bluesky/bluesky_record_test.go`)

```go
package bluesky

import "testing"

func TestBuildPostRecordNoReply(t *testing.T) {
	rec := buildPostRecord(Post{Text: "hi"})
	if rec["$type"] != "app.bsky.feed.post" || rec["text"] != "hi" {
		t.Fatalf("base record wrong: %+v", rec)
	}
	if _, ok := rec["reply"]; ok {
		t.Errorf("no reply expected: %+v", rec["reply"])
	}
}

func TestBuildPostRecordWithReply(t *testing.T) {
	rec := buildPostRecord(Post{Text: "second", Reply: &ReplyRef{
		RootURI: "at://root", RootCID: "cidR", ParentURI: "at://par", ParentCID: "cidP",
	}})
	reply, ok := rec["reply"].(map[string]any)
	if !ok {
		t.Fatalf("reply missing/!map: %+v", rec["reply"])
	}
	root := reply["root"].(map[string]any)
	parent := reply["parent"].(map[string]any)
	if root["uri"] != "at://root" || root["cid"] != "cidR" {
		t.Errorf("root wrong: %+v", root)
	}
	if parent["uri"] != "at://par" || parent["cid"] != "cidP" {
		t.Errorf("parent wrong: %+v", parent)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/bluesky/ -run TestBuildPostRecord -v`
Expected: FAIL — `buildPostRecord` / `ReplyRef` undefined.

- [ ] **Step 3: Add `ReplyRef`, the `Reply` field, and extract `buildPostRecord`**

Add the type (near the `Post` struct):
```go
// ReplyRef threads a post into an existing conversation. For a self-thread the
// root is the chain's first post and the parent is the immediately-preceding one.
type ReplyRef struct {
	RootURI, RootCID, ParentURI, ParentCID string
}
```
Add a field to `Post` (after `DisableQuotes`):
```go
	// Reply, when non-nil, makes this post a reply (used for threading).
	Reply *ReplyRef
```
Extract record construction into a pure helper (factor out the part of `Post` that builds the `record` map, and add the reply branch). Add:
```go
// buildPostRecord assembles the text/langs/facets/reply portion of an
// app.bsky.feed.post record. The image embed is added by the caller (it needs
// uploaded blobs), keeping this helper pure and network-free.
func buildPostRecord(p Post) map[string]any {
	record := map[string]any{
		"$type":     "app.bsky.feed.post",
		"text":      p.Text,
		"createdAt": time.Now().UTC().Format(time.RFC3339),
	}
	if len(p.Langs) > 0 {
		record["langs"] = p.Langs
	}
	if f := parseFacets(p.Text); len(f) > 0 {
		record["facets"] = f
	}
	if p.Reply != nil {
		record["reply"] = map[string]any{
			"root":   map[string]any{"uri": p.Reply.RootURI, "cid": p.Reply.RootCID},
			"parent": map[string]any{"uri": p.Reply.ParentURI, "cid": p.Reply.ParentCID},
		}
	}
	return record
}
```
Then in `Post`, replace the inline `record := map[string]any{...}` + langs + facets construction with:
```go
	record := buildPostRecord(p)
```
Keep the existing image-embed block that sets `record["embed"]` AFTER this call (it appends to the returned map), and the rest of `Post` (createRecord, gates) unchanged.

NOTE: the test calls `buildPostRecord(Post{...}, nil)` with no images, so `buildPostRecord` itself only handles text/langs/facets/reply; the image embed stays in `Post` (it needs the uploaded blobs). This keeps the helper pure and network-free.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/bluesky/ -v`
Expected: PASS (new record tests + existing bluesky tests).

- [ ] **Step 5: Commit**

```bash
git add internal/bluesky/bluesky.go internal/bluesky/bluesky_record_test.go
git commit -m "bluesky: support reply records (threading primitive)"
```

---

## Task 3: Mastodon — reply primitive

**Files:**
- Modify: `internal/mastodon/mastodon.go`
- Test: `internal/mastodon/mastodon_reply_test.go`

- [ ] **Step 1: Write the failing test** (`internal/mastodon/mastodon_reply_test.go`)

This stands up a fake Mastodon API and asserts the `in_reply_to_id` form field is sent.

```go
package mastodon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPostSendsInReplyToID(t *testing.T) {
	var gotReplyTo string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotReplyTo = r.FormValue("in_reply_to_id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"222","url":"https://m.example/@a/222"}`))
	}))
	defer srv.Close()

	cl := New(srv.URL, "token")
	res, err := cl.Post(context.Background(), Post{Text: "reply body", InReplyToID: "111"})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if gotReplyTo != "111" {
		t.Errorf("in_reply_to_id = %q, want 111", gotReplyTo)
	}
	if res.RemoteID != "222" {
		t.Errorf("remote id = %q", res.RemoteID)
	}
}
```

NOTE: first check whether `internal/mastodon/` already has a test that stands up an httptest server (the package may have a helper/pattern). If so, mirror it. If `New`'s signature differs, adapt the construction.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mastodon/ -run TestPostSendsInReplyToID -v`
Expected: FAIL — `Post` has no field `InReplyToID`.

- [ ] **Step 3: Add the field and map it to the Toot**

Add to the `Post` struct (after `Images`):
```go
	InReplyToID string // when set, posts as a reply (threading)
```
In `Post`, set it on the toot — change the `gomast.Toot{...}` literal to include:
```go
	st, err := cl.c.PostStatus(ctx, &gomast.Toot{
		Status: p.Text, SpoilerText: p.SpoilerText, Sensitive: p.Sensitive,
		Visibility: p.Visibility, Language: p.Language, MediaIDs: mediaIDs,
		InReplyToID: gomast.ID(p.InReplyToID),
	})
```
(`gomast.Toot` has field `InReplyToID ID`, confirmed via `go doc github.com/mattn/go-mastodon Toot`. An empty `gomast.ID("")` serializes to no/empty `in_reply_to_id`, preserving top-level behavior when unset.)

- [ ] **Step 4: Run tests**

Run: `go test ./internal/mastodon/ -v`
Expected: PASS. If go-mastodon omits the form field when the ID is empty, also confirm a non-reply post still works (existing tests cover this).

- [ ] **Step 5: Commit**

```bash
git add internal/mastodon/mastodon.go internal/mastodon/mastodon_reply_test.go
git commit -m "mastodon: support in_reply_to_id (threading primitive)"
```

---

## Task 4: Nostr — reply primitive (NIP-10 tags)

**Files:**
- Modify: `internal/nostr/nostr.go`
- Test: `internal/nostr/reply_test.go`

- [ ] **Step 1: Write the failing test** (`internal/nostr/reply_test.go`)

```go
package nostr

import "testing"

func TestReplyTagsRootAndParent(t *testing.T) {
	tags := replyTags(&NostrReply{RootID: "rootid", ParentID: "parentid", RelayHint: "wss://r"}, "mypub")
	// Expect: e root, e reply, p author. Find them.
	var eRoot, eReply, pTag []string
	for _, tg := range tags {
		switch {
		case len(tg) >= 4 && tg[0] == "e" && tg[3] == "root":
			eRoot = tg
		case len(tg) >= 4 && tg[0] == "e" && tg[3] == "reply":
			eReply = tg
		case len(tg) >= 2 && tg[0] == "p":
			pTag = tg
		}
	}
	if eRoot == nil || eRoot[1] != "rootid" || eRoot[2] != "wss://r" {
		t.Errorf("root e-tag wrong: %v", eRoot)
	}
	if eReply == nil || eReply[1] != "parentid" || eReply[2] != "wss://r" {
		t.Errorf("reply e-tag wrong: %v", eReply)
	}
	if pTag == nil || pTag[1] != "mypub" {
		t.Errorf("p-tag wrong: %v", pTag)
	}
}

func TestReplyTagsRootEqualsParentForFirstReply(t *testing.T) {
	// Replying directly to the root: root and parent are the same id; still emit
	// both markers (NIP-10 permits it and clients handle it).
	tags := replyTags(&NostrReply{RootID: "x", ParentID: "x", RelayHint: ""}, "p")
	n := 0
	for _, tg := range tags {
		if tg[0] == "e" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("expected 2 e-tags, got %d (%v)", n, tags)
	}
}

func TestReplyTagsNil(t *testing.T) {
	if tags := replyTags(nil, "p"); tags != nil {
		t.Errorf("nil reply → nil tags, got %v", tags)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/nostr/ -run TestReplyTags -v`
Expected: FAIL — `replyTags` / `NostrReply` undefined.

- [ ] **Step 3: Add `NostrReply`, `replyTags`, and wire into `Publish`**

Add the type (near `PublishInput`):
```go
// NostrReply carries NIP-10 reply context for a threaded event. RootID is the
// chain's first event; ParentID is the immediately-preceding one (equal to
// RootID when replying directly to the root). RelayHint is an optional relay URL.
type NostrReply struct {
	RootID, ParentID, RelayHint string
}
```
Add a field to `PublishInput` (after `POW`):
```go
	ReplyTo *NostrReply // when set, adds NIP-10 e/p tags (threading)
```
Add the helper:
```go
// replyTags builds NIP-10 tags: an "e" root marker, an "e" reply marker, and a
// "p" tag for the replied-to author (here, the owner — a self-thread).
func replyTags(r *NostrReply, authorPubkeyHex string) []gonostr.Tag {
	if r == nil {
		return nil
	}
	return []gonostr.Tag{
		{"e", r.RootID, r.RelayHint, "root"},
		{"e", r.ParentID, r.RelayHint, "reply"},
		{"p", authorPubkeyHex},
	}
}
```
In `Publish`, append the reply tags to the event BEFORE mining/signing. After the imeta-tags loop and before the POW block, add:
```go
	for _, tg := range replyTags(in.ReplyTo, p.cfg.OwnerPubkey.Hex()) {
		event.Tags = append(event.Tags, tg)
	}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/nostr/ -v`
Expected: PASS (new reply tests + existing nostr tests).

- [ ] **Step 5: Commit**

```bash
git add internal/nostr/nostr.go internal/nostr/reply_test.go
git commit -m "nostr: NIP-10 reply tags (threading primitive)"
```

---

## Task 5: Threads — reply primitive

**Files:**
- Modify: `internal/threads/threads.go`
- Test: `internal/threads/reply_test.go`

- [ ] **Step 1: Write the failing test** (`internal/threads/reply_test.go`)

```go
package threads

import (
	"net/url"
	"testing"
)

func TestAddReplyTo(t *testing.T) {
	v := url.Values{}
	(&Client{}).addReplyTo(v, "999")
	if v.Get("reply_to_id") != "999" {
		t.Errorf("reply_to_id = %q, want 999", v.Get("reply_to_id"))
	}
}

func TestAddReplyToEmpty(t *testing.T) {
	v := url.Values{}
	(&Client{}).addReplyTo(v, "")
	if _, ok := v["reply_to_id"]; ok {
		t.Errorf("empty reply id must not set the param: %v", v)
	}
}
```

NOTE: if `Client` has unexported required fields making `&Client{}` insufficient, make `addReplyTo` a package function `addReplyTo(v url.Values, id string)` instead of a method and adjust the test calls accordingly. Match how `addTopic`/`addReplyControl` are defined (method vs function) in the file.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/threads/ -run TestAddReplyTo -v`
Expected: FAIL — `addReplyTo` / `ReplyToID` undefined.

- [ ] **Step 3: Add the field, the helper, and call it in every container**

Add to the `Post` struct (after `ReplyControl`):
```go
	ReplyToID string // when set, posts as a reply to this media id (threading)
```
Add the helper next to `addTopic`/`addReplyControl` (match their receiver style — they are methods `func (c *Client) addTopic(...)`):
```go
// addReplyTo sets reply_to_id when id is non-empty, making the container a reply.
func (c *Client) addReplyTo(v url.Values, id string) {
	if id != "" {
		v.Set("reply_to_id", id)
	}
}
```
In `createMain`, call `c.addReplyTo(v, p.ReplyToID)` alongside the existing `c.addTopic`/`c.addReplyControl` calls in ALL THREE cases (the `case 0` TEXT container, the `case 1` IMAGE container, and the `default` CAROUSEL parent container `pv`). For the carousel, set it on the parent `pv` only (not the child image containers).

- [ ] **Step 4: Run tests**

Run: `go test ./internal/threads/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/threads/threads.go internal/threads/reply_test.go
git commit -m "threads: support reply_to_id (threading primitive)"
```

---

## Task 6: Full verification

**Files:** none (verification only)

- [ ] **Step 1: Full suite + vet + build**

Run:
```bash
go test ./...
go vet ./...
go build ./cmd/publisher
```
Expected: ALL packages PASS (including the unchanged `internal/dispatch`, `internal/api`, `internal/web` — B1 is additive and must not regress them), vet clean, build succeeds. If any failure, STOP and report it.

- [ ] **Step 2: Confirm additivity**

Run: `git diff --stat main..HEAD -- internal/dispatch internal/api internal/web`
Expected: EMPTY — B1 must not have touched dispatch, api, or web (those are Plan B2). If non-empty, something leaked; report it.

- [ ] **Step 3: Commit (only if a verification artifact changed; otherwise skip)**

No code changes in this task. If `go mod tidy` altered nothing, there is nothing to commit. (Do not create an empty commit.)

---

## Self-Review notes (for the implementer)

- **Spec coverage (B1 = data layer + reply primitives):** store chain persistence (Task 1), Bluesky reply record (Task 2), Mastodon `in_reply_to_id` (Task 3), Nostr NIP-10 tags (Task 4), Threads `reply_to_id` (Task 5). Orchestration/resume/UI are explicitly Plan B2.
- **Additivity:** every change is backward-compatible — new optional struct fields default to nil/"" (top-level behavior), and the `segments_json` column is added idempotently and read as empty for old rows. Existing dispatch/api/web code compiles and passes unchanged.
- **Testability:** reply logic is unit-tested via pure helpers (`buildPostRecord`, `replyTags`, `addReplyTo`) or a local `httptest` server (Mastodon), with no live network.
- **Pre-existing patterns to match:** check each client package for an existing test file and mirror its style; confirm `addTopic`/`addReplyControl` receiver style before adding `addReplyTo`; confirm `mastodon.New` signature before the httptest test.
- **Type names that B2 depends on:** `store.Segment`, `Target.Segments`, `bluesky.ReplyRef`, `bluesky.Post.Reply`, `mastodon.Post.InReplyToID`, `nostr.NostrReply`, `nostr.PublishInput.ReplyTo`, `threads.Post.ReplyToID`. Keep these names stable.
