# Auto-Threading Composer — Design

**Date:** 2026-05-25
**Status:** Approved (design); pending implementation plan
**Branch:** `brainstorm/new-features` (rename/rebranch for implementation)

## 1. Overview & scope

A long master draft is split into platform-sized segments and posted as a
**native reply-chain** on each selected platform (Nostr, Bluesky, Mastodon,
Threads).

Decisions:

- **Hybrid splitting:** manual `---` break markers are respected; otherwise
  over-limit text is auto-split at natural boundaries.
- **Native per-platform sizing:** each platform threads to its own limit;
  platforms where the whole draft fits post a single post (Nostr, which has no
  practical limit, stays a single note unless markers force breaks).
- **Stop & resume** on a mid-chain failure (non-destructive).
- **Numbering on by default:** per-platform ` k/n` counter, suffix-budgeted in
  the splitter; single-segment posts get no counter; a compose toggle disables it.
- **Images** attach to the first segment.
- **Auto-engage:** threading kicks in when text exceeds a platform limit or `---`
  markers are present — replacing today's hard "over 300 graphemes" Bluesky error.

### In scope (v1)

- The `internal/thread` splitter (pure).
- Reply-chain posting for all four platforms.
- Sequential dispatch with per-segment persistence and resume.
- `store.Target.Segments` model extension.
- `POST /api/thread-preview` and compose preview of the computed split.
- History rendering of threaded targets (+ partial badge + resume action).

### Out of scope (v1)

- Per-segment media placement (all images go on segment 1).
- Cross-platform uniform numbering (numbering is per-platform by design).
- Editing a published thread.
- Threading replies to *someone else's* post (this threads the owner's own draft).

## 2. The splitter — `internal/thread` (pure, isolated core)

```go
type Opts struct {
    Number bool // append " k/n" when the chain has >= 2 segments
}

// Split returns the ordered segments for one platform. limit is the per-platform
// grapheme budget; limit <= 0 means "no limit" (returns one segment, unless
// markers force breaks). With Number set, returned segments already include the
// trailing counter and are each guaranteed <= limit.
func Split(text string, limit int, opts Opts) []string
```

**Algorithm:**

1. Split `text` on lines consisting solely of `---` into user segments (manual
   markers). With no markers, the whole text is one user segment.
2. For each user segment longer than `limit` (grapheme count via `uniseg`,
   matching Bluesky's counting), sub-split at the best boundary `<= limit`:
   paragraph (`\n\n`) → sentence (`. ` / `! ` / `? ` / newline) → word (space);
   never mid-word and never mid-URL (a URL token is kept whole).
3. A single token longer than `limit` (e.g., a giant URL) is hard-split as a last
   resort and a warning is recorded.
4. `limit <= 0` (Nostr): no length splitting; markers still apply.

**Numbering (`Number: true`):**

- Applies only when the resulting chain has `>= 2` segments. A single segment
  gets no counter.
- Numbering is **per-platform** (`n` = that platform's chain length), so the same
  draft may be `1/4`…`4/4` on Bluesky and `1/2`…`2/2` on Mastodon.
- The counter consumes budget and its width depends on `n`, which depends on the
  budget — resolved by iterating to a fixpoint: split → provisional `n` → reserve
  suffix width (` k/n`, sized to `n`'s digit count) → re-split against the reduced
  limit → repeat until `n` is stable (1–2 passes). Guarantees every numbered
  segment is `<= limit`.
- Default format: trailing ` k/n` (e.g., `…working on nostr? 1/4`). Tunable.

The splitter has **no I/O** and is exhaustively unit-tested: natural-boundary
selection, manual markers, URL non-splitting, grapheme clusters/emoji, the
no-limit (Nostr) no-op, numbering correctness, the suffix-budget fixpoint
(including the case where reserving space pushes `9 → 10` segments and widens the
suffix), and single-segment = no counter.

## 3. Per-platform reply threading

Each client/adapter gains a `replyTo` reference; segment *k+1* replies to segment
*k*. Segment 1 is a normal top-level post.

- **Nostr:** NIP-10 tags added to the event before signing — `["e", <rootID>,
  <relayHint>, "root"]`, `["e", <parentID>, <relayHint>, "reply"]`, and
  `["p", <ownPubkey>]`. Root = segment 1's id; parent = previous segment's id.
- **Bluesky:** `reply: {root:{uri,cid}, parent:{uri,cid}}` on the record. The
  `uri`+`cid` of each segment come from its `createRecord` response (already
  returned today as `bluesky.Result.CID`).
- **Mastodon:** `in_reply_to_id` = previous status id.
- **Threads:** `reply_to_id` = previous media id, added to the container-create
  step of the existing create → poll → publish flow.

## 4. Dispatch sequencing & store model

- `store.Target` gains:
  ```go
  Segments []Segment
  // Segment: Ordinal int, Text string, RemoteID, RemoteURL, CID string,
  //          Status string ("success"|"failed"|"pending"), Error string
  ```
  A normal (single) post is a 1-segment chain, so the common path is essentially
  unchanged. The migration is additive — existing rows read as single-segment.
- The splitter runs **per platform** inside dispatch to produce each target's
  segments (using that platform's limit and the numbering toggle).
- The dispatcher posts each platform's chain **sequentially**, threading the
  prior segment's reference into the next, and **persists after each segment** so
  a crash or failure leaves an accurate partial chain.
- Platforms remain independent of each other (Bluesky failing does not block
  Mastodon), matching today's behavior.

## 5. Failure & resume

- If segment *k* fails, that platform's target is marked `partial` at ordinal
  *k*; segments `> k` are not attempted. Segments `< k` keep their `success`
  state and stored references.
- Resume extends the existing `Retry` path with "continue chain from the first
  non-success segment," re-threading from the last successful segment's stored
  reference. Already-succeeded segments are **never** re-posted (idempotent).
- The Nostr per-relay retry (`RetryRelay`) continues to operate per segment event
  where applicable.

## 6. API & compose preview

- `POST /api/post` is unchanged in request shape (master text + platforms +
  images + overrides). Threading is computed server-side; the response's per-
  platform target now carries `segments`.
- New `POST /api/thread-preview`: body `{ text, platforms, number }`; returns,
  per selected platform, the computed segments (count + text, including the
  numbering) and any hard-split warning. Read-only; no posting. Used by the
  compose preview so the user sees exactly how the draft will break before
  posting. Body-capped and behind the existing CSRF/security middleware.

## 7. History UI

A threaded target renders as an ordered list of its segments, each linking to its
remote URL, with:
- a `partial` badge when a chain stopped early, and
- a **resume** action that calls the extended retry endpoint.

Reuses the existing history detail panel and styling. A "number thread posts"
toggle (default on) is added to the compose view.

## 8. Testing

- **Splitter (`internal/thread`):** exhaustive table tests (pure, offline) — all
  boundary cases, markers, URLs, graphemes/emoji, Nostr no-op, numbering, the
  suffix-budget fixpoint, single-segment = no counter.
- **Reply threading:** per-client tests with fakes asserting the correct reply
  references are sent (NIP-10 tags, Bluesky root/parent uri+cid, Mastodon
  `in_reply_to_id`, Threads `reply_to_id`).
- **Dispatch:** an offline test that a 1-segment post behaves exactly as today
  (no regression); a multi-segment chain persists and resumes correctly using a
  fake adapter that fails at segment `k` (asserts segments `< k` are not
  re-posted on resume, and `>= k` complete).
- **Live smoke** per platform is deferred to manual verification (as with the
  verification feature's integration tests).

## 9. Risks / scope

- **Threads is the heaviest:** its create → poll → publish cycle runs per
  segment, so an N-post chain is ~N× the round-trips and polling; `reply_to_id`
  support is the main integration risk and should be validated early against a
  real account.
- **Grapheme/URL counting on Bluesky:** the splitter must match Bluesky's
  grapheme counting exactly to avoid an off-by-one rejection; covered by tests
  using the same `uniseg` path the Bluesky client uses.
- **Store migration:** additive `Segments` field; old rows deserialize as a
  single-segment chain. No destructive migration.
- **Partial-thread UX:** a `partial` thread is a real, visible state; the history
  UI must make resume obvious so a half-posted thread isn't silently abandoned.

## Decision log

- **Feature chosen:** auto-threading composer (from authoring theme).
- **Split control:** hybrid (manual `---` markers + auto natural-boundary split).
- **Per-platform sizing:** independent/native (each to its own limit).
- **Partial failure:** stop & resume (non-destructive).
- **Numbering:** on by default, per-platform ` k/n`, suffix-budgeted in the
  splitter, single-segment = none, compose toggle to disable.
- **Architecture:** Approach A — isolated `internal/thread` splitter + extend
  `store.Target` with an ordered `Segments` chain + sequential dispatch with
  per-segment persistence.
- **Platform scope:** all four (Nostr, Bluesky, Mastodon, Threads).
- **Media:** all images on segment 1.
