# Quote / Reply / Repost (inbound interactions) — Design

**Status:** approved design, ready for implementation planning.
**Branch:** `feature/quote-reply`.
**Date:** 2026-05-25.

## Goal

Let the owner act on an *existing* post from another account: **reply** to it,
**repost** it, or **quote** it (with commentary) — by pasting the post's URL
(or, for Nostr, an identifier). Publisher resolves the source post, previews it,
detects what interactions the post allows, and dispatches the chosen action.

This is the inbound counterpart to the existing outbound cross-poster: where
`dispatch` *posts*, the new `resolve` layer *reads* an external post and feeds it
into the same posting machinery.

## Approved decisions

1. **Reach model — quote can fan out.** Reply and repost are *source-platform
   only* (they have no meaning elsewhere). **Quote** does a native quote on the
   source platform **and**, optionally, a *link-quote* (your commentary + the
   source's web URL) on your other selected platforms.
2. **Restrictions — detect & warn, allow override.** Pre-check whether the post
   permits the action; if not, show the reason and disable the action by default
   but let the user force it ("try anyway"). Surfacing the reason matters because
   **Bluesky silently drops** a disallowed reply/quote (no error).
3. **Mastodon quote — native where possible, else link.** Probe the instance
   (native quote needs server 4.5+ / API v7) and read `quote_approval`; use a
   true native quote when allowed, fall back to commentary + URL otherwise.
4. **UI — a new "Interact" tab.** Smart input → source preview card → action row
   (Reply · Repost · Quote). Reply & Quote reuse the existing compose editor,
   live preview, and scheduling; Quote adds fan-out platform toggles.

## Scope & platform matrix (v1)

| Source platform | Detected from | Reply | Repost | Quote (native) | Fan-out link-quote target |
|---|---|---|---|---|---|
| **Bluesky** | host `bsky.app` | ✅ | ✅ `app.bsky.feed.repost` | ✅ `app.bsky.embed.record` | ✅ |
| **Mastodon** | any other URL | ✅ `in_reply_to_id` | ✅ `/reblog` | ✅ native 4.5+ / link | ✅ |
| **Nostr** | `nevent`/`note`/`naddr`/`nostr:`/64-hex | ✅ NIP-10 | ✅ NIP-18 kind 6/16 | ✅ NIP-18 `q`-tag + mention | ✅ via `njump.me` URL |
| **Threads** | host `threads.net` / `threads.com` | ❌ (no source) | ❌ | ❌ | ✅ link only (normal post + URL) |

**Threads is not a source.** The official Threads API has no way to turn a public
post URL/shortcode into the `threads-media-id` that reply/quote/repost require,
and reading arbitrary posts needs Meta "Advanced Access." Detect a Threads URL
and explain it can't be acted on. Threads can still *receive* a fan-out
link-quote, since that is just a normal post containing the source URL.

**Platform detection heuristic** (in `resolve.Resolve`): host `bsky.app` →
Bluesky; host `threads.net`/`threads.com` → Threads (unsupported source);
input matching `^(nostr:)?(nevent|note|naddr)1[0-9a-z]+$` or a bare 64-char hex →
Nostr; **any other http(s) URL → Mastodon** (Mastodon instances live on arbitrary
domains, so it is the fallthrough, resolved through the configured instance's
search).

## Architecture

### New package `internal/resolve`

The inbound analogue of `internal/dispatch`. Pure orchestration over the existing
platform clients (which gain *read* methods); no new long-lived state.

```go
// SourceRef is a resolved external post: its platform-native handle, a preview,
// and what the owner is allowed to do with it.
type SourceRef struct {
    Platform string        // "bluesky" | "mastodon" | "nostr" | "threads"
    Ref      PlatformRef   // platform-native identity (see below)
    Preview  Preview
    Caps     Caps
}

type Preview struct {
    AuthorName   string   // display name or handle
    AuthorHandle string   // @handle / acct / npub-ish
    Text         string
    Media        []Media  // url + alt, for thumbnails
    CreatedAt    time.Time
    WebURL       string   // canonical link (njump.me/<nevent> for Nostr)
}

// Caps reports per-action availability; Reason explains a false Allowed.
type Caps struct {
    Reply, Quote, Repost Cap
}
type Cap struct { Allowed bool; Reason string }

func Resolve(ctx context.Context, input string) (*SourceRef, error)
```

`PlatformRef` carries the platform-native identity needed to act:
- Bluesky: `{ URI, CID string }` (+ thread root URI/CID for replies).
- Mastodon: `{ LocalID string }` (the id on *our* instance).
- Nostr: `{ ID nostr.ID; RelayHints []string; Author nostr.PubKey; Kind uint16 }`.

### New *read* methods on the platform clients

The clients are currently write-only (`Post`). Each gains resolve/fetch:

- **`internal/bluesky`** — `Resolve(ctx, url) (*Post, error)` style: parse
  `bsky.app/profile/<handle-or-did>/post/<rkey>`; `com.atproto.identity.resolveHandle`
  (handle→DID); `app.bsky.feed.getPosts` (authed) → uri, cid, author, text, embed,
  counts, `viewer.{replyDisabled,embeddingDisabled}`, threadgate view; for replies,
  `app.bsky.feed.getPostThread` to derive the thread `root` strongRef. Implemented
  with the existing hand-rolled XRPC `do()` (same style as `createRecord`); no new
  dependency. (Indigo typed bindings exist but the client doesn't use them today.)
- **`internal/mastodon`** — `Resolve(ctx, url)`: `GET /api/v2/search?q=<url>&type=statuses&resolve=true&limit=1`
  (auth required) → local status id; `GET /api/v1/statuses/:id` → preview +
  `visibility`, `quote_approval`, counts; cache the instance's `api_versions.mastodon`
  (from `GET /api/v1/instance`) to know if native quote (≥7) is available.
- **`internal/nostr`** — decode the identifier with `nip19.ToPointer` after
  stripping a leading `nostr:`/`web+nostr:` (raw 64-hex handled like
  `internal/verify/nostr.go`); fetch the event via the existing relay `pool`
  (`QuerySingle`) over `RelayHints ∪ ResolveWriteRelays/FallbackRelays`, skipping
  overlay relays; best-effort fetch the author's kind-0 for a display name.
- **`internal/threads`** — no source resolver (unsupported). Detection lives in
  `resolve` and returns a friendly "Threads posts can't be acted on" error.

### Dispatch reuses the existing post/target/history model

An interaction is a `store.Post` carrying an **action descriptor**; it flows
through the existing `Dispatcher`, history, retry, and scheduling unchanged.

- **Reply / Repost** → a Post with a **single target** (the source platform).
- **Quote** → a Post with **multiple targets**: the source-platform native quote
  + one link-quote target per selected fan-out platform. This is structurally
  identical to today's multi-platform post, so target rendering, partial status,
  and retry all work as-is.

New record builders (in the clients / dispatch adapters):
- Bluesky: `app.bsky.feed.repost` (subject strongRef); `app.bsky.embed.record`
  (quote) / `embed.recordWithMedia` if commentary carries media; reply
  `reply.{root,parent}` strongRefs.
- Mastodon: `POST /statuses/:id/reblog`; native quote via `quoted_status_id`
  (with `quote_approval_policy`) when available, else append URL; reply via
  `in_reply_to_id` (already supported).
- Nostr: kind-6 repost (kind-1 source) / kind-16 + `k` tag (other kinds), with
  `e`+`p`+relay tags and `content = json.Marshal(event)` (omit the embed when the
  event is NIP-70 protected); quote = kind-1 with `["q", id, relay, author]` +
  `nostr:nevent…` mention in content; reply via NIP-10.

Replies reuse the B1/B2 reply primitives, with one extension: **Nostr replies to
an external author** must put the *replied-to* author's pubkey in the `p` tag
(not the owner), carry forward the parent's `p` tags (mention propagation), and
add the 5th author element to `e` tags. `internal/nostr.replyTags` and its tests
are updated accordingly (the self-thread case keeps working).

## Data flow

1. **Resolve.** Interact tab → `POST /api/resolve {input}` →
   `resolve.Resolve` → `SourceRef` JSON. UI renders the preview card + per-action
   availability.
2. **Act** → `POST /api/interact`:
   - `{action:"reply", platform, ref, text, overrides, scheduled_at?, force?}` →
     one-target Post.
   - `{action:"repost", platform, ref, force?}` → one-target Post (no text).
   - `{action:"quote", platform, ref, text, overrides, fanout:[platforms],
     scheduled_at?, force?}` → multi-target Post (native + link-quotes).
   `force:true` skips the capability gate (the override).
3. **Dispatch** builds the platform records, writes targets to history, and
   inherits retry + scheduling.

**Quote fan-out specifics.** Native quote on the source platform embeds the
reference (Bluesky `embed.record` renders the card — no URL appended; Nostr
`q`-tag + `nostr:` mention; Mastodon native or, on fallback, URL appended).
Link-quote on each other platform = commentary + the source `WebURL`. For a Nostr
source the `WebURL` is `https://njump.me/<nevent>` so non-Nostr platforms get a
clickable link.

**Universal fallback rule.** Quote is decided per *target* platform: use a native
quote if that platform supports it **and** the source post allows it (or the user
forced the override); **otherwise degrade to a link-quote** (commentary + URL) —
this applies to the source platform too, so a blocked/unsupported native quote is
never silently dropped, it becomes a link-quote. Reply and repost have no such
fallback: if blocked and not overridden, they stay disabled.

## Capability / restriction model

`Caps` is computed at resolve time:

- **Bluesky:** `Reply.Allowed = !viewer.replyDisabled`;
  `Quote.Allowed = !viewer.embeddingDisabled`; `Repost.Allowed = true` (unless the
  post is blocked/not-found). `Reason` from the threadgate view
  ("replies limited to followers", "author disabled quotes"). These viewer flags
  are only meaningful on **authed** requests, so resolve uses the owner's session.
- **Mastodon:** `Quote` from `quote_approval.current_user` — `automatic`→allowed;
  `manual`→allowed with reason "needs author approval" (lands pending);
  `denied`/`unknown`→not allowed (link-quote only). `Repost.Allowed = visibility ∈
  {public, unlisted}` (private/direct can't be reblogged → server returns 404).
  `Reply.Allowed = true`.
- **Nostr:** all allowed (open protocol). If the event carries the NIP-70 `["-"]`
  protected tag, repost won't embed the event JSON; surface a soft note steering
  toward quote.
- **Threads:** source actions all disallowed (can't resolve); fan-out target only.

UI shows the reason and disables the action by default, but a disabled action is
still clickable with a "try anyway" confirm that sets `force:true`, warning that
**Bluesky may silently drop** the result. A blocked native quote never blocks the
link-quote fan-out.

## UI — the Interact tab

A new tab alongside Compose / History / Scheduled / Tools / Verify.

- **Smart input:** one field accepting a URL or a Nostr identifier; on submit
  (or paste-and-debounce) calls `/api/resolve`.
- **Source preview card:** platform badge, author (name + handle), text (with the
  existing token highlighting), media thumbnails, timestamp, and a link to the
  original. Resolve errors render here (unreachable relay, unfederated Mastodon
  post, Threads-unsupported, private/blocked).
- **Action row:** Reply · Repost · Quote, each showing enabled/disabled + reason.
  - **Reply / Quote** reveal the existing compose editor (textarea, live
    count/thread preview, scheduling). Reply is scoped to the source platform;
    Quote adds **fan-out platform toggles** (your other platforms, default off).
  - **Repost** is a single confirm button.
- Reuses `common.js` (`el`, `api`, `flash`, `confirmModal`), `state.js`,
  `preview.js`, and the toast/modal patterns. On submit it posts to
  `/api/interact` and surfaces the resulting targets (success / partial), with the
  same retry affordances as History.

## Storage & history

Reuse `store.Post` / `store.Target`. Add an **action descriptor** to the post:
`{ action: "reply"|"repost"|"quote", source_platform, source_ref, source_web_url,
source_author }`. History renders interactions as
"↩ replied to / ❝ quoted / 🔁 reposted @author" linking to the source; retry,
partial status, and segment chains (a quote's commentary can itself thread) keep
working. Migration is additive (a JSON column, like `segments_json`).

## Errors

- **Resolve:** unreachable/unfederated source, private/blocked post, malformed
  input, Threads URL → a clear reason on the card; never a 500.
- **Act:** per-target failures flow through the existing partial/retry machinery.
  Overridden actions that the platform silently drops (Bluesky) are recorded as
  attempted; the UI's warning sets expectations.

## Testing

- **`internal/resolve`:** unit-tested with per-platform fakes — URL/identifier
  parsing and platform detection, the capability mapping (esp. the Mastodon
  version × `quote_approval` matrix and Bluesky viewer flags), Nostr NIP-19
  decode + `nostr:` stripping + hex.
- **Clients:** read methods tested against recorded fixtures (Bluesky getPosts /
  getPostThread shapes, Mastodon search/status JSON, Nostr event fetch via a fake
  relay), including restriction fields.
- **Dispatch builders:** tested with the existing fake posters — record shapes for
  repost/quote/reply, **fan-out target expansion** (one quote → native + N
  link-quotes), `njump.me` URL for Nostr sources, and `force` override behavior.
- Live posting verified manually with real credentials, as with prior features.

## Out of scope (v1)

- Threads as a source (hard API limitation).
- Browsing/searching a timeline inside publisher (input is paste-only).
- Editing/deleting/un-reposting interactions from the UI (history shows them; use
  the platform to undo).
- Cross-platform *reply/repost* (meaningless); only *quote* fans out.

## Decomposition note

This is large; expect the implementation plan to split into stages, e.g.:
- **Plan A — resolve + preview (read-only):** `internal/resolve`, client read
  methods, `POST /api/resolve`, the Interact tab in read-only preview mode
  (paste → card → capability display). Ship-able and testable on its own.
- **Plan B — actions:** `POST /api/interact`, dispatch record builders
  (repost/quote/reply incl. Nostr external-author tags), quote fan-out, store
  action descriptor + history rendering, override.

(Final decomposition decided in writing-plans.)

## Sources (platform research, 2026-05-25)

- **Bluesky:** docs.bsky.app posts/rate-limits/getPostThread; atproto.com/blog/create-post;
  threadgate/postgate lexicons; indigo `api/bsky`, `api/atproto` (replyDisabled/
  embeddingDisabled enforcement is client-side — atproto discussion #2576).
- **Mastodon:** docs.joinmastodon.org client/quotes, entities/Quote, entities/Status,
  methods/statuses, methods/search; blog.joinmastodon.org 2025-10 "4.5 for devs"
  (native quote = server 4.5 / API v7).
- **Threads:** developers.facebook.com/docs/threads — publishing reference
  (`reply_to_id`, `quote_post_id`, `/{id}/repost`), retrieve-media (no URL→id
  resolver; Advanced Access for others' posts).
- **Nostr:** NIPs 19 (identifiers), 21 (`nostr:` URI), 10 (replies), 18
  (quote `q`-tag / repost kinds 6 & 16), 70 (protected events); `fiatjaf.com/nostr`
  `nip19.ToPointer`, relay `pool.QuerySingle`.
