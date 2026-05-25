# Interaction Compose Mode — Design

**Status:** approved design, ready for implementation planning.
**Branch:** `feature/interaction-compose`.
**Date:** 2026-05-25.

## Goal

Make replying to / quoting an existing post a first-class composing experience: instead of a minimal inline box in the Interact tab, **Reply and Quote hand off to the full Compose tab** ("interaction mode") — with the live per-platform preview, auto-threading/splitting, media, and overrides. Fan-out posts (the action shared to platforms where the original isn't native) **reproduce the original's text + media + link**, not just a bare link.

This supersedes the v0.5.x inline Interact action panel. Repost stays a one-click action in the Interact tab (no text, no preview needed).

## Approved decisions

1. **Scope:** Reply and Quote both open Compose in interaction mode (preview + split + media). Repost stays a one-click in Interact.
2. **Architecture:** an *interaction mode* inside the existing Compose tab, reusing its composer (`state.js`/`compose.js`/`preview.js`) — not a second composer.
3. **Unified platform model:** Reply and Quote differ only in the *source-platform* action; the fan-out half is identical.
4. **Media:** full support. Compose's image picker is active; native quote carries media where the platform allows; fan-out reproduces the original's media (re-hosted).
5. **Fan-out content:** reproduce the original's **text + media + source link** (richest), assembled into a normal post then threaded.

## Flow & interaction mode

- **Interact tab** is the entry point: paste → `/api/resolve` → preview card with **Reply / Repost / Quote**.
  - **Repost:** one-click in Interact (unchanged from v0.5.0 — `runAction` repost).
  - **Reply / Quote:** set `state.interaction` and switch to the **Compose tab**.
- **Compose interaction mode** (`state.interaction != null`):
  - A **source banner** at the top of Compose: the resolved preview (platform badge, author, text snippet, media thumbnails, link to original) + a header (*"Replying to @x"* / *"Quoting @x"*) + an **×** to exit back to a normal new post. If the action was capability-blocked, the banner shows the reason + a **"try anyway"** toggle (sets `force`).
  - The master textarea is your **commentary**; the existing live preview, auto-split, `k/n` toggle, media picker, and per-platform overrides all apply.
  - **Scheduling is hidden** in interaction mode (interactions post immediately) — deferred to a future iteration.
  - **Send** → `POST /api/interact` (multipart) → the usual result modal.
- **State:** `state.interaction = { action, platform, ref, sourcePreview:{author_handle,text,media,web_url}, caps, force } | null`. Entering interaction mode resets the composer to a fresh commentary; exiting (×) clears `state.interaction` and restores normal compose.

## Unified platform-selection model

Both Reply and Quote:
- **Source platform** — always-on and **locked**: the native action (a **reply** for Reply; an **embed quote** for Quote).
- **Other platforms** — optional toggles: a **fan-out reproduction** (your commentary + the reproduced original + link). For a fanned-out *reply* this reads as "quoting the original with your take" on the other platform (the best cross-platform semantics; labeled so it's not surprising).

The platform chips visually distinguish "source — native" from "others — fan-out". The preview + split run for whichever platforms are active. (A post's stored `interaction.action` is reply/quote; its targets may mix one native action + several fan-out reproductions — that's expected.)

## Threading model (the "split")

Commentary over a platform's limit auto-threads via the existing splitter/`runChain`, exactly like normal Compose, with **the head segment carrying the action**:
- **Reply (source):** seg 1 replies to the original; seg 2…n thread under seg 1.
- **Quote (source):** seg 1 is the native quote (embeds the original); seg 2…n reply under seg 1.
- **Fan-out (any platform, either action):** the assembled reproduction post (see below) is a normal thread; the original's link rides the head segment.

The live preview shows the per-platform segment chain (reusing thread-preview); the `k/n` numbering toggle applies.

## Content assembly

A fan-out post is **assembled into a normal post up front, then run through `runChain`** — so threading, media-on-head, and numbering are reused unchanged.

**Fan-out post (per other platform):**
- **text** = `<your commentary>` + an attributed block: `— @author: <original text>` + the source URL (the `njump.me` URL for a Nostr original).
- **media** = the original's images (re-hosted) **+** any images you attached, **capped at the platform max** (your own images first, then the original's, truncated to 4 for Bluesky/Mastodon; Nostr has no fixed cap).
- Then `runChain` splits it if over the limit (media on the head segment).

**Source platform (native action):** commentary (threaded) + **your own media only** — the original is natively embedded (quote) or replied-to (reply), so it is not reproduced.

**Media re-host:** a dispatch helper downloads each original media URL (via the existing `dispatch.Fetcher`), runs it through the media pipeline (Blossom upload + dims/blurhash for imeta), and attaches the result. Alt text is carried from the source preview when present.

## Backend (API + dispatch)

- **`/api/resolve`:** unchanged — it already returns `preview.{text, media, author_handle, web_url}`. The frontend passes that `sourcePreview` back in the interact spec so dispatch reproduces **without re-resolving**.
- **`POST /api/interact`:** becomes **multipart** (a `spec` JSON field + `image` files), mirroring `/api/post`. The spec gains: `number` (k/n toggle), `overrides` (per-platform), `source_preview` (author/text/media/url for reproduction), and the existing `action`/`platform`/`ref`/`fanout`/`force`.
- **`dispatch.Interact`** is reworked from "one target per platform" to **one `runChain` per platform**:
  - Source platform: `runChain` seeded so the **head segment** performs the native action (reply-ref for reply; quote-embed for quote), tail segments thread as replies. Head carries the user's media.
  - Fan-out platforms: assemble the reproduction text + media (re-hosting original media), then `runChain` (normal thread).
  - **Mastodon quote + media:** native quote can't carry media → that platform is handled by the fan-out reproduction path instead (commentary + reproduced original + link + media as a normal post).
  - Each target keeps its `store.Target.Segments` chain (reused). The `store.Post.Interaction` descriptor is unchanged; history already renders chains + the interaction badge.
- **Threading the native action:** `runChain` today posts a fresh chain (head = a plain post). It gains a way to make the head a reply (reply-ref) or a quote (quote-ref) while the tail are plain replies — i.e. an optional "head action" parameter. (Reuses `runPlatform`/`runAction` per segment.)

## What changes vs the shipped v0.5.x

- **Interact tab:** Reply/Quote hand off to Compose interaction mode; the v0.5.x inline action panel (and its `.act-*` styles, the quote modal, the active-highlight) is **removed/superseded**. Repost's one-click stays.
- **`/api/interact`:** JSON → multipart; gains threading, reproduction, media re-host. `dispatch.Interact` reworked.
- **`/api/resolve`, `store` interaction descriptor, history rendering:** unchanged.
- The restriction **override** moves from the inline panel into the Compose source banner.

## Error handling

- Media re-host failure (a source image 404s / too large) → skip that image, keep posting (best-effort; surface a warning on the target, not a hard fail).
- A fan-out platform not configured → that target fails like any other (partial post status), the source action still posts.
- Over-limit with numbering: same fixpoint behavior as the threading feature.

## Testing

- **dispatch:** `Interact` threads (reply-chain; quote-then-replies; fan-out chain); fan-out **reproduces text + re-hosts media** (fake `Fetcher` + fake media pipeline); **Mastodon quote+media degrades** to fan-out reproduction; the **media cap** (user-first, truncate to platform max); head-action seeding of `runChain`.
- **api:** `/api/interact` multipart parse (spec + images) → forwards `InteractSpec` incl. `source_preview`/`number`/`overrides`; bad-action/empty-platform 400s.
- **web:** interaction-mode state transitions (enter from Interact, ×-exit), constrained platform chips (reply=source-locked, quote=source-locked+fanout), source banner render, preview/split reuse; `node --check`.
- Live posting verified manually with real creds, as with prior features.

## Decomposition (for writing-plans)

Two plans, each independently testable:
- **Plan 1 — backend:** `runChain` head-action seeding; `dispatch.Interact` rework (per-platform chains, fan-out reproduction assembly, media re-host helper, Mastodon-quote+media degrade); `POST /api/interact` multipart + `source_preview`/`number`. Unit-tested with fakes.
- **Plan 2 — frontend:** Compose interaction mode (`state.interaction`, source banner, constrained platform chips, hidden scheduling, multipart Send to `/api/interact`); Interact tab hand-off (Reply/Quote → Compose; Repost stays inline); remove the v0.5.x inline action panel.

## Out of scope (v1)

- Scheduling an interaction (posts immediately).
- Editing/deleting interactions from the UI.
- Threads as a *source* (still preview-excluded; remains a valid fan-out target).
- Quote-of-a-quote chains beyond one level.
