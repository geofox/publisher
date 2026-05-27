# Drafts — Design

**Status:** approved design, ready for implementation planning.
**Branch:** `feature/drafts`.
**Date:** 2026-05-27.

## Goal

Add a persistent scratchpad / drafts area to Publisher so the owner can prepare
content before publishing — save work-in-progress, tag and filter it, translate
drafts, and reference published content (via the existing Interact context).
Drafts live in the Compose tab as a left sidebar; saving is explicit; publishing
consumes the draft.

## Approved decisions

1. **Draft shape — full Compose state including media.** A draft persists the
   same spec the Compose tab already builds (`master_text`, per-platform
   `overrides`, `interaction` context, attached `images`). Media is stored as
   references to Blossom (the same content-addressed protocol the publish flow
   already uses); the DB holds blossom_url + sha256 + metadata, not blobs.
2. **Lifecycle — publish consumes.** Publishing converts the draft into a post
   and deletes the draft row + media rows in the same transaction. There is no
   "archive" relationship between drafts and posts.
3. **Distinct from scheduled posts.** Scheduled posts continue to live in
   `posts` with `status='scheduled'`. Drafts never appear in the scheduler loop.
   To schedule from a draft, load it in Compose, set the schedule field,
   publish — the post is created (`status='scheduled'`) and the draft is
   consumed.
4. **Referencing published content == reuse Compose's interaction field.**
   No new schema or UI for "reference a post"; drafts simply persist the
   existing `interaction` field used by the Interact tab (quote/reply/repost
   target).
5. **Save model — manual save + invisible recovery autosave.** Explicit
   "Save draft" (button or Ctrl+S) is the only way to create or update a draft.
   Compose additionally writes an autosave snapshot to `localStorage` for crash
   recovery; that slot is only surfaced via a banner when it contains content
   and no draft is currently active.
6. **Tags — free-form, lowercase, filterable.** Free-form strings, normalized
   server-side. A tag chip row above the sidebar list filters by tag (multiple
   tags = AND).
7. **UI placement — sidebar inside the Compose tab.** No new top-level tab.
   Compose grows a collapsible left sidebar; on mobile the sidebar is hidden
   behind a toggle.

## Out of scope (MVP)

- Tag autocomplete
- Bulk operations (multi-select delete/tag)
- Sort options beyond `updated_at DESC`
- Archive / history of past drafts
- Sharing drafts across devices in real time (single-user app; recovery is
  localStorage)

## Architecture

### Backend

New files:
- `internal/store/drafts.go` — `Draft` model, CRUD, filter by `q`/`tag`,
  pagination, transactional delete (cascades to `draft_media`).
- Two new tables appended to the existing idempotent migration block in
  `internal/store/store.go`: `drafts`, `draft_media`.
- `internal/api/api.go` — new handlers wired to the existing mux:
  `GET/POST/PUT/DELETE /api/drafts(/{id})`,
  `POST /api/drafts/{id}/translate`. All sit behind the existing
  `withCSRFGuard`.

`/api/post` (existing handler at `api.go:461`) gains one optional field in its
spec — `draft_id` — and on success deletes that draft inside the same
transaction.

The Blossom upload helper used by `/api/post` is extracted (or shared) so
`/api/drafts` reuses identical image handling. If the helper does not already
short-circuit on a known sha256, the implementation plan should add that —
otherwise every draft save re-uploads every attached image.

### Frontend

New file:
- `internal/web/assets/drafts.js` — sidebar state, list rendering, filter chips,
  load / save / delete actions, recovery banner.

Small extensions:
- `state.js` — track `state.activeDraftId`, a `dirty` flag, and an autosave
  hook that writes the current state to `localStorage` on every change.
- `compose.js` — toolbar buttons (Save, Translate ▾, Delete), save-status
  indicator, generalized `loadDraft(spec)` accepting a full spec rather than
  `{text, lang}`, publish flow wired to include `draft_id` and to clear
  `activeDraftId` on success.
- `index.html` — add the sidebar markup inside the Compose section; add a
  mobile toggle.

## Data model

### `drafts` table

```sql
CREATE TABLE IF NOT EXISTS drafts (
  id           TEXT PRIMARY KEY,            -- ULID, like posts
  created_at   TIMESTAMP NOT NULL,
  updated_at   TIMESTAMP NOT NULL,
  title        TEXT NOT NULL DEFAULT '',    -- derived from first line of master_text if empty
  master_text  TEXT NOT NULL DEFAULT '',    -- denormalized for list preview + LIKE search
  tags_json    TEXT NOT NULL DEFAULT '[]',  -- normalized JSON array of lowercase strings
  spec_json    TEXT NOT NULL                -- full Compose spec (see below)
);
CREATE INDEX IF NOT EXISTS idx_drafts_updated_at ON drafts(updated_at DESC);
```

`master_text` is lifted out of `spec_json` because list previews and search need
it without parsing JSON. `title` is lifted for the same reason; if empty, the
list rendering shows "(untitled)".

### `spec_json` shape

Matches what `state.js:buildSpec()` already produces (with the schedule field
omitted — drafts are not scheduled):

```json
{
  "master_text": "...",
  "platforms": ["nostr", "bluesky", "mastodon"],
  "overrides": {
    "bluesky": { "text": "...", "lang": "en", "visibility": "public" }
  },
  "interaction": { "kind": "quote", "ref": "at://..." }
}
```

`interaction` is `null` when no quote/reply/repost context is attached.

### `draft_media` table

```sql
CREATE TABLE IF NOT EXISTS draft_media (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  draft_id    TEXT NOT NULL REFERENCES drafts(id) ON DELETE CASCADE,
  ordinal     INTEGER NOT NULL,
  blossom_url TEXT NOT NULL,
  sha256      TEXT NOT NULL,
  mime        TEXT,
  dim         TEXT,
  blurhash    TEXT,
  size_bytes  INTEGER,
  alt         TEXT
);
CREATE INDEX IF NOT EXISTS idx_draft_media_draft_id ON draft_media(draft_id);
```

Same shape as the existing `media` table at `internal/store/store.go:120`. Kept
separate so refactors to either don't entangle drafts and posts; a shared row
type can be extracted later if and when it pays off.

### Tag normalization (server-side)

- Lowercase, trim whitespace, strip leading `#`
- Reject empty (after trim)
- Cap at 32 chars
- Deduplicate
- Stored as a JSON array; filtering uses `LIKE '%"<tag>"%'` over `tags_json`
  (fine at single-user scale of hundreds of drafts)

If filtering ever becomes a bottleneck, an auxiliary `draft_tags(draft_id, tag)`
table can be added without changing the public API.

## API

### Endpoints

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/drafts?q=&tag=&tag=&limit=&offset=` | List. `q` matches `master_text` (LIKE); `tag` may be specified multiple times → AND semantics; `limit` default 50, `offset` default 0 (mirrors `/api/posts`). Returns a JSON array; each item: `{id, title, tags[], updated_at, preview, first_media_url?}` — not the full spec. |
| `GET` | `/api/drafts/{id}` | Full draft: `spec_json` + media rows (`blossom_url`, `ordinal`, `alt`, …). |
| `POST` | `/api/drafts` | Create. Multipart: `spec` (JSON) + image files. Returns the saved draft. |
| `PUT` | `/api/drafts/{id}` | Update. Same multipart shape; full-replace semantics for spec, tags, and media set. |
| `DELETE` | `/api/drafts/{id}` | Cascade-delete row + draft_media. Returns 204. |
| `POST` | `/api/drafts/{id}/translate` | Body: `{"target":"de"}`. Translates `master_text` via DeepL; creates and returns a **new** draft. Original is untouched. |

All behind `withCSRFGuard`. No new auth surface.

### Multipart shape (POST/PUT)

```
spec   = JSON (see below)
img_0  = (file)    # only present for newly-attached images
img_1  = (file)
...
```

The `spec.images` array uses a dual entry shape:
- `{ "ordinal": 0, "ref": "img_0", "alt": "..." }` — newly uploaded; backend
  reads multipart field `img_0`, hashes, uploads to Blossom if sha256 is new,
  then inserts a `draft_media` row.
- `{ "ordinal": 1, "blossom_url": "...", "sha256": "...", "mime": "...",
  "alt": "..." }` — already uploaded (preserved from a prior load); backend
  skips upload and inserts the row as-is.

This is the same dual shape `/api/post` already needs for its multipart, so the
same helper handles both.

### Translate endpoint flow

`POST /api/drafts/{id}/translate {"target":"de"}`:
1. Load draft; read `master_text`.
2. Call `translate.DeepL.Translate(ctx, text, "de")`
   (`internal/translate/translate.go:91`).
3. Build a new spec: copy original's `platforms`, `interaction`, and `tags`;
   clear `overrides` (they'll be re-derived in the editor); replace
   `master_text` with the translation; copy media references (blossom_url
   rows — no re-upload, content-addressed).
4. Insert as a new draft; return it.
5. Frontend loads the new draft into Compose.

### `/api/post` consume hook

`/api/post` spec gains:
```json
{ "...existing fields...": "...", "draft_id": "01HQ..." }
```

On the 2xx success path (post row + targets created), the handler deletes the
referenced draft (and its media rows via cascade) inside the same transaction.
On any failure, the draft survives so the user can retry.

If multi-platform delivery later fails per-target, the draft is already gone —
that is the "consume" contract; retries happen from History.

## Frontend behavior

### Compose layout changes

A new collapsible left sidebar inside the Compose tab:

- **Header:** "Drafts" label, "+ New" button.
- **Search box:** filters list by `master_text` LIKE.
- **Tag chip row:** all tags in use; active tags highlighted with ✕; clicking
  toggles.
- **Draft rows:** title (or "(untitled)"), one-line preview of master_text,
  tags inline, relative timestamp ("2h", "yesterday", "3d"). The active draft
  is outlined.
- **Mobile:** sidebar hidden by default; "📋 Drafts" toggle in the toolbar opens
  it as a full-width overlay.

The editor pane gains:

- **Toolbar buttons:** Save (💾), Translate ▾, Delete (🗑 — active draft only).
- **Save-status indicator:** small dot + text in the toolbar — "● saved 4s ago"
  / "● unsaved changes" / "● saving…" / "● save failed — retry".
- **Tags input:** chip-style input below the master text.
- **Recovery banner:** shown only when localStorage has a recovery snapshot and
  no active draft is loaded. Two buttons: Restore (loads into Compose),
  Discard (clears localStorage).

### State model

- `state.activeDraftId` — `null` (no draft loaded) or the loaded draft's id.
- `state.dirty` — true when the in-memory spec differs from the last successful
  save.
- On every state change, the latest spec is written to
  `localStorage.compose_recovery`. The save flow on success clears it.
- Loading a draft from the sidebar: GET `/api/drafts/{id}` → call generalized
  `loadDraft(spec)` → set `state.activeDraftId = id`, `state.dirty = false`.
- "+ New" or switching drafts with `state.dirty === true`: three-button confirm
  dialog — **Save & continue** (saves current draft, then performs the switch),
  **Discard & continue** (drops in-memory changes, then performs the switch),
  **Cancel** (no-op). No silent autosave to the drafts table.
- Ctrl/Cmd+S — save the active draft (or create if `activeDraftId` is null).

### Publish-consume

Publish includes `spec.draft_id` when `state.activeDraftId` is set. On 2xx:
clear `state.activeDraftId`, refresh the sidebar list, reset
`state.dirty = false`. The recovery snapshot is also cleared.

### Translate-to-new-draft

Translate ▾ shows the languages from `config.UserLanguages` (intersected with
DeepL's supported targets — already done by the existing
`/api/translate` handler). Selecting a target POSTs to
`/api/drafts/{id}/translate`, then loads the returned new draft into Compose.

## Error handling

| Case | Behavior |
|---|---|
| Save fails (network/5xx) | Editor state preserved; status shows "● save failed — retry"; localStorage still holds the recovery snapshot. No data loss. |
| Concurrent deletion (PUT 404) | Frontend treats `activeDraftId` as stale; banner: "this draft was deleted — save as new?"; clicking saves via POST and refreshes the list. |
| Blossom upload fails during save | Whole save fails atomically (Blossom uploads run before opening the SQL tx, so no partial rows exist). Toolbar surfaces the error. Already-uploaded images are content-addressed → retry is cheap. |
| Translate API fails | No new draft created; toolbar shows "translation failed: \<reason\>"; original draft untouched. |
| Publish fails (`/api/post` 4xx/5xx) | Draft survives (consume only on 2xx). |
| Publish succeeds, per-platform delivery fails later | Draft is gone (consume contract); user retries from History. |
| Stored `spec_json` cannot be parsed (schema drift) | GET returns the row with `spec = {}`; frontend banner: "draft format unrecognized — open as text-only or delete." |
| localStorage quota exceeded | Recovery autosave silently no-ops; console.warn; main save unaffected. |
| Tag normalization rejects an input | Backend strips/normalizes and silently drops empty entries; frontend pre-normalizes for live preview. |
| Empty `master_text` and no title | List shows "(untitled)" in muted style. |
| Image referenced in `spec.images` but no matching `img_N` multipart field | Save returns 400 with the missing field name. |

## Testing

### Go unit tests (mirror `internal/store/posts_filter_test.go`)

- `drafts_crud_test.go` — Create / Get / Update / Delete round-trip; cascade
  delete removes `draft_media`; `updated_at` advances on Update.
- `drafts_filter_test.go` — list with `q`, list with `tag`, combined filters,
  pagination, empty results.
- `drafts_tags_test.go` — normalization (case, whitespace, `#`-strip, dedup,
  length cap, empty rejection).

### Go integration tests (handler-level)

- POST `/api/drafts` happy path with one new image multipart; assert draft +
  draft_media rows + Blossom call.
- POST with an image whose sha256 is already in Blossom — assert no re-upload.
- PUT replacing media set (add one, remove one, keep one).
- DELETE → 204 → GET → 404.
- POST `/api/drafts/{id}/translate` happy path; DeepL error path.
- POST `/api/post` with `draft_id` → assert post created + draft deleted in same
  tx; with a forced 4xx → assert draft survives.

### Frontend (manual smoke)

- Save / Load / Delete cycle.
- Tag chip filter toggles (AND semantics with multiple).
- Recovery banner appears after refresh with unsaved content.
- Confirm dialog on switching drafts with unsaved changes.
- Ctrl/Cmd+S triggers save.
- Mobile: sidebar toggle opens/closes overlay; rows are tappable.

## Build sequence (suggested for the implementation plan)

1. Store layer — schema migration + `drafts.go` + unit tests.
2. API layer — handlers + integration tests (without the `/api/post` consume
   hook yet).
3. `/api/post` consume hook (additive `draft_id` field).
4. Frontend state + sidebar UI + Save/Load/Delete.
5. Tag chips + filter.
6. Translate-to-new-draft action.
7. Recovery autosave + banner.
8. Mobile sidebar toggle.
9. Manual smoke pass; release notes.
