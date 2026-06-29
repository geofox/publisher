# Media placement: anchor attachments to a chosen part of the thread

**Date:** 2026-06-29
**Status:** approved (design), pending implementation plan
**Decisions made with operator:**
- Unit of control = which **post** in the thread each image rides; per-image;
  may differ per platform.
- Must work in **both** threading regimes (manual `---` and auto/length split).
- Per-platform divergence is **rare** — one shared placement that maps onto every
  platform, with the occasional single-network override.
- **Mechanism = a structured anchor map**, not in-text markers (see "Why not
  in-text markers" below — reversed after a three-agent design critique).
- **Granularity = the `---` "part"**, labeled honestly. An image is pinned to a
  part (a `---`-delimited block); placing it on a sub-post of an *auto-split*
  part is out of scope — to place mid-auto-thread you insert a break. The UI
  says "part 1/2/3", never "post N".
- **Images only** in v1; the existing "one video OR images per post" rule stands.

## Background

Publisher threads a draft by honoring manual `---` breaks or auto-splitting
over-limit text per platform (`internal/thread`). Attached media is a flat,
ordered list owned by the post as a whole; `thread.PlanMedia` (thread.go:53) is
the only placement logic and hard-codes **front-load**: fill the head post up to
the platform cap, spill to following posts, append image-only posts when images
outrun text. The 10-image gallery spec (2026-06-09) explicitly deferred manual
placement, noting "this feature builds its rails." This feature lets the
operator decide which part of the thread each image rides on.

### Why not in-text markers (the rejected first design)

The first iteration encoded placement as `⟦media N⟧` marker lines inside the
draft text. A three-agent critique killed it. The fatal pattern: putting the
anchor *inside the counted/translated/edited text* spawns independent collisions:
- **Translation wipes placement.** `handleTranslateDraft` (drafts.go:249) ships
  raw `master_text` to DeepL with no tag protection; `⟦media N⟧` comes back
  translated/reflowed → every anchor silently orphans.
- **Char-count parity.** The client counts the marker bytes; the server strips
  them before counting → the recurring counter/splitter parity-bug surface.
- **Edit-split sheet corruption.** `openThreadSheet` (compose.js:294) shows the
  marker as raw segment text; merge/break edits move or split it mid-line.

A **structured anchor map** (placement as data, keyed by attachment ordinal,
outside the prose) sidesteps all three by construction. The original "no new
persistence" argument for markers was also illusory — resume needed special
handling regardless (see Constraint 1). So the map's small persistence cost is
roughly what markers secretly required anyway, without the collisions.

### Constraints inherited from the architecture

1. **Resume/schedule-fire re-derive the plan, and today they front-fill from a
   bare count.** `resumeSegments` (dispatch.go:502) calls
   `thread.PlanMedia(len(imgs), len(segs), cap)` and gathers
   `imgs[start:start+count]` (dispatch.go:527) — pure contiguous front-fill, no
   placement input. `Schedule`→Fire (dispatch.go:1041) has the identical gap.
   Any placement must survive both re-derivations.
2. **Thread shape is per platform.** Bluesky (300) may make 4 posts where
   Mastodon (500) makes 2 and Nostr (unbounded) makes 1. **Parts (`---` count)
   are platform-stable; post counts are not** — so the addressable unit is the
   part, and the UI must not label it "post N".
3. **The whole pipeline assumes contiguous head-fill.** `plan []int` is per-post
   *counts*; dispatch slices `imgs[start:start+count]` (dispatch.go:422), the
   prefix-sum lives at dispatch.go:408-415/503-508, the preview does the
   identical `media.slice(...)` (preview.js:235), and `PlanBlueskyCard` tests
   `plan[target] > 0` (card.go:67). Anchoring breaks contiguity, so the plan
   generalizes from counts to explicit attachment indices.

## Scope

- Per-image control over which **part** (`---` block) an attachment rides, in
  both regimes; a single-block draft is one part (everything front-loads until a
  break is added).
- One shared placement; rare per-platform override via a lightweight per-platform
  **anchor delta** (no text fork).
- Zero-regression default: no anchors → posts byte-for-byte as today.

Out of scope (v1): placing an image on a sub-post of an auto-split part (insert a
break instead); video placement; placement in interaction/quote-reply mode
(disabled, like Edit-split); reordering attachments *by* placement; drag-and-drop
(tap to assign).

## Design

### 1. The anchor map (data model)

Placement is structured data keyed by **attachment ordinal** (the existing
stable-within-post index; `Media.Ordinal`, models.go:68 / `imageSpec.Ordinal`,
api.go:819). It is never written into `master_text`.

- **Shared anchors:** `anchors: map[ordinal]partIndex` on the post spec / draft.
  Applies to every platform. A missing entry → default part 0 (front-load).
- **Per-platform delta (rare):** `Overrides[p].Anchors: map[ordinal]partIndex`.
  When present for ordinal o, it replaces the shared entry for platform p only.
  No text fork — prose stays mastered.

`partIndex` is 0-based over the `---`-delimited blocks of the *effective* text
for that platform. **Re-validation:** an entry whose `partIndex >= nParts`
(operator deleted a `---`) clamps to the last part. An entry for an ordinal that
no longer exists is ignored. Both deterministic.

Keying by ordinal (not a stable id) means add/remove/reorder must update the map
— but that is a small, local structured-object edit (drop key, shift keys past
the removed ordinal), not the cross-text-body rewrite the marker design needed.
A stable per-attachment id is a noted future refinement if reorder churn hurts.

### 2. Resolution → the per-post plan (pure, internal/thread)

Anchored images are no longer a contiguous head-fill, so the per-post plan
generalizes from **counts** to **explicit attachment indices**:

```
// today:     plan []int     e.g. [2, 1, 0]        (counts per post)
// proposed:  plan [][]int   e.g. [[0,3],[1],[]]   (attachment indices, attach order)
```

`SplitWithMedia` gains the resolved per-image part assignment as input (the
caller resolves `Overrides[p].Anchors ?? anchors ?? 0` per image before calling)
and returns `[][]int`. Resolution, per platform:
1. Split text into parts by `---` (existing `splitMarkers`).
2. Auto-subdivide each part by length (existing `splitAt`).
3. For each part, gather the images assigned to it (in attach-ordinal order) and
   distribute across *its own* sub-posts via the existing `PlanMedia` rule
   (head sub-post up to cap, overflow to appended image-only posts).
4. The **head part** additionally absorbs every image with no/clamped assignment.

**Canonical ordering (golden-tested, because resume depends on it):** a part's
image set = {images assigned to it} ∪ {unassigned, for the head part only},
sorted ascending by attach ordinal, then cap-overflowed. This makes the `[][]int`
a deterministic function of (text, anchors, platform).

**Numbering fixpoint.** Total posts = Σ over parts of (text sub-posts +
image-only overflow). The existing `number()`/`SplitWithMedia` fixpoint
(thread.go:289, 125-132) currently drives toward a single global
`extra = ceil(nImages/cap)`; it must be generalized to the per-part sum and
re-proven to converge with image-only posts that can now appear **mid-chain**
(not only as a tail). The `len(segs) == len(plan)` invariant that every caller
relies on must be preserved. *This convergence proof and the marker-only-part /
image-only-post representation are the riskiest implementation items and the
plan must treat them as their own task with explicit tests, not a drop-in.*

### 3. Dispatch (internal/dispatch)

- `runChain` (dispatch.go:417-435): replace the `starts[]` prefix-sum
  (408-415) and `imgs[start:start+count]` slice with a gather by index list,
  `pick(imgs, plan[i])`. The prefix-sum block becomes dead code — remove it.
- **Nostr imeta** (dispatch.go:424, and resume 530): today `if i == 0 { segImetas
  = imetas }` hard-attaches all imeta to the head ("nostr never splits media").
  Under per-part placement nostr *can* have media on a non-head part, so imeta
  must be distributed per post in step with `plan[i]` (each nostr post's imeta =
  the imeta for the images that post carries).
- **Resume** (`resumeSegments`, dispatch.go:471-552): stop deriving from
  `PlanMedia(count)`. Re-resolve `[][]int` from **`tg.FinalText` + the persisted
  anchors** (see §5) via `SplitWithMedia`, then gather by index. Remove the
  prefix-sum (503-508); redesign the "trailing images exceed segments"
  drop-warning (511-518) — summing `plan[len(segs):]` counts is meaningless for
  index lists.
- **Schedule→Fire** (dispatch.go:1009/1041): persist anchors in `FieldsJSON`
  (§5); Fire re-resolves from `FinalText` + anchors, same as resume.

### 4. Bluesky card (internal/dispatch/card.go)

Today `PlanBlueskyCard` (card.go:53-72) reverts the card entirely when its target
segment already holds images (`plan[target] > 0`, card.go:67). Two real changes:
- The predicate becomes `len(plan[target]) > 0` under `[][]int`.
- When an image is *deliberately* anchored onto the card's post, the card wins
  its embed slot and those images **spill to an appended image-only post in that
  part** (rather than the card silently reverting). This requires
  `PlanBlueskyCard` to insert a post and re-thread the `[][]int`/`Card.Segment`
  index — a genuine planner change, called out as its own task.

### 5. Persistence (no SQL migration)

- **Draft:** `anchors` (and per-platform `Overrides[p].Anchors`) live in the
  opaque `spec_json` blob (drafts.go:21) → save/load/autosave round-trip for
  free. `handleTranslateDraft` must copy `anchors` onto the translated draft it
  builds (drafts.go:261-279) — a field copy; markers-in-text mangling is gone.
- **Dispatched/scheduled target:** anchors ride `Target.FieldsJSON`
  (models.go:42) through the existing override-rehydration path (dispatch.go:985
  unmarshals `FieldsJSON` into `Overrides`). Add `Anchors` to `dispatch.Overrides`.
- **Resume input:** `tg.FinalText` (the per-platform final text, already
  persisted) + `FieldsJSON` anchors → deterministic `[][]int`. No `store.Segment`
  field added.

### 6. API (internal/api)

- `POST /api/thread-preview` (api.go:1527): request gains `anchors` (and resolves
  per-platform deltas); `Imgs []int` → `Imgs [][]int`. Same single source of
  truth as dispatch — preview and posting cannot diverge.
- `handleAPIPost` / `postSpecJSON` (api.go:825): add `anchors`; thread it into
  `dispatch.PostSpec`. `assembleImages` unchanged (ordinals already exist).

### 7. Web UI (internal/web/assets)

- **Placement chip per thumbnail** (`renderImages`, compose.js:860): a
  touch-friendly `▸ part 1`, `▸ part 2`, … picker whose options are the current
  `---` parts. **Parts are platform-stable, so "part N" is truthful and
  platform-neutral** (this is why the labeling works where "post N" could not).
  Picking a part writes the shared `anchors` entry. Default = part 1, so an
  untouched draft is unchanged. Single-part draft: the chip is active with a
  hint "Pin to a post — needs a break" and a one-tap "Add a break" that drops a
  `---` via the existing Edit-split machinery.
- **Map hygiene** on add/remove/reorder: update the `anchors` object in
  `state` (drop/shift ordinal keys). Small and local — no text rewrite.
- **Per-platform override (rare)** lives in the per-platform split sheet
  (`openThreadSheet`, compose.js:282), which already shows that platform's real
  posts: an "override placement for {platform}" affordance writes
  `Overrides[p].Anchors`. This is the only per-platform surface; the chip covers
  the 95% shared case.
- **Live preview is the WYSIWYG feedback** (preview.js:235):
  `media.slice(...)` → `pv.imgs[i].map(ix => media[ix])`. Each platform's preview
  renders images on their assigned posts, updating as chips change.
- **Client estimator** (`threadInfoFor`, compose.js:113-120): today
  `Math.ceil(previewMedia(p).length / mmax)` is a *global* front-load count — it
  must be **rewritten** to the per-part sum so the live thread badge matches the
  server. Char counting, `splitMarkers`, and `bskyCardText` are **untouched**
  (anchors aren't in the text) — the parity surface shrinks to this one estimator.

### 8. Explicitly unchanged

Store schema (SQL), retrier, progress/SSE, verifier, video gate/transcoding, the
one-video-or-images attach guard, Blossom upload, char-counting, DeepL request
shape.

## Error handling

| Case | Behavior |
|---|---|
| **Part over platform cap** (6 images on one part, Mastodon cap 4) | Fill the part's sub-posts up to cap, overflow to appended image-only posts; numbering fixpoint counts them. |
| **Image-only part** (a `---` block with an image anchored, little/no text) | Image-only post (empty text, or bare counter when numbering on) — must be representable *mid-chain*, a new case for `SplitWithMedia` (§2). |
| **Anchor out of range / orphan ordinal** | Clamp to last part / ignore; soft preview note, never a hard error. |
| **Image anchored to Bluesky card's post** | Card wins; images spill to an appended image-only post in that part (§4). |
| **Nostr (cap 0)** | Each part's images become URLs/imeta on that part's post; imeta distributed per post (§3), not all on head. |
| **Interaction/quote-reply mode** | Placement disabled; anchors not applied; the placement chip is hidden (consistent with Edit-split being disabled there). Source images re-hosted after the operator's images (capMedia, dispatch.go:781) therefore never collide with anchors. |

## Testing

TDD throughout, matching the table-driven `thread`/`dispatch` tests:

- **thread (core):** anchor resolution → `[][]int`; canonical head-part ordering
  (assigned ∪ unassigned, by ordinal); per-part overflow; **mid-chain image-only
  posts**; numbering fixpoint convergence with per-part sums (worst-case bound);
  `len(segs)==len(plan)` invariant; clamp/orphan; Nostr cap-0 per-part.
- **Determinism:** same (text, anchors, platform) → identical `[][]int` across
  repeated calls; **resume re-derivation from `FinalText`+anchors equals the
  original run's plan** (the bug the critique caught — explicit regression test).
- **Regression golden test:** no anchors → plan byte-identical to today.
- **dispatch:** per-post gather; nostr per-post imeta; `resumeSegments` and
  Schedule→Fire reproduce placement; prefix-sum removal.
- **card:** `len(plan[target])` predicate; card-vs-anchored-image spill.
- **api/preview parity:** `Imgs [][]int` equals dispatch; client `threadInfoFor`
  per-part estimate matches the server thread count.
- **drafts:** `anchors` round-trips save/load/autosave; **translate copies
  anchors** (no placement loss).
- **Manual e2e on Oppy:** a 3-part thread with an image anchored to part 2 across
  all four platforms; confirm placement, numbering, and resume after a forced
  mid-chain stop.

## Risks

- **Numbering fixpoint with mid-chain image-only posts (§2)** is the highest
  implementation risk: the convergence argument and the `len(segs)==len(plan)`
  invariant must be re-established, not assumed. Treat as a standalone task.
- **Bluesky card spill (§4)** is a real `PlanBlueskyCard` change; if it proves
  costly, the v1 fallback is "card wins, anchored image on its post falls back to
  front-load" with a preview warning.
- **Per-part placement, not per-post** is a deliberate v1 limitation: an image on
  a sub-post of an auto-split part requires inserting a break. Labeled honestly
  ("part N") so the operator is never misled.
- **Ordinal-keyed anchors** require the UI to maintain the map on
  remove/reorder; a stable per-attachment id is the fallback if this churns.
