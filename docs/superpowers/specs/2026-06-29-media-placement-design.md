# Media placement: anchor attachments to a chosen post in the thread

**Date:** 2026-06-29
**Status:** approved (design), pending implementation plan
**Decisions made with operator:**
- Unit of control = a **specific post** in the thread, per-image, and may differ per platform.
- Must work in **both** threading regimes (manual `---` and auto/length split).
- Per-platform divergence is **rare** — one shared placement that maps onto every
  platform, with the occasional single-network override.
- Mechanism = **Approach A**: placement encoded in the draft text as anchor
  markers (the same philosophy as `---`), not a separate per-platform schema.
- **Images only** in v1; the existing "one video OR images per post" rule stands,
  so a video remains a single post and needs no placement control yet.

## Background

Publisher threads a draft either by honoring manual `---` breaks or by
auto-splitting over-limit text per platform (`internal/thread`). Attached media
is a flat, ordered list owned by the post as a whole; `thread.PlanMedia`
(thread.go:53) is the only placement logic and is hard-coded to **front-load**:
fill the head post up to the platform cap, spill to following posts, append
image-only posts when images outrun text (Mastodon's 4-attachment cap). The
10-image gallery spec (2026-06-09) explicitly deferred manual placement, noting
"this feature builds its rails."

This feature lets the operator decide which post each image rides on.

### Constraints inherited from the architecture

- **The plan is re-derived, not stored.** Resume (`resumeSegments`,
  dispatch.go:502) and republish recompute the plan from raw counts; per-segment
  assignments are deliberately not persisted. Any placement must stay
  deterministic across those re-derivations.
- **Thread shape is per platform.** Bluesky (300) may produce 4 posts where
  Mastodon (500) makes 2 and Nostr (unbounded) makes 1. A raw "post index" does
  not translate across platforms; placement must resolve against each platform's
  own split.
- **The whole pipeline assumes contiguous head-fill.** `plan []int` is per-post
  *counts*; dispatch slices `imgs[start:start+count]` (dispatch.go:422) and the
  preview does the identical `media.slice(...)` (preview.js:235).

## Scope

- Per-image control over which post an attachment appears on, in both regimes.
- One placement intent shared across platforms; per-platform override via the
  existing per-platform text-override mechanism (no new per-platform UI in v1).
- Zero-regression default: a draft with no anchors posts byte-for-byte as today.

Out of scope (v1): video placement (one-video-or-images rule unchanged);
drag-and-drop placement UI (tap-chip instead); a dedicated per-platform
placement control (use text overrides); reordering attachments by placement.

## Design

### 1. Model — logical posts are `---`-delimited blocks

A draft splits into **blocks** at `---` (existing `splitMarkers`). A draft with
no `---` is one block. Auto-splitting by length happens *within* a block, per
platform. The block is the addressable unit for media.

Each image is assigned to a block; **default is the head block** (= today's
front-load). Assignment is encoded in the text as a **media-anchor marker** — a
line of its own, symmetric with `---`:

```
Here's the new feature.
---
This screenshot shows the dialog.
⟦media 2⟧
---
And the wrap-up.
```

`⟦media 2⟧` inside the second block assigns attachment #2 (1-based, attach
order) to that block. Because it occupies its own line it is extracted before
length-counting, so it never inflates char counts and never splits mid-token,
exactly like `---`.

**Marker grammar:** only a **whole trimmed line** matching `⟦media <digits>⟧`
(one or more comma-separated digits, e.g. `⟦media 1,3⟧`) is a marker. Inline
occurrences in prose are left as text. The `⟦ ⟧` brackets are chosen for rarity
in social-post content. A block may contain several marker lines; the block's
image set is the **union** of every ordinal across all its markers (so
`⟦media 1⟧` + `⟦media 3⟧` and `⟦media 1,3⟧` are equivalent). The UI writes one
comma-listed marker line per block; multi-line is accepted for hand edits. An
ordinal that appears in two blocks resolves to its **last** occurrence (the UI
never produces this; it is a hand-edit tie-break).

**Resolution at split time, per platform:**
1. Split into blocks by `---`.
2. Extract `⟦media N⟧` lines per block → that block's explicit image set; strip
   them from the visible text.
3. Auto-subdivide each block by length (existing logic).
4. Distribute each block's images across *its own* sub-posts via the existing
   `PlanMedia` rule (first sub-post up to cap, overflow to later sub-posts).
5. The head block additionally absorbs any **unassigned** images (front-load).

Within any post, images render in attach order. Unassigned images front-load on
the head block; assigned images go to their block.

**Orphan rules (hand-edited / stale markers):** a marker referencing a
non-existent attachment ordinal is ignored; an image whose block was deleted
falls back to the head. Both deterministic; surfaced as a soft preview note,
never a hard error.

### 2. Thread layer (pure, internal/thread) — counts become index lists

Anchored images are no longer a contiguous head-fill, so the per-post plan
generalizes from **counts** to **explicit attachment indices**:

```
// today:     plan []int     e.g. [2, 1, 0]        (counts per post)
// proposed:  plan [][]int   e.g. [[0,3],[1],[]]   (attachment indices, attach order)
```

- Marker parsing and block→image resolution live entirely in `internal/thread`
  (its most-tested package; `splitMarkers` already lives here).
- `SplitWithMedia` keeps its orchestrating role but its image input grows from a
  bare count to the set of attachment indices that exist (so it can resolve
  markers and front-load the unassigned ones), and it returns `[][]int`.
- The numbering fixpoint runs *after* marker extraction and block-scoped
  planning. Total posts = Σ over blocks of (text sub-posts + image-only
  overflow), which stays monotone and bounded — the existing convergence
  argument holds, computed per-block.
- Determinism is preserved: placement lives in the (already-persisted) text, so
  resume/republish re-derive the identical `[][]int`. **No store schema change.**

### 3. Dispatch (internal/dispatch)

`runChain` (dispatch.go:417-435): the contiguous slice
`imgs[starts[i]:starts[i]+plan[i]]` becomes a gather by index list,
`pick(imgs, plan[i])`. Same chain threading, same up-front pending records; only
per-post image selection changes. `resumeSegments` (dispatch.go:502) re-derives
the identical assignment from the persisted segment text it already replays —
the documented "re-derive from text" determinism carries over unchanged,
including its existing best-effort caveat about a skipped Blossom re-fetch
re-indexing later images (anchoring does not worsen it). `PlanBlueskyCard`
(dispatch/card.go) gets the same `[][]int` treatment.

### 4. API (internal/api)

`POST /api/thread-preview` (api.go:1527): `Imgs []int` → `Imgs [][]int` (each
post's attachment indices). Same single source of truth as dispatch — preview
and posting cannot diverge. The stale-bundle fallback (missing field → head
only) stays. `assembleImages` / spec ingest are unchanged: markers travel inside
`master_text` (and per-platform `Overrides[p].text`), which are already
persisted on the post spec, drafts, and scheduled posts.

### 5. Web UI (internal/web/assets)

- **Placement chip per thumbnail** (`renderImages`, compose.js:860): a
  touch-friendly stepper/dropdown — `▸ post 1`, `▸ post 2`, … — whose options
  are the current logical posts (blocks). Picking a post writes/moves that
  image's `⟦media N⟧` marker into the matching block in `state.master`. Default
  chip = post 1, so an untouched draft is unchanged. With only one block the chip
  is inert and offers "Add a break to place later →", which drops a `---` via the
  existing Edit-split machinery (one tap, no context switch).
- **Live preview is the WYSIWYG feedback** (preview.js:235):
  `media.slice(...)` → `pv.imgs[i].map(ix => media[ix])`. Each platform's preview
  renders images on their assigned posts, updating per-network as chips change.
  The split sheet's timeline reflects the same.
- **Per-platform override (rare path)** rides the existing `ov.text` override:
  the strip writes to `state.master` (applies everywhere); to diverge on one
  network the operator gives that platform a text override whose markers win for
  it. No dedicated per-platform placement UI in v1.
- **Marker hygiene** is the UI's job: add/remove/reorder in the strip rewrites
  markers (positional references). Markers are visible in the raw `#master`
  textarea (like `---`), with the chip as the friendly editor.
- **Client estimator** (`threadInfoFor`/`previewMedia`, compose.js) mirrors the
  per-block totals so the live thread badge matches the server.

### 6. Explicitly unchanged

Store schema, retrier, progress/SSE, verifier, video gate / transcoding, the
one-video-or-images attach guard, Blossom upload paths.

## Error handling

- **Block over platform cap** (e.g. 6 images on one block, Mastodon cap 4): fill
  the block's sub-posts up to cap, overflow to appended image-only posts —
  existing overflow, scoped per-block; numbering fixpoint counts them.
- **Block is only a marker** (`⟦media 2⟧` alone): image-only post (empty text, or
  bare counter when numbering on) — existing image-only-segment path.
- **Bluesky link card on an assigned image's post**: the card owns the embed
  slot; assigned images there spill to an appended image-only post in that block.
  `PlanBlueskyCard` reconciles; preview reflects it.
- **Nostr (cap 0)**: no attachment slots — each block's assigned images become
  URLs/imeta on that block's post (today all on head; this generalizes
  per-block).
- **Orphan / sentinel-collision**: covered by the marker grammar and orphan
  rules in §1 — soft notes, never hard failures.

## Testing

TDD throughout, matching the existing table-driven `thread`/`dispatch` tests:

- **thread (core):** whole-line marker parsing & inline-collision immunity;
  block→image resolution; per-block overflow; image-only blocks;
  unassigned-fills-head; orphan handling; numbering fixpoint with anchors; Nostr
  cap-0 path.
- **Determinism:** same input → identical `[][]int` across repeated calls.
- **Regression golden test:** no markers → plan byte-identical to today.
- **dispatch:** per-post gather posts the right images; `resumeSegments`
  re-derives the identical assignment (via the existing fake clients).
- **Preview parity:** `/api/thread-preview` `Imgs [][]int` equals what dispatch
  posts; client estimator mirrors the server (the recurring counter/splitter
  parity-bug area gets explicit coverage).
- **Bluesky card interaction:** assigned image on the card's post spills
  correctly.
- **Manual e2e on Oppy:** a 3-post thread with an image anchored to post 2 across
  all four platforms; confirm placement, numbering, and resume after a forced
  mid-chain stop.

## Risks

- **Marker visibility in the raw textarea.** Operators editing `#master` directly
  see `⟦media N⟧` lines. Mitigated by the chip being the primary editor and the
  precedent of visible `---`. If hand-editing markers proves common, a later
  refinement could reference attachments by a stable id instead of position.
- **Per-platform divergence requires a full text override** in v1 (forking that
  platform's text to move one image). Accepted because divergence is rare; a
  dedicated per-platform control is a future enhancement.
- **Precise placement in a pure auto-split draft requires introducing a break**
  at the placement point (the chip offers this). Honest consequence of the
  block model: the boundary must exist to be addressable.
