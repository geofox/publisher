# Public Feed API + Publish Webhook Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a read-only `GET /api/public/feed` returning the latest public master posts as custom JSON (content + media + per-platform links), plus a signal-only webhook that pings an external consumer when a feed-eligible post is published.

**Architecture:** A new `internal/feed` package owns feed semantics — the DTO shape, the `Eligible` predicate, the `Build` reshaper, and the outbound `Webhook`. `internal/store` gains a `PublicFeed` query (newest-first by first-success time, hydrating only what the feed needs). `internal/api` adds a token-gated handler. `internal/dispatch` calls an injected `PostNotifier` at each terminal publish; `feed.Webhook` implements it and applies the same `Eligible` gate. Config + `cmd/publisher` wire three new env vars.

**Tech Stack:** Go 1.26, stdlib `net/http`, `modernc.org/sqlite`, `crypto/subtle`. Tests use stdlib `testing` + `net/http/httptest`.

**Spec:** `docs/superpowers/specs/2026-05-28-public-feed-api-design.md`

---

## Background the implementer needs

- A **master post** is `store.Post` (`internal/store/models.go:19`). It holds
  per-platform `store.Target`s (`models.go:34`). Each successful target already
  stores a public web URL in `Target.RemoteURL` — including Nostr, which gets
  `https://njump.me/<nevent>` (`internal/dispatch/adapters.go:58`). The feed
  reads `RemoteURL` directly; it never constructs URLs.
- **Per-post visibility** exists only for Mastodon. It lives in the target's
  `fields_json` column under key `visibility` (produced by `ov2fields` at
  `internal/dispatch/dispatch.go:732`; values `""|public|unlisted|private|direct`).
  Bluesky/Nostr/Threads have no per-post visibility.
- **Publish times** are recorded as `target_attempts` rows: the initial publish
  writes attempt #1 with its status + timestamp (`dispatch.go:503`), retries
  append later rows (`store.AppendTargetAttempt`, `models.go:387`). So
  `MIN(attempted_at WHERE status='success')` is the first-success time and a
  later retry never lowers it.
- **JSON helpers:** `httpx.WriteJSON(w, status, body)` and
  `httpx.WriteError(w, status, msg)` (`internal/httpx/httpx.go`).
- **Routes** are registered in `(*API).Routes()` at `internal/api/api.go:117`;
  GET routes pass the existing CSRF/security middleware unchanged.
- Run a single package's tests with e.g. `go test ./internal/feed/ -run TestX -v`.
  Run everything with `go test ./...`.

## File structure

- Modify `internal/store/models.go` — add `Post.FirstSuccessAt *time.Time` field.
- Create `internal/store/feed.go` — `(*Store).PublicFeed(limit int)`.
- Create `internal/store/feed_test.go` — PublicFeed query tests.
- Create `internal/feed/feed.go` — DTOs, `Eligible`, `Build`, helpers.
- Create `internal/feed/feed_test.go` — predicate + reshape tests.
- Create `internal/feed/webhook.go` — `Webhook`, `NewWebhook`, `PostPublished`.
- Create `internal/feed/webhook_test.go` — webhook delivery + gating tests.
- Modify `internal/dispatch/dispatch.go` — `PostNotifier` interface, `Notify`
  field, `notify` helper, four call sites.
- Create `internal/dispatch/notify_test.go` — wiring tests.
- Modify `internal/api/api.go` — `API.PublicFeedToken`, route, `handlePublicFeed`.
- Create `internal/api/public_feed_test.go` — handler auth + shape tests.
- Modify `internal/config/config.go` — three new env vars.
- Modify `cmd/publisher/main.go` — wire token + notifier.
- Modify `README.md` — document env vars + endpoint.

---

## Task 1: Store — `FirstSuccessAt` field + `PublicFeed` query

**Files:**
- Modify: `internal/store/models.go:19-32` (Post struct)
- Create: `internal/store/feed.go`
- Test: `internal/store/feed_test.go`

- [ ] **Step 1: Add the `FirstSuccessAt` field to `Post`**

In `internal/store/models.go`, add the field to the `Post` struct (after
`FiredAt` at line 28). The `json:"-"` tag keeps it out of every existing
endpoint's response; only `PublicFeed` sets it and only `feed.Build` reads it.

```go
	FiredAt      *time.Time   `json:"fired_at,omitempty"` // list view: latest target attempt time (actual publish/retry)
	// FirstSuccessAt is the earliest time the post went live on ANY platform
	// (MIN over successful attempts). Set only by PublicFeed, never serialized.
	// Retries append later attempt rows, so this never moves once set.
	FirstSuccessAt *time.Time `json:"-"`
	Targets      []Target     `json:"targets,omitempty"`
```

- [ ] **Step 2: Write the failing test**

Create `internal/store/feed_test.go`:

```go
package store

import (
	"path/filepath"
	"testing"
	"time"
)

// mkFeedPost saves a post with explicit per-target attempts so tests control
// the first-success time. Each target gets one attempt per (status, time) pair.
func mkFeedPost(t *testing.T, db *Store, id, status string, targets []Target) {
	t.Helper()
	rec := &Post{
		ID:         id,
		CreatedAt:  time.Now().UTC(),
		MasterText: "text " + id,
		Platforms:  []string{},
		Source:     "web",
		Status:     status,
		Targets:    targets,
	}
	for _, tg := range targets {
		rec.Platforms = append(rec.Platforms, tg.Platform)
	}
	if err := db.SavePost(rec); err != nil {
		t.Fatalf("mkFeedPost %q: %v", id, err)
	}
}

func TestPublicFeedFirstSuccessAndOrder(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	t1 := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC) // a later retry
	t3 := time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC)

	// Older post: nostr succeeded at t1; mastodon failed at t1 then succeeded
	// on retry at t2. First success = t1; a retry must not move it.
	mkFeedPost(t, db, "old", "success", []Target{
		{Platform: "nostr", Status: "success", RemoteURL: "https://njump.me/x",
			Attempts: []Attempt{{AttemptNo: 1, Status: "success", AttemptedAt: t1}}},
		{Platform: "mastodon", Status: "success", RemoteURL: "https://m/1", FieldsJSON: `{"visibility":"public"}`,
			Attempts: []Attempt{
				{AttemptNo: 1, Status: "failed", AttemptedAt: t1},
				{AttemptNo: 2, Status: "success", AttemptedAt: t2},
			}},
	})
	// Newer post: nostr succeeded at t3.
	mkFeedPost(t, db, "new", "success", []Target{
		{Platform: "nostr", Status: "success", RemoteURL: "https://njump.me/y",
			Attempts: []Attempt{{AttemptNo: 1, Status: "success", AttemptedAt: t3}}},
	})

	posts, err := db.PublicFeed(20)
	if err != nil {
		t.Fatalf("PublicFeed: %v", err)
	}
	if len(posts) != 2 {
		t.Fatalf("got %d posts, want 2", len(posts))
	}
	// Ordered by first-success DESC → "new" (t3) before "old" (t1).
	if posts[0].ID != "new" || posts[1].ID != "old" {
		t.Fatalf("order = [%s,%s], want [new,old]", posts[0].ID, posts[1].ID)
	}
	if posts[1].FirstSuccessAt == nil || !posts[1].FirstSuccessAt.Equal(t1) {
		t.Fatalf("old FirstSuccessAt = %v, want %v (retry must not move it)", posts[1].FirstSuccessAt, t1)
	}
	// Targets hydrated with remote_url + fields_json.
	var masto *Target
	for i := range posts[1].Targets {
		if posts[1].Targets[i].Platform == "mastodon" {
			masto = &posts[1].Targets[i]
		}
	}
	if masto == nil || masto.RemoteURL != "https://m/1" || masto.FieldsJSON != `{"visibility":"public"}` {
		t.Fatalf("mastodon target not hydrated: %+v", masto)
	}
}

func TestPublicFeedExcludesHiddenAndUnpublished(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ts := time.Date(2026, 5, 22, 10, 0, 0, 0, time.UTC)
	ok := []Target{{Platform: "nostr", Status: "success", RemoteURL: "https://njump.me/z",
		Attempts: []Attempt{{AttemptNo: 1, Status: "success", AttemptedAt: ts}}}}

	mkFeedPost(t, db, "shown", "success", ok)
	mkFeedPost(t, db, "hidden", "success", ok)
	mkFeedPost(t, db, "scheduled", "scheduled", []Target{{Platform: "nostr", Status: "scheduled"}})
	mkFeedPost(t, db, "failed", "failed", []Target{{Platform: "nostr", Status: "failed",
		Attempts: []Attempt{{AttemptNo: 1, Status: "failed", AttemptedAt: ts}}}})

	if err := db.HidePost("hidden"); err != nil {
		t.Fatalf("HidePost: %v", err)
	}

	posts, err := db.PublicFeed(20)
	if err != nil {
		t.Fatalf("PublicFeed: %v", err)
	}
	if len(posts) != 1 || posts[0].ID != "shown" {
		ids := make([]string, len(posts))
		for i, p := range posts {
			ids[i] = p.ID
		}
		t.Fatalf("got %v, want [shown] (hidden/scheduled/failed excluded)", ids)
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/store/ -run TestPublicFeed -v`
Expected: FAIL — `db.PublicFeed undefined`.

- [ ] **Step 4: Implement `PublicFeed`**

Create `internal/store/feed.go`:

```go
package store

import (
	"database/sql"
	"encoding/json"
	"time"
)

// firstSuccessExpr is the SQL fragment for a post's first-success time: the
// earliest target_attempts.attempted_at across the post's targets whose attempt
// succeeded. Used for both ORDER BY and the FirstSuccessAt projection.
const firstSuccessExpr = `(SELECT MIN(ta.attempted_at) FROM target_attempts ta
	JOIN post_targets pt ON ta.target_id = pt.id
	WHERE pt.post_id = p.id AND ta.status = 'success')`

// PublicFeed returns posts for the public homepage feed, newest-first by
// first-success time. It includes only non-hidden posts in a published state
// (success/partial — the only statuses that can hold a successful target) and
// hydrates each target with platform/status/remote_url/fields_json plus media,
// but NOT attempt/relay history. Reply exclusion and per-platform visibility
// filtering are applied later by feed.Build/feed.Eligible, so this over-fetches
// a bounded window to keep a full page after those drops.
func (s *Store) PublicFeed(limit int) ([]Post, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	fetch := limit * 4
	if fetch < 50 {
		fetch = 50
	}
	if fetch > 200 {
		fetch = 200
	}

	q := `SELECT p.id, p.created_at, p.master_text, p.platforms, p.source, p.status, p.interaction_json,
	             ` + firstSuccessExpr + ` AS first_success_at
	        FROM posts p
	       WHERE p.hidden = 0 AND p.status IN ('success','partial')
	       ORDER BY COALESCE(` + firstSuccessExpr + `, p.created_at) DESC
	       LIMIT ?`

	rows, err := s.sql.Query(q, fetch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Post, 0)
	for rows.Next() {
		var p Post
		var platforms string
		var interactionJSON sql.NullString
		var fsa sql.NullString // MIN() loses TIMESTAMP affinity → returned as text
		if err := rows.Scan(&p.ID, &p.CreatedAt, &p.MasterText, &platforms, &p.Source, &p.Status, &interactionJSON, &fsa); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(platforms), &p.Platforms)
		if interactionJSON.String != "" {
			var ix Interaction
			if json.Unmarshal([]byte(interactionJSON.String), &ix) == nil {
				p.Interaction = &ix
			}
		}
		if fsa.Valid && fsa.String != "" {
			if t, perr := time.Parse("2006-01-02 15:04:05.999999999 -0700 MST", fsa.String); perr == nil {
				u := t.UTC()
				p.FirstSuccessAt = &u
			}
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range out {
		trows, err := s.sql.Query(`SELECT platform, status, remote_url, fields_json FROM post_targets WHERE post_id=? ORDER BY id`, out[i].ID)
		if err != nil {
			return nil, err
		}
		for trows.Next() {
			var tg Target
			var rurl, fields sql.NullString
			if err := trows.Scan(&tg.Platform, &tg.Status, &rurl, &fields); err != nil {
				trows.Close()
				return nil, err
			}
			tg.RemoteURL, tg.FieldsJSON = rurl.String, fields.String
			out[i].Targets = append(out[i].Targets, tg)
		}
		if err := trows.Err(); err != nil {
			trows.Close()
			return nil, err
		}
		trows.Close()

		mrows, err := s.sql.Query(`SELECT ordinal, blossom_url, sha256, mime, dim, blurhash, size_bytes, alt FROM media WHERE post_id=? ORDER BY ordinal`, out[i].ID)
		if err != nil {
			return nil, err
		}
		for mrows.Next() {
			var m Media
			if err := mrows.Scan(&m.Ordinal, &m.BlossomURL, &m.SHA256, &m.Mime, &m.Dim, &m.Blurhash, &m.SizeBytes, &m.Alt); err != nil {
				mrows.Close()
				return nil, err
			}
			out[i].Media = append(out[i].Media, m)
		}
		if err := mrows.Err(); err != nil {
			mrows.Close()
			return nil, err
		}
		mrows.Close()
	}
	return out, nil
}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/store/ -run TestPublicFeed -v`
Expected: PASS (both tests).

- [ ] **Step 6: Commit**

```bash
git add internal/store/models.go internal/store/feed.go internal/store/feed_test.go
git commit -m "feat(store): PublicFeed query + Post.FirstSuccessAt for the public feed"
```

---

## Task 2: Feed package — DTOs, `Eligible`, `Build`

**Files:**
- Create: `internal/feed/feed.go`
- Test: `internal/feed/feed_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/feed/feed_test.go`:

```go
package feed

import (
	"testing"
	"time"

	"github.com/geofox/publisher/internal/store"
)

func successTarget(platform, url, fields string) store.Target {
	return store.Target{Platform: platform, Status: "success", RemoteURL: url, FieldsJSON: fields}
}

func TestBuildIncludesPublicLinksOnly(t *testing.T) {
	posts := []store.Post{{
		ID:         "p1",
		MasterText: "hello",
		FirstSuccessAt: ptr(time.Date(2026, 5, 20, 9, 0, 0, 0, time.UTC)),
		Targets: []store.Target{
			successTarget("nostr", "https://njump.me/x", ""),
			successTarget("mastodon", "https://m/1", `{"visibility":"unlisted"}`), // dropped
			successTarget("bluesky", "https://bsky/1", ""),
			{Platform: "threads", Status: "failed", RemoteURL: ""}, // dropped: not success
		},
	}}
	out := Build(posts, 20)
	if len(out.Posts) != 1 {
		t.Fatalf("got %d items, want 1", len(out.Posts))
	}
	it := out.Posts[0]
	if it.PublishedAt != *posts[0].FirstSuccessAt {
		t.Errorf("PublishedAt = %v, want first-success time", it.PublishedAt)
	}
	gotPlatforms := map[string]bool{}
	for _, l := range it.Links {
		gotPlatforms[l.Platform] = true
	}
	if len(it.Links) != 2 || !gotPlatforms["nostr"] || !gotPlatforms["bluesky"] {
		t.Errorf("links = %+v, want nostr+bluesky only (unlisted mastodon + failed threads dropped)", it.Links)
	}
}

func TestBuildDropsPostWithNoPublicLinks(t *testing.T) {
	posts := []store.Post{{
		ID: "only-unlisted", MasterText: "secret-ish",
		Targets: []store.Target{successTarget("mastodon", "https://m/9", `{"visibility":"private"}`)},
	}}
	if out := Build(posts, 20); len(out.Posts) != 0 {
		t.Fatalf("got %d items, want 0 (no public link → dropped)", len(out.Posts))
	}
}

func TestBuildExcludesRepliesIncludesQuotesReposts(t *testing.T) {
	mk := func(id, action string) store.Post {
		p := store.Post{ID: id, Targets: []store.Target{successTarget("nostr", "https://njump.me/"+id, "")}}
		if action != "" {
			p.Interaction = &store.Interaction{Action: action, SourcePlatform: "bluesky",
				SourceURL: "https://src/" + id, SourceAuthor: "@a"}
		}
		return p
	}
	out := Build([]store.Post{mk("orig", ""), mk("q", "quote"), mk("rp", "repost"), mk("rep", "reply")}, 20)
	got := map[string]bool{}
	for _, it := range out.Posts {
		got[it.ID] = true
	}
	if !got["orig"] || !got["q"] || !got["rp"] || got["rep"] {
		t.Fatalf("included = %v, want orig+q+rp (reply excluded)", got)
	}
	for _, it := range out.Posts {
		if it.ID == "q" {
			if it.Interaction == nil || it.Interaction.Action != "quote" || it.Interaction.SourceAuthor != "@a" {
				t.Errorf("quote interaction not surfaced: %+v", it.Interaction)
			}
		}
		if it.ID == "orig" && it.Interaction != nil {
			t.Errorf("original post should have no interaction block")
		}
	}
}

func TestBuildMapsMediaAndHonorsLimit(t *testing.T) {
	mkP := func(id string) store.Post {
		return store.Post{ID: id, Targets: []store.Target{successTarget("nostr", "https://njump.me/"+id, "")},
			Media: []store.Media{{BlossomURL: "https://b/" + id, Mime: "image/png", Alt: "alt", Dim: "1x1", Blurhash: "L0"}}}
	}
	out := Build([]store.Post{mkP("a"), mkP("b"), mkP("c")}, 2)
	if len(out.Posts) != 2 {
		t.Fatalf("got %d items, want 2 (limit)", len(out.Posts))
	}
	if len(out.Posts[0].Media) != 1 || out.Posts[0].Media[0].URL != "https://b/a" || out.Posts[0].Media[0].Alt != "alt" {
		t.Errorf("media not mapped: %+v", out.Posts[0].Media)
	}
	if out.Version != 1 {
		t.Errorf("Version = %d, want 1", out.Version)
	}
}

func TestEligible(t *testing.T) {
	public := store.Post{Targets: []store.Target{successTarget("nostr", "https://njump.me/x", "")}}
	reply := store.Post{Interaction: &store.Interaction{Action: "reply"},
		Targets: []store.Target{successTarget("nostr", "https://njump.me/x", "")}}
	unlistedOnly := store.Post{Targets: []store.Target{successTarget("mastodon", "https://m/1", `{"visibility":"unlisted"}`)}}
	if !Eligible(public) {
		t.Error("public post should be eligible")
	}
	if Eligible(reply) {
		t.Error("reply should not be eligible")
	}
	if Eligible(unlistedOnly) {
		t.Error("only-unlisted post should not be eligible")
	}
}

func ptr(t time.Time) *time.Time { return &t }
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/feed/ -v`
Expected: FAIL — package/types undefined.

- [ ] **Step 3: Implement the feed core**

Create `internal/feed/feed.go`:

```go
// Package feed builds the public homepage feed and decides which published
// posts are eligible to appear in it. The same Eligible predicate gates the
// publish webhook, so the read API and the webhook can never disagree about
// what is "public".
package feed

import (
	"encoding/json"
	"time"

	"github.com/geofox/publisher/internal/store"
)

type Response struct {
	Version     int       `json:"version"`
	GeneratedAt time.Time `json:"generated_at"`
	Posts       []Item    `json:"posts"`
}

type Item struct {
	ID          string       `json:"id"`
	PublishedAt time.Time    `json:"published_at"`
	Text        string       `json:"text"`
	Media       []MediaItem  `json:"media,omitempty"`
	Interaction *Interaction `json:"interaction,omitempty"`
	Links       []Link       `json:"links"`
}

type MediaItem struct {
	URL      string `json:"url"`
	Mime     string `json:"mime"`
	Alt      string `json:"alt,omitempty"`
	Dim      string `json:"dim,omitempty"`
	Blurhash string `json:"blurhash,omitempty"`
}

type Interaction struct {
	Action         string `json:"action"`
	SourcePlatform string `json:"source_platform"`
	SourceURL      string `json:"source_url"`
	SourceAuthor   string `json:"source_author"`
}

type Link struct {
	Platform string `json:"platform"`
	URL      string `json:"url"`
}

const defaultLimit, maxLimit = 20, 100

func isReply(p store.Post) bool {
	return p.Interaction != nil && p.Interaction.Action == "reply"
}

// publicVisible reports whether a target is publicly visible. Only Mastodon has
// a per-post visibility setting; an unset value means the account default,
// which we treat as public. A malformed fields_json is treated as non-public.
func publicVisible(platform, fieldsJSON string) bool {
	if platform != "mastodon" || fieldsJSON == "" {
		return true
	}
	var f struct {
		Visibility string `json:"visibility"`
	}
	if err := json.Unmarshal([]byte(fieldsJSON), &f); err != nil {
		return false
	}
	return f.Visibility == "" || f.Visibility == "public"
}

// targetLink returns the public link for a target, or false if it should not
// appear: it must have succeeded, carry a URL, and be publicly visible.
func targetLink(t store.Target) (Link, bool) {
	if t.Status != "success" || t.RemoteURL == "" || !publicVisible(t.Platform, t.FieldsJSON) {
		return Link{}, false
	}
	return Link{Platform: t.Platform, URL: t.RemoteURL}, true
}

// Eligible reports whether a post may appear in the feed: it is not a reply and
// has at least one public, successful platform link.
func Eligible(p store.Post) bool {
	if isReply(p) {
		return false
	}
	for _, t := range p.Targets {
		if _, ok := targetLink(t); ok {
			return true
		}
	}
	return false
}

func publishedAt(p store.Post) time.Time {
	if p.FirstSuccessAt != nil {
		return *p.FirstSuccessAt
	}
	return p.CreatedAt
}

// Build reshapes hydrated store posts into the feed response, applying the
// link filter, dropping ineligible/link-less posts, and trimming to limit.
func Build(posts []store.Post, limit int) Response {
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	out := Response{Version: 1, GeneratedAt: time.Now().UTC(), Posts: []Item{}}
	for _, p := range posts {
		if len(out.Posts) >= limit {
			break
		}
		if isReply(p) {
			continue
		}
		links := make([]Link, 0, len(p.Targets))
		for _, t := range p.Targets {
			if l, ok := targetLink(t); ok {
				links = append(links, l)
			}
		}
		if len(links) == 0 {
			continue
		}
		item := Item{
			ID:          p.ID,
			PublishedAt: publishedAt(p),
			Text:        p.MasterText,
			Links:       links,
			Media:       mediaItems(p.Media),
		}
		if p.Interaction != nil {
			item.Interaction = &Interaction{
				Action:         p.Interaction.Action,
				SourcePlatform: p.Interaction.SourcePlatform,
				SourceURL:      p.Interaction.SourceURL,
				SourceAuthor:   p.Interaction.SourceAuthor,
			}
		}
		out.Posts = append(out.Posts, item)
	}
	return out
}

func mediaItems(ms []store.Media) []MediaItem {
	if len(ms) == 0 {
		return nil
	}
	out := make([]MediaItem, 0, len(ms))
	for _, m := range ms {
		out = append(out, MediaItem{
			URL: m.BlossomURL, Mime: m.Mime, Alt: m.Alt, Dim: m.Dim, Blurhash: m.Blurhash,
		})
	}
	return out
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/feed/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/feed/feed.go internal/feed/feed_test.go
git commit -m "feat(feed): DTOs, Eligible predicate, and Build reshaper"
```

---

## Task 3: Feed package — outbound `Webhook`

**Files:**
- Create: `internal/feed/webhook.go`
- Test: `internal/feed/webhook_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/feed/webhook_test.go`:

```go
package feed

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/store"
)

func eligiblePost(id string) *store.Post {
	return &store.Post{ID: id, MasterText: "hi",
		Targets: []store.Target{{Platform: "nostr", Status: "success", RemoteURL: "https://njump.me/" + id}}}
}

func TestWebhookFiresForEligiblePost(t *testing.T) {
	type got struct {
		auth string
		body map[string]string
	}
	ch := make(chan got, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]string
		_ = json.Unmarshal(b, &m)
		ch <- got{auth: r.Header.Get("Authorization"), body: m}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	wh := NewWebhook(srv.URL, "tok")
	wh.PostPublished(context.Background(), eligiblePost("p1"))

	select {
	case g := <-ch:
		if g.auth != "Bearer tok" {
			t.Errorf("auth = %q, want Bearer tok", g.auth)
		}
		if g.body["event"] != "post.published" || g.body["id"] != "p1" {
			t.Errorf("body = %+v, want event=post.published id=p1", g.body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("webhook did not fire within 2s")
	}
}

func TestWebhookSilentForIneligibleOrUnconfigured(t *testing.T) {
	fired := make(chan struct{}, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fired <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Configured, but the post is a reply → must not fire.
	wh := NewWebhook(srv.URL, "")
	reply := eligiblePost("r1")
	reply.Interaction = &store.Interaction{Action: "reply"}
	wh.PostPublished(context.Background(), reply)

	// Unconfigured URL → must not fire even for an eligible post.
	NewWebhook("", "").PostPublished(context.Background(), eligiblePost("p2"))

	select {
	case <-fired:
		t.Fatal("webhook fired for an ineligible/unconfigured case")
	case <-time.After(300 * time.Millisecond):
		// success: nothing fired
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/feed/ -run TestWebhook -v`
Expected: FAIL — `NewWebhook` / `PostPublished` undefined.

- [ ] **Step 3: Implement the webhook**

Create `internal/feed/webhook.go`:

```go
package feed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/geofox/publisher/internal/store"
)

// Webhook POSTs a signal-only ping to an external consumer (the homepage build)
// when a feed-eligible post is published. The payload carries no post content;
// the receiver re-fetches GET /api/public/feed. Delivery is best-effort and
// never blocks the caller. An empty URL makes it a no-op.
type Webhook struct {
	URL   string
	Token string
	HTTP  *http.Client
}

func NewWebhook(url, token string) *Webhook {
	return &Webhook{URL: url, Token: token, HTTP: &http.Client{Timeout: 10 * time.Second}}
}

// PostPublished pings the consumer if the post is feed-eligible. It returns
// immediately; delivery (with retries) runs in a background goroutine.
func (w *Webhook) PostPublished(_ context.Context, p *store.Post) {
	if w == nil || w.URL == "" || p == nil || !Eligible(*p) {
		return
	}
	body, err := json.Marshal(map[string]string{
		"event":        "post.published",
		"id":           p.ID,
		"published_at": publishedAt(*p).Format(time.RFC3339),
	})
	if err != nil {
		return
	}
	go w.deliver(body, p.ID)
}

// deliver POSTs the payload with up to 3 attempts and a short linear backoff.
// It uses a detached context so the originating request finishing cannot cancel
// the ping. On exhaustion it logs a warning — a missed ping just leaves the
// homepage stale until the next post, never wrong.
func (w *Webhook) deliver(body []byte, postID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
		if err != nil {
			lastErr = err
			break
		}
		req.Header.Set("Content-Type", "application/json")
		if w.Token != "" {
			req.Header.Set("Authorization", "Bearer "+w.Token)
		}
		resp, err := w.HTTP.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode/100 == 2 {
				return
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		if attempt < 3 {
			time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
		}
	}
	slog.Warn("feed webhook delivery failed", "post_id", postID, "err", lastErr)
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/feed/ -run TestWebhook -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/feed/webhook.go internal/feed/webhook_test.go
git commit -m "feat(feed): signal-only publish webhook with eligibility gate"
```

---

## Task 4: Dispatch — `PostNotifier` hook at terminal publishes

**Files:**
- Modify: `internal/dispatch/dispatch.go:146-153` (struct), and the returns of
  `Post` (`:533`), `Interact` (`:702`), `Fire` (`:832`), `Retry` (`:859`)
- Test: `internal/dispatch/notify_test.go`

The dispatcher calls the notifier **unconditionally** at every terminal publish;
the eligibility gate lives in `feed.Webhook`, not here. So these tests only
assert the hook is invoked with the published post.

- [ ] **Step 1: Write the failing test**

Create `internal/dispatch/notify_test.go`:

```go
package dispatch

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/geofox/publisher/internal/store"
)

type recordingNotifier struct{ posts []*store.Post }

func (r *recordingNotifier) PostPublished(_ context.Context, p *store.Post) {
	r.posts = append(r.posts, p)
}

func TestPostNotifiesOnImmediatePublish(t *testing.T) {
	rn := &recordingNotifier{}
	d := &Dispatcher{Nostr: fakeNostr{}, Notify: rn}
	rec := d.Post(context.Background(), PostSpec{MasterText: "hi", Platforms: []string{"nostr"}, Source: "web"})
	if len(rn.posts) != 1 || rn.posts[0].ID != rec.ID {
		t.Fatalf("PostPublished calls = %d, want 1 with id %s", len(rn.posts), rec.ID)
	}
}

func TestInteractNotifies(t *testing.T) {
	rn := &recordingNotifier{}
	d := &Dispatcher{Nostr: fakeNostr{}, Notify: rn}
	rec := d.Interact(context.Background(), InteractSpec{
		Action: "quote", SourcePlatform: "nostr", Text: "nice",
		Ref: InteractRef{EventID: "e1", Author: "a1"},
	})
	if len(rn.posts) != 1 || rn.posts[0].ID != rec.ID {
		t.Fatalf("Interact PostPublished calls = %d, want 1 with id %s", len(rn.posts), rec.ID)
	}
}

func TestRetryNotifies(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// A post whose nostr target failed, so Retry will re-run it.
	if err := db.SavePost(&store.Post{
		ID: "p1", Platforms: []string{"nostr"}, Status: "failed",
		Targets: []store.Target{{Platform: "nostr", Status: "failed",
			Attempts: []store.Attempt{{AttemptNo: 1, Status: "failed"}}}},
	}); err != nil {
		t.Fatal(err)
	}
	rn := &recordingNotifier{}
	d := &Dispatcher{Nostr: fakeNostr{}, Store: db, Notify: rn}
	if _, err := d.Retry(context.Background(), "p1", nil); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if len(rn.posts) != 1 || rn.posts[0].ID != "p1" {
		t.Fatalf("Retry PostPublished calls = %d, want 1 with id p1", len(rn.posts))
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/dispatch/ -run 'Notifies|RetryNotifies' -v`
Expected: FAIL — `Dispatcher.Notify` undefined.

- [ ] **Step 3: Add the interface, field, and helper**

In `internal/dispatch/dispatch.go`, add the interface and field. Put the
interface just above the `Dispatcher` struct (line 146):

```go
// PostNotifier is told when a post reaches a terminal publish state, so an
// implementation (feed.Webhook) can ping an external consumer. Implementations
// must be non-blocking and best-effort — the dispatcher fires this on the hot
// publish path and does not wait for or check the result.
type PostNotifier interface {
	PostPublished(ctx context.Context, p *store.Post)
}

type Dispatcher struct {
	Nostr    NostrPoster
	Mastodon MastodonPoster
	Bluesky  BlueskyPoster
	Threads  ThreadsPoster
	Store    *store.Store // may be nil in unit tests
	Fetcher  Fetcher
	Notify   PostNotifier // may be nil; notify() guards it
}
```

Then add the helper (place it right after the struct, before `runPlatform`):

```go
// notify fires the PostNotifier when configured. Safe with a nil notifier or
// nil post so call sites stay one-liners.
func (d *Dispatcher) notify(ctx context.Context, p *store.Post) {
	if d.Notify != nil && p != nil {
		d.Notify.PostPublished(ctx, p)
	}
}
```

- [ ] **Step 4: Add the four call sites**

In `Post`, replace the final `return rec` (line 533):

```go
	d.notify(ctx, rec)
	return rec
```

In `Interact`, replace the final `return rec` (line 702):

```go
	d.notify(ctx, rec)
	return rec
```

In `Fire`, replace `return d.Store.GetPost(postID)` (line 832):

```go
	post, err = d.Store.GetPost(postID)
	if err != nil {
		return nil, err
	}
	d.notify(ctx, post)
	return post, nil
```

In `Retry`, replace `return d.Store.GetPost(postID)` (line 859):

```go
	post, err = d.Store.GetPost(postID)
	if err != nil {
		return nil, err
	}
	d.notify(ctx, post)
	return post, nil
```

Note: in `Fire`/`Retry` the local `post` and `err` already exist (declared
earlier in each function via `post, err := d.Store.GetPost(postID)`), so reuse
`=` not `:=` as shown.

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/dispatch/ -run 'Notifies|RetryNotifies' -v`
Expected: PASS. Also run the full dispatch suite to catch regressions:
`go test ./internal/dispatch/ -v`

- [ ] **Step 6: Commit**

```bash
git add internal/dispatch/dispatch.go internal/dispatch/notify_test.go
git commit -m "feat(dispatch): PostNotifier hook fired on publish/interact/fire/retry"
```

---

## Task 5: API — token-gated `GET /api/public/feed`

**Files:**
- Modify: `internal/api/api.go` (imports, `API` struct ~line 90, `Routes` line 117, new handler)
- Test: `internal/api/public_feed_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/api/public_feed_test.go`:

```go
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/store"
)

func feedTestStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, 5, 20, 9, 0, 0, 0, time.UTC)
	if err := db.SavePost(&store.Post{
		ID: "p1", Platforms: []string{"nostr"}, Status: "success", MasterText: "hello",
		Targets: []store.Target{{Platform: "nostr", Status: "success", RemoteURL: "https://njump.me/x",
			Attempts: []store.Attempt{{AttemptNo: 1, Status: "success", AttemptedAt: ts}}}},
	}); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestPublicFeedDisabledWhenNoToken(t *testing.T) {
	a := &API{Store: feedTestStore(t)} // PublicFeedToken == ""
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/public/feed", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404 when token unset", rec.Code)
	}
}

func TestPublicFeedRejectsBadToken(t *testing.T) {
	a := &API{Store: feedTestStore(t), PublicFeedToken: "secret"}
	for _, h := range []string{"", "Bearer wrong", "secret"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/public/feed", nil)
		if h != "" {
			req.Header.Set("Authorization", h)
		}
		a.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("auth %q: code = %d, want 401", h, rec.Code)
		}
	}
}

func TestPublicFeedReturnsItems(t *testing.T) {
	a := &API{Store: feedTestStore(t), PublicFeedToken: "secret"}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/public/feed", nil)
	req.Header.Set("Authorization", "Bearer secret")
	a.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Version int `json:"version"`
		Posts   []struct {
			ID    string `json:"id"`
			Text  string `json:"text"`
			Links []struct {
				Platform string `json:"platform"`
				URL      string `json:"url"`
			} `json:"links"`
		} `json:"posts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Version != 1 || len(resp.Posts) != 1 || resp.Posts[0].ID != "p1" {
		t.Fatalf("resp = %+v, want version 1 + one post p1", resp)
	}
	if len(resp.Posts[0].Links) != 1 || resp.Posts[0].Links[0].URL != "https://njump.me/x" {
		t.Fatalf("links = %+v, want one nostr njump link", resp.Posts[0].Links)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/api/ -run TestPublicFeed -v`
Expected: FAIL — `API.PublicFeedToken` undefined / route 404 for all cases.

- [ ] **Step 3: Add imports, struct field, route, and handler**

In `internal/api/api.go`:

Add to the import block (line 3-31) — `crypto/subtle` and the feed package:

```go
	"crypto/subtle"
```

```go
	"github.com/geofox/publisher/internal/feed"
```

Add a field to the `API` struct (after `Translator`, line 107):

```go
	// PublicFeedToken gates GET /api/public/feed. Empty → the endpoint is
	// disabled (returns 404). Set → callers must send Authorization: Bearer <it>.
	PublicFeedToken string
```

Register the route in `Routes()` (after the `GET /api/config` line, ~line 140):

```go
	mux.HandleFunc("GET /api/public/feed", a.handlePublicFeed)
```

Add the handler (place it next to `handleListPosts`, after line 630):

```go
// ─── GET /api/public/feed ────────────────────────────────────────────────
//
// Read-only homepage feed: latest public master posts as custom JSON. Disabled
// (404) unless PUBLIC_FEED_TOKEN is set; when set, requires a matching bearer
// token. GET, so it passes the CSRF guard and is meant for a server-side
// (build-time) consumer that keeps the token secret.
func (a *API) handlePublicFeed(w http.ResponseWriter, r *http.Request) {
	if a.PublicFeedToken == "" {
		httpx.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) ||
		subtle.ConstantTimeCompare([]byte(h[len(prefix):]), []byte(a.PublicFeedToken)) != 1 {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	limit := atoiOr(r.URL.Query().Get("limit"), 20)
	posts, err := a.Store.PublicFeed(limit)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, feed.Build(posts, limit))
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/api/ -run TestPublicFeed -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/api.go internal/api/public_feed_test.go
git commit -m "feat(api): token-gated GET /api/public/feed"
```

---

## Task 6: Config — three new env vars

**Files:**
- Modify: `internal/config/config.go` (struct ~line 56-61, `Load` ~line 87-88)
- Test: `internal/config/config_test.go` (create if absent)

- [ ] **Step 1: Write the failing test**

Create `internal/config/config_test.go` (or append if it exists):

```go
package config

import "testing"

func TestFeedEnvVars(t *testing.T) {
	if got := getEnv("PUBLIC_FEED_TOKEN", ""); got != "" {
		t.Errorf("default PUBLIC_FEED_TOKEN = %q, want empty", got)
	}
	t.Setenv("PUBLIC_FEED_TOKEN", "tok")
	t.Setenv("FEED_WEBHOOK_URL", "https://hook")
	t.Setenv("FEED_WEBHOOK_TOKEN", "wtok")
	if getEnv("PUBLIC_FEED_TOKEN", "") != "tok" ||
		getEnv("FEED_WEBHOOK_URL", "") != "https://hook" ||
		getEnv("FEED_WEBHOOK_TOKEN", "") != "wtok" {
		t.Error("feed env vars not read via getEnv")
	}
}
```

(`config.Load()` requires NSEC/Blossom and is awkward to unit-test in full; this
test pins the env-var wiring through the same `getEnv` helper `Load` uses.)

- [ ] **Step 2: Run the test to verify it fails (compile) / passes the helper**

Run: `go test ./internal/config/ -run TestFeedEnvVars -v`
Expected: PASS for the helper assertions (it exercises `getEnv`). If it does not
compile, the package is broken — fix before continuing.

- [ ] **Step 3: Add the config fields and load them**

In `internal/config/config.go`, add fields to the `Config` struct (after
`DeepLAPIKey`, line 61):

```go
	// PublicFeedToken gates GET /api/public/feed (empty → endpoint disabled).
	PublicFeedToken string
	// FeedWebhookURL: signal-only ping POSTed when a feed-eligible post is
	// published (empty → no webhook). FeedWebhookToken is sent as a bearer token.
	FeedWebhookURL   string
	FeedWebhookToken string
```

Load them in `Load()` (after the `DeepLAPIKey` line, line 88):

```go
	c.PublicFeedToken = getEnv("PUBLIC_FEED_TOKEN", "")
	c.FeedWebhookURL = getEnv("FEED_WEBHOOK_URL", "")
	c.FeedWebhookToken = getEnv("FEED_WEBHOOK_TOKEN", "")
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/config/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): PUBLIC_FEED_TOKEN, FEED_WEBHOOK_URL, FEED_WEBHOOK_TOKEN"
```

---

## Task 7: Wire everything in `cmd/publisher/main.go`

**Files:**
- Modify: `cmd/publisher/main.go` (imports, dispatcher construction line 68-75, API setup line 76-82)

This task has no new unit test; it is verified by `go build ./...` and the full
test suite. The dispatcher's `Notify` and the API's `PublicFeedToken` are
plumbed from config.

- [ ] **Step 1: Add the feed import**

In `cmd/publisher/main.go` import block (line 15-29), add:

```go
	"github.com/geofox/publisher/internal/feed"
```

- [ ] **Step 2: Inject the notifier into the dispatcher**

Change the `Dispatcher` literal (line 68-75) to set `Notify`:

```go
	d := &dispatch.Dispatcher{
		Nostr:    dispatch.NostrAdapter{P: np},
		Mastodon: dispatch.MastodonAdapter{C: mc},
		Bluesky:  dispatch.BlueskyAdapter{C: bc},
		Threads:  dispatch.ThreadsAdapter{C: tc},
		Store:    st,
		Fetcher:  mp,
		Notify:   feed.NewWebhook(cfg.FeedWebhookURL, cfg.FeedWebhookToken),
	}
```

(`feed.NewWebhook` with an empty URL is a safe no-op, so this is unconditional.)

- [ ] **Step 3: Set the API feed token**

After `a.UserLanguages = cfg.UserLanguages` (line 79), add:

```go
	a.PublicFeedToken = cfg.PublicFeedToken
```

- [ ] **Step 4: Verify build + full suite**

Run: `go build ./... && go test ./...`
Expected: build succeeds; all tests PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/publisher/main.go
git commit -m "feat: wire public feed token + publish webhook into main"
```

---

## Task 8: Document env vars + endpoint in README

**Files:**
- Modify: `README.md` (env table ~line 313-315, API endpoints section ~line 216)

- [ ] **Step 1: Add env-var rows**

In `README.md`, after the `ALERT_WEBHOOK_PASS` row (line 315), add:

```markdown
| `PUBLIC_FEED_TOKEN` | no | — | Bearer token gating `GET /api/public/feed`; unset disables the endpoint (**secret**) |
| `FEED_WEBHOOK_URL` | no | — | Signal-only webhook POSTed when a feed-eligible post is published |
| `FEED_WEBHOOK_TOKEN` | no | — | Bearer token sent on the feed webhook so the receiver can verify it (**secret**) |
```

- [ ] **Step 2: Document the endpoint**

In the API endpoints section (near the drafts endpoints, after line 219), add a
short subsection:

```markdown
### Public feed

- `GET /api/public/feed?limit=20` — latest public master posts as JSON for a
  homepage. Requires `Authorization: Bearer $PUBLIC_FEED_TOKEN`; returns `404`
  when `PUBLIC_FEED_TOKEN` is unset. Each item has `id`, `published_at` (first
  time the post went live on any platform), `text`, optional `media[]`, optional
  `interaction` (for quotes/reposts), and `links[]` — one `{platform, url}` per
  platform where the post is public and successfully published. Replies, and any
  post with no public platform copy, are omitted. `limit` defaults to 20, max
  100.

When a feed-eligible post is published (immediately, on a scheduled fire, or on
a retry), publisher fires a signal-only `POST` to `FEED_WEBHOOK_URL`
(`{ "event": "post.published", "id", "published_at" }`) so the homepage can
re-fetch instead of polling. The body carries no content; treat it as a refresh
trigger.
```

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs: document public feed endpoint + env vars"
```

---

## Final verification

- [ ] **Run the whole suite + build:**

Run: `go build ./... && go test ./...`
Expected: all packages build and pass.

- [ ] **Manual smoke test (optional but recommended):**

```bash
PUBLIC_FEED_TOKEN=devtoken DB_PATH=/tmp/pub.db \
  NSEC_HEX=... OWNER_PUBKEY=... BLOSSOM_URL=... \
  go run ./cmd/publisher &
curl -s localhost:8080/api/public/feed                       # → 404 body {"error":"not found"} only if token unset
curl -s -H 'Authorization: Bearer devtoken' localhost:8080/api/public/feed | jq .
```

Expected: with the token set, a JSON object `{version, generated_at, posts}`;
without the header, `401`.

## Release note

This ships as **v0.9.0** (new public API surface). Tag + release-note commit per
the repo convention, and add `PUBLIC_FEED_TOKEN` / `FEED_WEBHOOK_URL` /
`FEED_WEBHOOK_TOKEN` to the Oppy deploy environment (both features stay off until
their env vars are set).
