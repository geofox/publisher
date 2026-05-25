# Interaction Compose Mode — Plan 2: Frontend Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.
>
> **Note on testing:** this repo has no JS unit-test runner. Frontend tasks are verified with `node --check <file>` (syntax), `go build ./cmd/publisher` (assets embed), `go test ./internal/web/`, and a manual pass in Task 5 — consistent with all prior frontend work here. Keep every change small and re-read the helper signatures before editing.

**Goal:** Reply/Quote in the Interact tab hand off to the **Compose tab in interaction mode** — a source banner + constrained platform chips + the full live preview/split/media — and **Send** posts to the multipart `/api/interact`. Repost stays a one-click action in Interact.

**Architecture:** A new `state.interaction` flag drives Compose's interaction mode. `compose.js` owns the mode (banner, constrained chips, hidden scheduling, `doInteract` multipart Send) and exposes `startInteraction(src, action)` / `exitInteraction()`. `interact.js` calls `startInteraction` (no import cycle: `interact.js`→`compose.js` is acyclic) and keeps Repost inline. The v0.5.x inline action panel is removed.

**Tech Stack:** vanilla-JS SPA (`state.js`/`compose.js`/`preview.js`/`interact.js`/`main.js`), Go-embedded assets.

**Spec:** `docs/superpowers/specs/2026-05-25-interaction-compose-mode-design.md` (§Flow, §Unified platform model, §What changes). The backend (multipart `/api/interact`, threading, reproduction) is **Plan 1 — already done**: the `/api/interact` spec shape is `{ action, platform, ref, source_url, source_author, source_preview:{author,text,media:[{url,alt}]}, text, overrides, fanout, number, force, images:[{alt}] }` as a `spec` multipart field + `image` files.

**Builds on:** `/api/resolve` returns `SourceRef{platform, ref, preview:{author_name,author_handle,text,media:[{url,alt}],web_url}, caps}` (Plan A). `compose.js` `doPost`/`showResultModal`/`renderChips`/`buildSpec` patterns.

---

## File Structure

| File | Responsibility (Plan 2) |
|---|---|
| `internal/web/assets/state.js` | `state.interaction` field; `buildInteractSpec()` |
| `internal/web/assets/compose.js` | `startInteraction`/`exitInteraction`; source banner; interaction-mode chips; `doInteract`; submit routing; hide scheduling |
| `internal/web/assets/interact.js` | Reply/Quote → `startInteraction`; Repost one-click; remove inline action panel |
| `internal/web/assets/index.html` | `#srcbanner` container at the top of the compose section |
| `internal/web/assets/app.css` | source-banner styles; remove dead `.act-*` panel styles |

---

## Task 1: `state.interaction` + `buildInteractSpec`

**Files:** Modify `internal/web/assets/state.js`.

- [ ] **Step 1: Read** `state.js` — the `state` object (`master`, `platforms` Set, `ov`, `images`, `focus`), `effectiveText`, and `buildSpec` (note how it assembles `overrides` per platform and reads `#threadnum`/`#schedat`).

- [ ] **Step 2: Add `state.interaction`** — add the field to the `state` object literal:
```js
export const state = {
  master: "",
  platforms: new Set(ORDER),
  ov: {},
  images: [],
  focus: "bluesky", // platform shown in the live preview
  interaction: null, // null = normal compose; else {action, platform, ref, sourcePreview, sourceURL, sourceAuthor, caps, force}
};
```

- [ ] **Step 3: Add `buildInteractSpec`** (after `buildSpec`) — assembles the `/api/interact` spec from interaction state. Fan-out = the selected platforms other than the source; overrides are gathered for the source + fan-out platforms (reuse the same per-platform override shape `buildSpec` builds — factor the per-platform override object into a shared `ovFor(p)` helper if `buildSpec` doesn't already have one; otherwise inline the same fields):
```js
// buildInteractSpec assembles the /api/interact spec (reply/quote) from the
// interaction state: the source platform is the locked native target; the other
// selected platforms are fan-out reproductions.
export function buildInteractSpec() {
  const it = state.interaction;
  const fanout = [...state.platforms].filter((p) => p !== it.platform);
  const overrides = {};
  for (const p of state.platforms) overrides[p] = ovFor(p);
  return {
    action: it.action,
    platform: it.platform,
    ref: it.ref,
    source_url: it.sourceURL,
    source_author: it.sourceAuthor,
    source_preview: {
      author: it.sourceAuthor,
      text: (it.sourcePreview && it.sourcePreview.text) || "",
      media: ((it.sourcePreview && it.sourcePreview.media) || []).map((m) => ({ url: m.url, alt: m.alt || "" })),
    },
    text: state.master,
    overrides,
    fanout,
    number: document.getElementById("threadnum")?.checked ?? true,
    force: !!it.force,
    images: state.images.map((i) => ({ alt: i.alt })),
  };
}
```
If `buildSpec` builds its `overrides` inline (not via a helper), extract that per-platform object into:
```js
// ovFor returns the API override object for one platform (shared by buildSpec
// and buildInteractSpec).
function ovFor(p) {
  const ov = state.ov[p], o = {};
  if (ov.text != null) o.text = ov.text;
  if (p === "bluesky")  { o.langs = ov.langs.split(",").map((s) => s.trim()).filter(Boolean); o.bluesky_reply = ov.reply; o.bluesky_disable_quotes = ov.disable_quotes; }
  if (p === "mastodon") { o.spoiler_text = ov.spoiler_text; o.sensitive = ov.sensitive; o.visibility = ov.visibility; o.language = ov.language; }
  if (p === "threads")  { o.topic_tag = ov.topic_tag; o.threads_reply_control = ov.reply_control; }
  if (p === "nostr")    { o.pow = ov.pow; o.content_warning = ov.content_warning; }
  return o;
}
```
and have `buildSpec` call `ovFor(p)` in its `for (const p of state.platforms)` loop (replacing its inline object). Verify the field names against the CURRENT `buildSpec` body and match them exactly (do not change the wire shape `/api/post` expects).

- [ ] **Step 4: Verify** — `node --check internal/web/assets/state.js` (clean) and `go build ./cmd/publisher` (clean).

- [ ] **Step 5: Commit**
```bash
git add internal/web/assets/state.js
git commit -m "web/state: interaction mode flag + buildInteractSpec"
```

---

## Task 2: source banner + constrained chips + hidden scheduling

**Files:** Modify `internal/web/assets/index.html`, `internal/web/assets/compose.js`, `internal/web/assets/app.css`.

- [ ] **Step 1: Read** the compose `<section id="compose">` in `index.html` (find the top of it and the `#chips`/`#schedat` elements) and `compose.js` (`renderChips`, `composeInit`, the scheduling row id).

- [ ] **Step 2: Add the banner container** (`index.html`) — at the very top of `<section id="compose" ...>`, before `#chips`/`#master`:
```html
<div id="srcbanner" hidden></div>
```

- [ ] **Step 3: Implement the banner + mode rendering** (`compose.js`) — add `startInteraction`, `exitInteraction`, `renderSrcBanner`, and make `renderChips` interaction-aware:

```js
import { state, effectiveText, buildSpec, buildInteractSpec } from "./state.js"; // extend the existing state import

// startInteraction puts Compose into interaction mode for a resolved source post
// and switches to the Compose tab. action is "reply" | "quote".
export function startInteraction(src, action) {
  state.interaction = {
    action, platform: src.platform, ref: src.ref,
    sourcePreview: src.preview, sourceURL: src.preview.web_url, sourceAuthor: src.preview.author_handle,
    caps: src.caps, force: false,
  };
  state.master = "";
  state.images = [];
  state.platforms = new Set([src.platform]); // source locked on; user toggles fan-out
  state.focus = src.platform;
  document.querySelector('.tab[data-view="compose"]')?.click(); // switchTab via main.js
  renderInteractionUI();
}

// exitInteraction returns Compose to a normal new post.
export function exitInteraction() {
  state.interaction = null;
  state.platforms = new Set(ORDER);
  renderInteractionUI();
}

// renderInteractionUI re-renders the compose chrome for the current mode.
function renderInteractionUI() {
  renderSrcBanner();
  const sched = $("#schedrow"); if (sched) sched.hidden = state.interaction != null;
  $("#submit").textContent = state.interaction ? (state.interaction.action === "quote" ? "Quote" : "Reply") : "Post";
  renderChips(); renderCards(); renderMeta(); renderPreview();
}

// renderSrcBanner shows the source post being replied-to/quoted (or nothing).
function renderSrcBanner() {
  const host = $("#srcbanner"); if (!host) return;
  host.innerHTML = "";
  const it = state.interaction;
  if (!it) { host.hidden = true; return; }
  host.hidden = false;
  const verb = it.action === "quote" ? "Quoting" : "Replying to";
  const head = el("div", { class: "srcb-head" },
    el("span", { class: "srcb-verb", text: verb + " " }),
    el("span", { class: "srcb-author", text: it.sourceAuthor || it.platform }),
    el("button", { class: "srcb-x", type: "button", text: "× exit", onclick: exitInteraction }),
  );
  const body = el("div", { class: "srcb-text", text: (it.sourcePreview && it.sourcePreview.text) || "" });
  host.append(head, body);
  // capability override: if the chosen action is blocked, show the reason + a toggle.
  const cap = it.caps && it.caps[it.action];
  if (cap && !cap.allowed) {
    const warn = el("label", { class: "srcb-force" });
    const cb = el("input", { type: "checkbox", onchange: (e) => { it.force = e.target.checked; } });
    cb.checked = !!it.force;
    warn.append(cb, el("span", { text: " " + (cap.reason || "blocked") + " — try anyway" +
      (it.platform === "bluesky" ? " (Bluesky may silently drop it)" : "") }));
    host.append(warn);
  }
}
```
Make `renderChips` interaction-aware: when `state.interaction != null`, the **source platform chip is locked on** (always selected, click does nothing / disabled) and the other chips toggle fan-out. Modify the existing `renderChips` click handler:
```js
function renderChips() {
  const c = $("#chips"); c.innerHTML = "";
  const it = state.interaction;
  for (const p of ORDER) {
    const on = state.platforms.has(p);
    const locked = it != null && p === it.platform;
    const chip = el("button", {
      class: "chip p-" + p + (on ? " on" : "") + (locked ? " locked" : ""), type: "button",
      text: META[p].label + (it && p === it.platform ? " · source" : (it ? " · link" : "")),
      onclick: () => {
        if (locked) return; // source platform stays selected in interaction mode
        on ? state.platforms.delete(p) : state.platforms.add(p);
        if (!state.platforms.has(state.focus)) state.focus = p;
        renderChips(); renderCards(); renderPreview();
      },
    });
    c.append(chip);
  }
}
```
(Drop the `canFanout`/`continue` line — show all platforms; the source is locked, others are fan-out toggles. Keep it simple: every platform is shown; the source is locked-on.)

Add the `ORDER` import to compose.js if not already imported (it imports from common.js — confirm `ORDER` is in that import list; add it if missing).

- [ ] **Step 4: Wrap the scheduling row with `#schedrow`** — in `index.html`, ensure the scheduling controls (`#schedat`, `#schedclear`, `#schednote`) are inside a container `<div id="schedrow">…</div>` so `renderInteractionUI` can hide them. If they're already in a labeled row, add `id="schedrow"` to that wrapper.

- [ ] **Step 5: CSS** (`app.css`) — add source-banner styles (mirror existing card styles, real vars):
```css
/* Compose interaction-mode source banner */
#srcbanner{border:1px solid var(--accent-dim);background:var(--bg-2);border-radius:8px;padding:10px;margin-bottom:10px}
.srcb-head{display:flex;align-items:baseline;gap:6px;flex-wrap:wrap}
.srcb-verb{color:var(--muted)}
.srcb-author{font-weight:600;color:var(--accent)}
.srcb-x{margin-left:auto;font:inherit;font-size:12px;background:none;border:0;color:var(--muted);cursor:pointer}
.srcb-text{white-space:pre-wrap;color:var(--ink);margin-top:6px;font-size:13px;max-height:6em;overflow:auto}
.srcb-force{display:flex;align-items:center;gap:6px;margin-top:8px;font-size:12px;color:var(--bad)}
.chip.locked{opacity:.9;cursor:default}
```

- [ ] **Step 6: Verify** — `node --check internal/web/assets/compose.js` + `go build ./cmd/publisher` + `go test ./internal/web/`. Clean/pass.

- [ ] **Step 7: Commit**
```bash
git add internal/web/assets/index.html internal/web/assets/compose.js internal/web/assets/app.css
git commit -m "web/compose: interaction-mode source banner + constrained chips"
```

---

## Task 3: `doInteract` multipart Send + submit routing

**Files:** Modify `internal/web/assets/compose.js`.

- [ ] **Step 1: Read** `doPost` (FormData with `spec` + `image` files → `/api/post`), `submit` (over-limit guard → `doPost`), and `showResultModal` (it reads `data.post_id`, `data.status`, `data.targets`).

- [ ] **Step 2: Implement `doInteract`** (mirror `doPost` but post to `/api/interact` and normalize the result):
```js
async function doInteract() {
  const btn = $("#submit");
  const label = state.interaction.action === "quote" ? "Quote" : "Reply";
  btn.disabled = true; btn.textContent = label + "ing…";
  const fd = new FormData();
  fd.append("spec", JSON.stringify(buildInteractSpec()));
  for (const img of state.images) fd.append("image", img.file);
  try {
    const r = await fetch("/api/interact", { method: "POST", body: fd, credentials: "same-origin" });
    const data = await r.json();
    if (!r.ok) throw new Error(data.error || ("HTTP " + r.status));
    showResultModal({ post_id: data.id, status: data.status, targets: data.targets });
    exitInteraction(); // return to a normal composer after a successful interaction
  } catch (e) {
    flash("Error: " + e.message);
  } finally { btn.disabled = false; btn.textContent = label; }
}
```

- [ ] **Step 3: Route `submit`** — in interaction mode, skip the over-limit confirm (commentary auto-threads server-side) and call `doInteract`. Change the top of `submit`:
```js
function submit() {
  if (state.interaction) {
    if (!state.interaction.platform) { flash("No source post."); return; }
    doInteract();
    return;
  }
  // ... existing normal-post over-limit guard + doPost ...
}
```

- [ ] **Step 4: Verify** — `node --check internal/web/assets/compose.js` + `go build ./cmd/publisher`. Clean.

- [ ] **Step 5: Commit**
```bash
git add internal/web/assets/compose.js
git commit -m "web/compose: doInteract multipart send + submit routing"
```

---

## Task 4: Interact tab hand-off (remove inline panel)

**Files:** Modify `internal/web/assets/interact.js`, `internal/web/assets/app.css`.

- [ ] **Step 1: Read** `interact.js` — `renderSource(s)` and the `actionPanel(s)` it appends; the imports.

- [ ] **Step 2: Replace `actionPanel` with hand-off buttons** — Reply/Quote call `startInteraction`; Repost posts immediately to `/api/interact` (multipart spec, no text/images); blocked actions still surface the reason (`confirmModal` → set a one-shot force for repost, or hand off with the banner override for reply/quote). Replace the `actionPanel` function and its call:

```js
import { el, $, api, flash } from "./common.js";
import { startInteraction } from "./compose.js";

function actionRow(s) {
  const row = el("div", { class: "act-panel" });
  for (const [action, cap] of [["reply", s.caps.reply], ["repost", s.caps.repost], ["quote", s.caps.quote]]) {
    const btn = el("button", { class: "act-btn", type: "button", text: action[0].toUpperCase() + action.slice(1) });
    if (!cap.allowed) { btn.classList.add("blocked"); btn.title = cap.reason || "not allowed"; }
    btn.addEventListener("click", () => {
      if (action === "repost") { doRepost(s, cap); return; }
      startInteraction(s, action); // reply/quote → Compose interaction mode (override handled in the banner)
    });
    row.append(btn);
  }
  return row;
}

// doRepost posts a one-click repost via /api/interact (multipart spec, no media).
async function doRepost(s, cap) {
  const send = async (force) => {
    const fd = new FormData();
    fd.append("spec", JSON.stringify({ action: "repost", platform: s.platform, ref: s.ref,
      source_url: s.preview.web_url, source_author: s.preview.author_handle, force: !!force }));
    try {
      const r = await fetch("/api/interact", { method: "POST", body: fd, credentials: "same-origin" });
      const data = await r.json();
      if (!r.ok) throw new Error(data.error || ("HTTP " + r.status));
      flash("repost " + data.status);
    } catch (e) { flash("Error: " + e.message); }
  };
  if (!cap.allowed) {
    if (window.confirm("repost: " + (cap.reason || "blocked") + " — try anyway?")) send(true); // eslint-disable-line no-alert
    return;
  }
  send(false);
}
```
In `renderSource`, replace `card.append(actionPanel(s))` with `card.append(actionRow(s))`. Remove the now-dead `actionPanel`, its `send`/`fanout`/`confirmModal` machinery, and any imports it alone used (`confirmModal` if no longer referenced — grep first). Keep `el/$/api/flash` as needed.

NOTE: `interact.js` importing `compose.js` is acyclic (compose.js does NOT import interact.js — confirm with `grep "interact" internal/web/assets/compose.js`). If a cycle exists, instead expose `startInteraction` on `window` from compose.js and call it from interact.js — but the direct import should be fine.

- [ ] **Step 3: Remove dead `.act-*` styles** for the old inline composer (`.act-compose`, `.act-text`, `.act-fanout`, `.act-fan`, `.act-go`, `.act-status`, `.act-compose[hidden]`/`.act-fanout[hidden]`, `.act-btn.active`) from `app.css` — keep `.act-panel`, `.act-btn`, `.act-btn.blocked` (still used by `actionRow`). Grep the CSS for `.act-` and delete only the unused rules.

- [ ] **Step 4: Verify** — `node --check internal/web/assets/interact.js` + `go build ./cmd/publisher` + `go test ./internal/web/`. Clean/pass.

- [ ] **Step 5: Commit**
```bash
git add internal/web/assets/interact.js internal/web/assets/app.css
git commit -m "web/interact: hand off reply/quote to Compose; repost one-click"
```

---

## Task 5: docs + full verification

**Files:** Modify `README.md`.

- [ ] **Step 1: Update the Interact-tab paragraph** in `README.md` (the "Web UI & /api" section) to: paste a URL/Nostr-id → preview → **Reply/Quote open the Compose tab** (interaction mode: source banner, live preview, auto-split, media, fan-out toggles); **Repost** is one-click.

- [ ] **Step 2: Full verification**:
```bash
go test ./...
go vet ./...
go build ./cmd/publisher
for f in internal/web/assets/*.js; do node --check "$f" || echo "SYNTAX FAIL $f"; done
```
Expected: all pass, all `node --check` clean. STOP and report on any failure.

- [ ] **Step 3: Commit**
```bash
git add README.md
git commit -m "docs: Interact tab opens Compose for reply/quote"
```

---

## Self-Review notes (for the implementer)

- **Spec coverage (Plan 2):** §Flow — `state.interaction`, `startInteraction`/`exitInteraction`, hand-off from Interact, Repost one-click (T1,T2,T4); §source banner + override (T2); §unified platform model — source locked + fan-out toggles (T2); §hidden scheduling (T2); §Send → multipart `/api/interact` (T3); §remove inline panel (T4).
- **No import cycle:** `interact.js`→`compose.js`→(`state.js`/`preview.js`/`history.js`/`common.js`). `compose.js` must NOT import `interact.js` (it doesn't). The tab switch uses a DOM click (the existing pattern in `showResultModal.goHistory`), not a `main.js` import.
- **Preview fidelity (known v1 limitation):** the live preview shows your **commentary** for the focused platform. For the **source** platform that's exactly the post text (accurate). For a **fan-out** platform the real post also appends the reproduced original + URL server-side, so the preview under-represents fan-out length/threading. Acceptable for v1 (the source platform — the primary target — previews accurately); note it. Do not duplicate `assembleReproduction` in JS.
- **Result modal reuse:** `/api/interact` returns a `store.Post` (`id`/`status`/`targets`); `doInteract` normalizes it to `{post_id, status, targets}` for `showResultModal`.
- **Type/shape consistency:** `buildInteractSpec` emits exactly the Plan-1 `/api/interact` spec shape (`source_preview.{author,text,media[]}`, `images[].alt`, `fanout`, `number`, `force`). `startInteraction(src, action)` consumes a `SourceRef` (`src.platform`, `src.ref`, `src.preview.{text,media,web_url,author_handle}`, `src.caps`).
