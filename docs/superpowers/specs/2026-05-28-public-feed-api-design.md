# Public Feed API + Publish Webhook — Design

**Status:** approved design, ready for implementation planning.
**Branch:** TBD (suggest `feature/public-feed`).
**Date:** 2026-05-28.

## Goal

Let the owner's personal homepage surface what they publish through Publisher.
Two cooperating pieces:

1. **Public read API** — a single authenticated, read-only endpoint returning the
   latest master posts as custom JSON: full content (text + media), and a link
   per platform where the post is publicly visible. The homepage renders a real
   microblog feed and links out to each service.
2. **Publish webhook** — a signal-only outbound POST fired when a post is
   successfully published (immediate, scheduled fire, or retry) *and* would
   appear in the feed. The homepage uses it to re-fetch instead of polling; it
   carries no post data.

The two share one eligibility predicate so they can never disagree about what is
"public."

## Approved decisions

1. **Payload = full content + links.** Each feed item carries `text`
   (canonical `master_text`), `media[]` (Blossom URL + mime + alt + dim +
   blurhash), `published_at`, optional `interaction`, and `links[]` (one
   `{platform, url}` per publicly-visible successful target).
2. **Privacy = per-platform link filter, then empty-drop.** A target becomes a
   link only if it is `status=success`, has a non-empty `remote_url`, and is
   publicly visible. Visibility applies only to Mastodon (`fields_json.visibility`
   must be `public`, or unset → account default which resolves to public);
   `unlisted`/`private`/`direct` are omitted. Bluesky, Nostr, Threads have no
   per-post visibility and always pass. After filtering, a post with zero links
   is dropped entirely. (This is the user's rule: hide just that platform, but if
   no public copy remains, the whole post disappears.)
3. **Post types: originals, quotes, reposts — not replies.** Replies
   (`interaction.action == "reply"`) never appear. Quotes and reposts appear and
   carry an `interaction` block exposing `action`, `source_platform`,
   `source_url`, `source_author`. A bare repost may have empty `text`.
4. **Every platform already has a web URL.** `Target.RemoteURL` is populated for
   all four platforms, including Nostr (`https://njump.me/<nevent>`, set in
   `dispatch/adapters.go:58-59`). The feed reads it directly; no URL construction.
5. **Format = custom JSON**, wrapped in an object (`{version, generated_at,
   posts[]}`) so metadata/pagination can be added later without breaking
   consumers.
6. **Auth = optional static bearer token.** Env `PUBLIC_FEED_TOKEN`. Unset →
   endpoint returns 404 (feature off, nothing exposed by default). Set →
   requires `Authorization: Bearer <token>`, compared with
   `crypto/subtle.ConstantTimeCompare`; missing/wrong → 401. Consumed
   server-side (build-time) so the token stays secret.
7. **Webhook is signal-only and best-effort.** Fired async (never blocks/fails a
   publish), context timeout + a couple of retries with backoff, warning logged
   on exhaustion. A missed webhook degrades to "stale until next post," not wrong
   data — so no guaranteed-delivery machinery.
8. **Webhook eligibility gate = the feed predicate.** Fires only for posts that
   would appear in the feed (not replies, ≥1 public successful link). Same
   `feed.Eligible(post)` used by the read endpoint.
9. **Webhook fires on retry too.** Treat retry as a terminal publish: if the
   post is feed-eligible afterward, ping. Redundant pings are harmless because
   the receiver just re-fetches (idempotent).
10. **`published_at` = first-success time, stable across retries.** The item's
    publish date is the earliest moment the post went live on *any* platform
    (`MIN(attempted_at)` over successful attempts). A later retry that fixes a
    failed platform appends a newer attempt row, so it adds a link but does
    **not** move the publish date. The feed is ordered by this same timestamp.

## Out of scope

- Pagination beyond a `limit` (no cursor/offset in v1).
- JSON Feed / RSS / Atom output (custom JSON only).
- Per-platform content in the feed (we expose canonical `master_text`, not each
  platform's truncated/threaded variant).
- Guaranteed webhook delivery (queue, dead-letter, persistence).
- HMAC request signing (a shared bearer token is sufficient).
- Exposing thread structure (we link to the chain head only).

## Architecture

### Shared feed package — `internal/feed`

New package, single source of truth for feed semantics. Imports `internal/store`
only (no cycles: `api` and `dispatch` import `feed`; nothing `feed` needs imports
them).

- DTO types: `Response{Version int, GeneratedAt time.Time, Posts []Item}`,
  `Item`, `MediaItem`, `Link`, `Interaction`.
- `Eligible(p store.Post) bool` — the inclusion predicate: not a reply, and at
  least one target passes the public+success+has-url test. Operates on a post
  hydrated with target `status`, `remote_url`, `fields_json`.
- `Build(posts []store.Post, limit int) Response` — applies the per-target link
  filter, empty-drop, trims to `limit`, reshapes to DTOs. Each item's
  `published_at` is `p.FirstSuccessAt` (the first-success time set by
  `PublicFeed`), falling back to `p.CreatedAt` only if unset. Pure and table-test
  friendly.
- `publicVisible(platform, fieldsJSON string) bool` — the Mastodon-visibility
  helper (unset/`public` → true; otherwise false; non-Mastodon → true).

### Data access — `internal/store`

New method (new file `internal/store/feed.go` or appended to `models.go`):

- `PublicFeed(limit int) ([]Post, error)` — posts ordered by **first-success
  time** descending (`MIN(ta.attempted_at)` over the post's attempts where
  `ta.status='success'`, `COALESCE`-d with `created_at` defensively), filtered to
  `hidden=0` and `status IN ('success','partial')` (the only statuses that can
  hold a successful target). Hydrates each target with `platform`, `status`,
  `remote_url`, `fields_json`, plus the post's `media[]` and `interaction`, and
  populates the new `FirstSuccessAt` field (see below) with that same
  first-success timestamp. Does **not** hydrate attempt/relay history (unlike
  `GetPost`). Reply exclusion and visibility filtering are left to
  `feed.Eligible`/`feed.Build` so the predicate stays the single source of truth;
  the bounded over-fetch window absorbs the rows those rules drop.
- `store.Post` gains `FirstSuccessAt *time.Time` with `json:"-"` (never
  serialized, so it can't leak into the existing `/api/posts` responses;
  populated only by `PublicFeed`, read only by `feed.Build`). It is the time the
  post first went live on **any** platform; because retries append *later*
  attempt rows, `MIN` is unaffected, so a successful retry never moves the
  publish date.
- Because the visibility/empty-drop filtering happens in `feed.Build` after the
  query, `PublicFeed` over-fetches a bounded recent window (e.g. `max(limit*4,
  50)` capped at 200) so trimming to `limit` still yields a full page in the
  common case. Worst case returns slightly fewer than `limit` — acceptable.

### Read endpoint — `internal/api/api.go`

- Register `mux.HandleFunc("GET /api/public/feed", a.handlePublicFeed)`.
- New `API` field `PublicFeedToken string` (set from config in
  `cmd/publisher/main.go`).
- `handlePublicFeed`: if `PublicFeedToken == ""` → 404. Else require
  `Authorization: Bearer <token>` (constant-time compare) → 401 on mismatch.
  Parse `limit` (default 20, cap 100). Call `store.PublicFeed`, then
  `feed.Build`, then `httpx.WriteJSON`.
- GET passes the existing `withCSRFGuard` (safe method) and inherits
  `withSecurityHeaders`.

### Webhook — `internal/feed` (sender) + dispatcher injection

- `Notifier` interface (defined where the dispatcher consumes it):
  `PostPublished(ctx context.Context, p *store.Post)`. Injected into
  `dispatch.Dispatcher` so dispatch stays decoupled and tests use a fake.
- Concrete `feed.Webhook` implements `Notifier`: holds the URL, optional token,
  and an `*http.Client`. `PostPublished` runs `feed.Eligible(p)`; if eligible and
  a URL is configured, fires the POST in a goroutine with timeout + small
  retry/backoff; logs a warning on failure. No-op when URL unset. A no-op
  notifier (also implementing `Notifier`) is injected when `FEED_WEBHOOK_URL` is
  unset, so dispatch never branches on nil.
- Call sites in `internal/dispatch`: the four publish methods do **not** share a
  single convergence point, so each gets an explicit
  `d.Notify.PostPublished(ctx, post)` just before it returns its `*store.Post`:
  `Post` (immediate), `Interact` (quote/repost; replies are dropped by the
  `Eligible` gate, not here), `Fire` (scheduled), and `Retry`. `RetryRelay` is a
  narrow Nostr-relay-only retry — covering it is optional and lower priority.
- Payload: `{"event":"post.published","id":"<id>","published_at":"<rfc3339>"}`
  with `Authorization: Bearer <FEED_WEBHOOK_TOKEN>` when configured.
- *Verify during implementation:* the in-memory `*store.Post` returned by each
  publish method carries target `fields_json` + `remote_url` so `Eligible` is
  accurate. If any path lacks them, the notifier re-reads via
  `store.GetPost(p.ID)` before evaluating.

### Config — `internal/config/config.go`

Three new env vars loaded in `Load()`:
- `PublicFeedToken` ← `PUBLIC_FEED_TOKEN` (default `""`).
- `FeedWebhookURL` ← `FEED_WEBHOOK_URL` (default `""`).
- `FeedWebhookToken` ← `FEED_WEBHOOK_TOKEN` (default `""`).

`cmd/publisher/main.go` wires the token into `API` and constructs the
`feed.Webhook` notifier (or a no-op notifier when `FEED_WEBHOOK_URL` is unset)
to inject into the dispatcher.

## API contract

`GET /api/public/feed?limit=20`

```json
{
  "version": 1,
  "generated_at": "2026-05-28T10:00:00Z",
  "posts": [
    {
      "id": "a1b2c3",
      "published_at": "2026-05-27T18:42:00Z",
      "text": "The full master text of the post…",
      "media": [
        { "url": "https://blossom.example/abc.jpg", "mime": "image/jpeg",
          "alt": "a cat on a keyboard", "dim": "1200x800", "blurhash": "L6Pj0^..." }
      ],
      "interaction": {
        "action": "quote",
        "source_platform": "bluesky",
        "source_url": "https://bsky.app/profile/x/post/y",
        "source_author": "@someone"
      },
      "links": [
        { "platform": "bluesky",  "url": "https://bsky.app/profile/you/post/abc" },
        { "platform": "nostr",    "url": "https://njump.me/nevent1…" },
        { "platform": "mastodon", "url": "https://mastodon.social/@you/123" }
      ]
    }
  ]
}
```

- `interaction` omitted for original posts; present for quotes/reposts.
- `media` omitted/empty when the post has no attachments.
- `links` always has ≥1 entry (else the post is dropped).
- Responses: `200` (feed), `401` (token configured, bad/missing bearer),
  `404` (feature disabled), `500` (store error).

## Testing

- **`store.PublicFeed`:** ordering by first-success time descending; hydrates
  `remote_url` + `fields_json` + `FirstSuccessAt`; excludes hidden; bounded
  over-fetch. A post that succeeded on platform A at T1 and only succeeded on
  platform B after a retry at T2>T1 reports `FirstSuccessAt == T1` (retry does
  not move the date) and now carries both links.
- **`feed.Build` / `feed.Eligible` (pure, table-driven):** Mastodon `unlisted`
  link omitted; post public-elsewhere keeps its other links; only-unlisted post
  dropped; quote/repost carry `interaction`; reply excluded; media mapped;
  `limit` honored and capped.
- **`handlePublicFeed`:** token unset → 404; token set + no/bad bearer → 401;
  good bearer → 200 + expected shape.
- **Webhook:** fake `Notifier` asserts dispatch calls it on
  Post/Interact/Fire/Retry; `feed.Webhook` unit test asserts it fires for an
  eligible post, stays silent for a reply and an only-unlisted-Mastodon post,
  sends the bearer header and minimal payload, and never blocks the caller.

## Release

- Ships as **v0.9.0** (new public API surface), following the repo's tag +
  release-note-commit convention.
- Add `PUBLIC_FEED_TOKEN`, `FEED_WEBHOOK_URL`, `FEED_WEBHOOK_TOKEN` to the Oppy
  deploy environment (per the deploy-to-Oppy procedure). The feed and webhook
  are both off until their env vars are set.
