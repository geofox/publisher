# Media placement: pin attachments to a post in a threaded draft

**Date:** 2026-06-29
**Status:** approved (design), pending implementation plan
**Decisions made with operator (after two multi-agent design critiques):**
- Pin each image to a chosen **post** of a thread; per-image.
- **v1 scope = manual/threaded drafts only.** Placement is offered only when the
  draft already has `---` parts (≥2). Parts are operator-defined and
  platform-stable, so part ≈ post, the preview always shows segments, and there
  is no "insert a break to place a picture" footgun. Auto-split-only and
  single-post placement are **deferred** (that is where the unsolved UX/preview
  problems live — see Revision history).
- **Shared placement only.** One assignment maps onto every platform. The rare
  per-platform case uses the existing per-platform text override; no per-platform
  anchor delta in v1.
- **Images only.** The "one video OR images per post" rule stands.

## Revision history (why this is scoped the way it is)

This spec went through two design-critique rounds (three Opus subagents each),
which reshaped it substantially. Recording the dead ends so they are not retried:

- **v1 — in-text `⟦media N⟧` markers — REJECTED.** Putting the anchor inside the
  draft text collided with translation (DeepL mangled markers), client/server
  char-count parity, and the Edit-split sheet.
- **v2 — structured anchor map, "part" granularity, per-platform delta,
  "no persistence" — REJECTED.** The "rides existing JSON blobs for free" claim
  was false: `FieldsJSON` is written through `ov2fields`, a hand-picked field map
  (dispatch.go:946), not a struct marshal; the shared map had no post-level home;
  the draft path re-marshals through typed `draftSpecJSON` (drafts.go:91-146)
  that drops unknown keys. Resume still front-loaded (it never re-splits text).
  Ordinal keys were already unstable across save/load/recovery. And "part"
  granularity abandoned the auto-split regime.
- **v3 (this doc) — structured map, manual/threaded-only, shared-only,
  stable-id, persist-the-resolved-plan.** Each cut removes a whole class of the
  problems above.

## Background

Publisher threads a draft by honoring manual `---` breaks or auto-splitting
over-limit text per platform (`internal/thread`). Media is a flat ordered list
owned by the post as a whole; `thread.PlanMedia` (thread.go:53) hard-codes
front-load. The 10-image gallery spec (2026-06-09) deferred manual placement,
noting "this feature builds its rails." v1 builds the smallest correct version:
in an already-threaded draft, pin each image to a part.

### Inherited architecture facts (verified)

- **Resume/Fire replay persisted segments; they do not re-split.**
  `resumeSegments` (dispatch.go:520-538) posts `segs[i].Text` verbatim and
  recomputes media only as `PlanMedia(len(imgs), len(segs), cap)` (dispatch.go:502)
  — a count, with no placement input. Therefore placement must be **persisted**,
  not re-derived. `Segment`s serialize as JSON in the `segments_json` column
  (models.go:136/480), so a new `Segment.Images []int` is additive — no SQL
  migration.
- **The pipeline assumes contiguous head-fill.** `plan []int` is per-post counts;
  dispatch slices `imgs[start:start+count]` (dispatch.go:422, prefix-sum at
  408-415), preview does `media.slice(...)` (preview.js:235), `PlanBlueskyCard`
  tests `plan[target] > 0` (card.go:67). Anchoring breaks contiguity → the plan
  generalizes to explicit attachment indices `[][]int`.
- **Manual `---` parts apply to every network** (compose.js footer:398), and
  Nostr honors them too (`splitMarkers` is length-independent). So ≥2 parts ⇒ a
  real multi-post thread on every platform ⇒ the preview always renders segments.

## Scope

- Pin each image to a part (`---` block) of a draft that has **≥2 parts**; default
  (unset) = part 0 (head) = today's front-load.
- A part with more images than its platform cap overflows into appended
  image-only posts within that part (existing `PlanMedia` overflow, per-part).
- Zero-regression: no anchors → byte-identical to today.

Out of scope (v1, deferred with explicit seams): placement in single-`---`-part
or purely auto-split drafts; per-platform anchor deltas; Bluesky card "spill";
video placement; interaction/quote-reply placement; drag-and-drop.

## Design

### 1. When active, and the data model

Placement UI activates only when `splitMarkers(master)` yields ≥2 parts.

Each attachment carries a **stable `id`** (client-generated at attach time;
added to `imageSpec`, api.go:810, and the draft image JSON). Placement is a
shared map keyed by that id:

```
anchors: map[imageID]partIndex   // 0-based over the --- parts; missing ⇒ part 0
```

The id (not the ordinal) is the key, so add/remove/reorder needs **no map
maintenance** — a removed image's entry is simply ignored, and an image keeps its
id when reordered. This kills the save/load/recovery renumber bug class that
sank v2's ordinal keys (serialize used array index, load used stored ordinal,
recovery dropped fresh images and renumbered — all moot when the key is an id).

**Re-validation:** an entry whose `partIndex >= nParts` clamps to the last part;
an entry for a missing id is ignored. Deterministic.

### 2. Resolution → the `[][]int` plan (pure, internal/thread)

The caller resolves each image's part (`anchors[id] ?? 0`, clamped) and passes
the per-image part assignment to `SplitWithMedia`, which returns `[][]int`
(attachment indices per post). Per platform:

1. Split into parts by `---` (existing `splitMarkers`).
2. Auto-subdivide each part by length (existing `splitAt`, text-only — unchanged).
3. For each part, gather its images (by attach order) and distribute across the
   part's sub-posts via `PlanMedia` (head sub-post up to cap, overflow to
   appended image-only posts **for that part**).
4. The head part also absorbs every unassigned/clamped image.

**Canonical chain ordering** (golden-tested; resume depends on it being a pure
function of inputs): posts are emitted **part by part, in order**; within a part,
**text sub-posts first, then its overflow image-only posts**. Within a post,
images sort by attach order. Image-only posts are materialized in
`SplitWithMedia` (media-aware), **not** in `splitAt` — so `splitAt`'s
drop-empty-blocks contract is untouched.

**Numbering convergence (the v2 open question, now resolved).** Total posts
`T = Σ_part max(textSubposts_part(budget), ⌈images_part / cap⌉)`. The image floor
`⌈images_part/cap⌉` is a **constant** (independent of the counter budget);
`textSubposts_part` is **monotone non-decreasing** as the budget shrinks (smaller
limit ⇒ same-or-more sub-posts). `max(monotone, const)` is monotone, and a sum of
monotone-non-decreasing terms is monotone-non-decreasing. So `T` is monotone in
(1/budget) and bounded above (text cannot split past one grapheme per post) ⇒ the
existing `number()` fixpoint (thread.go:289) converges by the same argument it
uses today, now with a per-part sum instead of a single global `extra`. The
`len(segs) == len(plan)` invariant holds because every emitted post (text or
image-only) gets exactly one plan entry.

### 3. Dispatch (internal/dispatch)

- `runChain` (dispatch.go:417-435): replace the `starts[]` prefix-sum (408-415,
  now dead) and `imgs[start:start+count]` slice with a gather by index list,
  `pick(imgs, plan[i])`. **Record `Segment.Images = plan[i]`** on each segment as
  it is planned (so resume can read it).
- **Nostr imeta** (dispatch.go:424): today `if i==0 { segImetas = imetas }`
  attaches all imeta to the head. A part can now hold media on a non-head post,
  so imeta is distributed per post in step with `plan[i]`.

### 4. Resume / Schedule-Fire (internal/dispatch)

Resume and Fire **read the persisted `Segment.Images`** and gather by index — no
re-split, no re-derivation, no `FinalText`/card-idempotency hazard. Remove the
`PlanMedia(count)` call and `starts[]` prefix-sum at dispatch.go:502-508; redesign
the "trailing images exceed segments" warning (dispatch.go:511-518) — meaningless
for index lists — into per-segment bounds/orphan handling. `Schedule`
(dispatch.go:1041) pre-splits and persists segments; it records `Segment.Images`
the same way, so Fire is identical to resume.

### 5. Bluesky link card (internal/dispatch/card.go)

The predicate becomes `len(plan[target]) > 0`. **No spill in v1:** if an image is
anchored onto the post that would carry the card, the **card wins** and that image
falls back to front-load, with a preview warning. (The v2 "insert a post and
re-thread" spill is deferred — it entangles reply-pointer/`card.Segment`
re-indexing with resume reproduction.)

### 6. Persistence (verified — no SQL migration)

- **Resolved plan:** `Segment.Images []int` rides the existing `segments_json`
  JSON column (models.go:136/480) — additive.
- **Draft:** add `anchors` and a per-image `id` to `draftSpecJSON` (drafts.go:37)
  so they survive the API's re-marshal (the v2 trap) and round-trip via
  `spec_json`. Update `buildSpec`/`imageSpecs` (state.js) and the recovery
  snapshot (drafts_recovery.js) to emit both; recovery restore (drafts.js:274)
  drops un-uploaded images — anchors keyed by id simply orphan those entries, no
  renumber.
- **Translation:** `handleTranslateDraft` (drafts.go:255-267) must copy `anchors`
  + image ids onto the new draft. Shared-only means the `Overrides:"{}"` reset is
  irrelevant. Same for the history.js translate path.
- **Untouched:** `Media` table, `ov2fields`, `ovFor`, `FieldsJSON` — shared-only
  placement needs none of them.

### 7. API (internal/api)

`POST /api/thread-preview` (api.go:1527): request gains `anchors` + image ids;
`Imgs []int` → `Imgs [][]int`. Same planner as dispatch — preview and posting
cannot diverge. `postSpecJSON` / `assembleImages` (api.go:825/810) carry `anchors`
and ids into `dispatch.PostSpec`.

### 8. Web UI (internal/web/assets)

- **Placement chip per thumbnail** (`renderImages`, compose.js:860), shown only
  when the master has ≥2 parts: a `▸ part 1 … part n` picker writing
  `anchors[id]`. Default part 1 ⇒ untouched drafts unchanged. Labeled "part N"
  (honest: a part may auto-subdivide, so it is not strictly "post N").
- **Live preview** (preview.js:235): `media.slice(...)` →
  `pv.imgs[i].map(ix => media[ix])`. With ≥2 parts the threaded preview always
  renders, so placement is always visible (the v2 single-post blind spot is out
  of scope by construction).
- **Thread badge count**: source the per-platform post count from the existing
  `/api/thread-preview` round-trip (preview.js:372) rather than re-estimating in
  `threadInfoFor` (compose.js:113), so the badge cannot drift from the server —
  the recurring counter/splitter parity-bug surface is avoided, not re-fought.
- **Char counting, `splitMarkers`, `bskyCardText` are untouched** — anchors are
  not in the text.

### 9. Explicitly unchanged

SQL schema (only additive JSON fields), retrier, progress/SSE, verifier, video
gate/transcoding, one-video-or-images guard, Blossom upload, DeepL request shape,
`ov2fields`/`ovFor`, the `Media` table.

## Error handling

| Case | Behavior |
|---|---|
| **Part over platform cap** | Fill the part's sub-posts up to cap, overflow to appended image-only posts within that part; numbering fixpoint counts them (§2). |
| **Anchor out of range / orphan id** | Clamp to last part / ignore; soft preview note, never a hard error. |
| **Image anchored to Bluesky card's post** | Card wins; image front-loads; preview warning (§5). |
| **Nostr (cap 0)** | Each part's images become URLs/imeta on that part's post; imeta per post (§3). |
| **Draft drops below 2 parts after an edit** | Chips hide; existing `anchors` are retained but inert (clamp/ignore) until parts return. |
| **Interaction/quote-reply mode** | Placement disabled; chip hidden (consistent with Edit-split). |

## Testing

TDD, table-driven like the existing `thread`/`dispatch` tests:

- **thread (core):** anchor resolution → `[][]int`; canonical part-ordered chain
  with mid-chain image-only posts; per-part overflow; head-part absorbs
  unassigned; clamp/orphan; numbering fixpoint convergence (assert the monotone
  bound) with `len(segs)==len(plan)`; Nostr cap-0 per-part.
- **Determinism / regression:** same inputs → identical `[][]int`; no anchors →
  byte-identical to today.
- **dispatch:** per-post gather; `Segment.Images` recorded; nostr per-post imeta;
  **resume/Fire read `Segment.Images` and reproduce the original placement**
  (the bug both critique rounds caught — explicit test).
- **card:** `len(plan[target])` predicate; card-wins-front-load fallback + warning.
- **api/preview parity:** `Imgs [][]int` equals dispatch; badge count sourced from
  the preview round-trip matches.
- **drafts:** `anchors` + ids round-trip save/load/autosave; recovery orphans
  dropped images without renumber; **translate copies anchors**.
- **Manual e2e on Oppy:** a 3-part thread, image anchored to part 2, across all
  four platforms; confirm placement, numbering, and resume after a forced
  mid-chain stop.

## Risks

- **Numbering fixpoint with mid-chain image-only posts** is the highest-risk code
  area; §2 gives the convergence argument but it must be encoded as an explicit
  bound + tests, treated as its own task.
- **Stable-id plumbing** spans client attach, draft JSON, and recovery; the id
  must be generated once and never regenerated on reorder. Test the recovery
  boundary specifically.
- **Deferred seams:** per-platform deltas, card spill, and auto-split/single-post
  placement are intentionally cut; the data model (id-keyed shared map, `[][]int`
  plan) leaves clean seams to add them later without rework.
