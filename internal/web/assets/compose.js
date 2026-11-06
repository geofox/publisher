"use strict";
import { el, $, wcount, flash, confirmModal, META, ORDER, v1, problemError } from "./common.js";
import { state, postedText, v1PostBody, defaultOv, setInflight, getInflight, clearInflight, clearImages, imagesGen, bumpImagesGen } from "./state.js";
import { renderPreview, mediaMax, previewMedia } from "./preview.js";
import { openDetail } from "./history.js";
import { accountFor } from "./accounts.js";
import { followPost } from "./follow.js";
import { brandTile, icon, PLATFORM_META } from "./brands.js";
import { planFor, lastPlanFor, planError } from "./plan.js";
import { editorInit, renderEditor, setTab } from "./editor.js";
import { fromMarkers, toLF } from "./textpos.js";
import { countFor, limitFor } from "./count.js";

export function markDirty() {
  state.dirty = true;
  state.edits++;
  const el2 = document.getElementById("draft-status");
  if (el2) { el2.className = "draft-status dirty"; el2.textContent = "unsaved changes"; }
  const save = document.getElementById("draft-save");
  if (save) save.disabled = false;
}

// ---------------------------------------------------------------------------
// Counter / character class
// ---------------------------------------------------------------------------

function counterClass(n, limit) {
  if (!limit) return "cnt muted";
  if (n > limit) return "cnt bad";
  if (n > limit * 0.9) return "cnt warn";
  return "cnt ok";
}

// ---------------------------------------------------------------------------
// Live counts. How a post splits is the server's plan (plan.js); the counts
// here keep up with typing, measured the way each platform counts.
// ---------------------------------------------------------------------------

// bskyCardText strips a trailing URL for the Bluesky effective-text estimate,
// mirroring dispatch.PlanBlueskyCard + unfurl.StripTrailing. When a trailing
// URL will become a link card the server posts a shorter text, so the
// client-side counter must count the stripped text — not the raw URL — or it
// diverges from the live preview. Only strips when there are no attached images
// (images take the embed slot, blocking the card) and we're not in interaction
// mode (interactions never carry cards). URL-only posts are left as-is (the URL
// stays in the text even when a card attaches, per PlanBlueskyCard).
const _bskyURLRe = /https?:\/\/[^\s]+/g;
function bskyCardText(text) {
  if (state.images.length > 0 || state.interaction !== null) return text;
  _bskyURLRe.lastIndex = 0;
  let m, last;
  while ((m = _bskyURLRe.exec(text)) !== null) last = m;
  if (!last) return text;
  const url = last[0].replace(/[.,!?);:]+$/, ""); // mirror unfurl.trimURL
  if (text.slice(last.index + url.length).trim() !== "") return text; // not trailing
  const stripped = text.trimEnd().slice(0, text.trimEnd().length - url.length).trimEnd();
  return stripped.trim() === "" ? text : stripped; // URL-only post: keep as-is
}

// effectivePostedText returns the text the platform will actually post: for
// Bluesky, a trailing card URL is stripped (server attaches it as a link card).
function effectivePostedText(p) {
  const t = postedText(p);
  return p === "bluesky" ? bskyCardText(t) : t;
}

// threadInfoFor is platform p's post count from the server's plan (the last
// one while a newer is on its way).
function threadInfoFor(p) {
  const pl = planFor(p) || lastPlanFor(p);
  const posts = pl && pl.items.length ? pl.items.length : 1;
  return { posts, threaded: posts > 1 };
}

// cwOf is platform p's content warning, which Mastodon counts in.
function cwOf(p) {
  const ov = state.ov[p];
  return p === "mastodon" ? ov.spoiler_text || "" : p === "nostr" ? ov.content_warning || "" : "";
}

// targetCount → {text, cls} for a platform's live count. State colors mirror the
// design: under 90% → ink2 (no class), >90% → amber, over → indigo (will thread,
// informational), no-limit → teal ∞.
function targetCount(p) {
  const limit = limitFor(p, cwOf(p)), n = countFor(p, effectivePostedText(p));
  if (!limit) return { text: "∞", cls: "inf", n };
  const pct = n / limit;
  return { text: `${n}/${limit}`, cls: n > limit ? "over" : pct > 0.9 ? "near" : "", n };
}

// ---------------------------------------------------------------------------
// Targets — selectable rows with brand tile, live count, switch, thread badge
// ---------------------------------------------------------------------------

function iosSwitch(on) { return el("div", { class: "ios-switch" + (on ? " on" : "") }, el("div", { class: "knob" })); }

function threadBadge(p) {
  const ti = threadInfoFor(p);
  const badge = el("span", { class: "thread-badge", title: "See and shape the thread",
    onclick: e => { e.stopPropagation(); showThread(p); } });
  const ic = icon("thread", { size: 12, sw: 1.9 }); if (ic) badge.append(ic);
  badge.append(el("span", { class: "tb-txt", text: `${ti.posts} posts` }));
  return badge;
}

function renderChips() {
  const c = $("#chips"); c.innerHTML = "";
  const it = state.interaction;
  for (const p of ORDER) {
    const on = state.platforms.has(p);
    const locked = it != null && p === it.platform;
    // A reply goes only to the source's network.
    const barred = it != null && it.action === "reply" && !locked;
    const meta = PLATFORM_META[p] || {};
    const tc = targetCount(p);
    const ti = threadInfoFor(p);

    const top = el("div", { class: "target-top" }, el("span", { class: "t-name", text: META[p].label }));
    if (it) top.append(el("span", { class: "t-tag" + (locked ? " native" : ""), text: locked ? "native" : "copy" }));
    if (on && ti.threaded) top.append(threadBadge(p));

    const row = el("div", {
      class: "target-row p-" + p + (on ? "" : " off") + (locked ? " locked" : "") + (barred ? " barred" : ""),
      title: barred ? "A reply goes only to " + META[it.platform].label : null,
      onclick: () => {
        if (locked || barred) return; // source platform stays selected in interaction mode
        on ? state.platforms.delete(p) : state.platforms.add(p);
        if (!state.platforms.has(state.focus)) state.focus = p;
        renderChips(); renderCards(); renderEditor(); renderPreview();
      },
    },
      brandTile(p, { size: 29 }),
      el("div", { class: "target-main" }, top, el("div", { class: "t-handle", text: meta.handle || "" })),
      el("span", { class: "t-count" + (tc.cls ? " " + tc.cls : ""), text: tc.text }),
      iosSwitch(on),
    );
    c.append(row);
  }
  renderTargetsMeta();
  renderThreadNotice();
}

// refreshTargets updates each row's count + thread badge in place on every
// keystroke (rebuilding rows would re-create the brand-mark <img>s and flicker).
function refreshTargets() {
  for (const p of ORDER) {
    const row = $(`#chips .target-row.p-${p}`); if (!row) continue;
    const on = state.platforms.has(p);
    const tc = targetCount(p);
    const cnt = row.querySelector(".t-count");
    if (cnt) { cnt.textContent = tc.text; cnt.className = "t-count" + (tc.cls ? " " + tc.cls : ""); }
    const top = row.querySelector(".target-top");
    const ti = threadInfoFor(p);
    let badge = top.querySelector(".thread-badge");
    if (on && ti.threaded) {
      if (!badge) { badge = threadBadge(p); top.append(badge); }
      else badge.querySelector(".tb-txt").textContent = `${ti.posts} posts`;
    } else if (badge) { badge.remove(); }
  }
  renderTargetsMeta();
  renderThreadNotice();
}

function renderTargetsMeta() {
  const m = $("#targets-meta"); if (m) m.textContent = `${state.platforms.size} selected`;
  const aud = $("#compose-audience");
  if (aud) aud.textContent = state.platforms.size ? `Public · mirror to ${state.platforms.size}` : "Public";
}

// shortId middle-truncates a long opaque identifier (a Nostr npub, hex key, …)
// to "head…tail" so it stays glanceable instead of overflowing the row.
function shortId(s, head = 10, tail = 5) {
  return s && s.length > head + tail + 3 ? s.slice(0, head) + "…" + s.slice(-tail) : s;
}
// isOpaqueId reports whether s is an identifier (npub/nprofile/long hex) rather
// than a human handle/name — those are the ones worth middle-truncating.
function isOpaqueId(s) { return /^(npub1|nprofile1)/i.test(s) || /^[0-9a-f]{32,}$/i.test(s); }

// applyIdentity swaps the placeholder author + per-platform handles for the
// operator's real profile (from the accounts, GET /v1/accounts). Called once after boot;
// failures leave the placeholders untouched.
export function applyIdentity(id) {
  if (!id) return;
  const acc = id.accounts || {};
  for (const p of ORDER) {
    const a = acc[p];
    if (a && a.handle && PLATFORM_META[p]) {
      // The Nostr npub is ~63 chars — truncate it (and any other opaque id) for display.
      PLATFORM_META[p].handle = (p === "nostr" || isOpaqueId(a.handle)) ? shortId(a.handle) : a.handle;
    }
  }
  const nameEl = $("#compose-author");
  if (nameEl && id.name) nameEl.textContent = isOpaqueId(id.name) ? shortId(id.name) : id.name;
  const av = $("#compose-avatar");
  if (av) {
    if (id.avatar) {
      av.style.background = "none";
      av.textContent = "";
      av.append(el("img", { src: id.avatar, alt: "", class: "avatar-img" }));
    } else if (id.monogram) {
      av.textContent = id.monogram;
    }
  }
  renderChips(); // re-render target rows so the real handles show
}

// rerenderComposer redraws every composer view, after something they all
// read changed (the accounts' limits arrived).
export function rerenderComposer() {
  renderChips(); renderCards(); renderMeta(); renderEditor(); renderPreview();
}

// showThread opens platform p's tab in the thread editor.
function showThread(p) {
  setTab(p);
  document.getElementById("editor")?.scrollIntoView({ behavior: "smooth", block: "start" });
}

// renderThreadNotice shows the calm informational banner above POST TO whenever
// ≥1 selected target is a thread; tapping it opens that platform's tab.
function renderThreadNotice() {
  const host = $("#threadnotice"); if (!host) return;
  host.innerHTML = "";
  const threaded = ORDER.filter(p => state.platforms.has(p) && threadInfoFor(p).threaded);
  if (!threaded.length) return;
  const names = threaded.map(p => `${META[p].label} (${threadInfoFor(p).posts})`).join(", ");
  const banner = el("div", { class: "thread-notice", onclick: () => showThread(threaded[0]) });
  const ic = icon("thread", { size: 20, sw: 1.8 }); if (ic) banner.append(ic);
  banner.append(el("div", { class: "tn-body" },
    el("div", { class: "tn-title", text: `Auto-threading on ${names}` }),
    el("div", { class: "tn-sub", text: "Each platform gets a thread that fits its limit: tap to see and shape it." }),
  ));
  const ch = icon("chevron", { size: 16 }); if (ch) banner.append(ch);
  host.append(banner);
}

// ---------------------------------------------------------------------------
// Interaction mode — source banner + constrained chips
// ---------------------------------------------------------------------------

// startInteraction puts Compose into interaction mode for a resolved source post
// (src is a resolved source, see interact.js) and switches to the Compose tab.
// action is "reply" | "quote".
export function startInteraction(src, action) {
  const begin = () => {
    state.interaction = {
      action, platform: src.platform, ref: src.ref,
      sourcePreview: src.preview, sourceURL: src.preview.web_url, sourceAuthor: src.preview.author_handle,
      sourceInput: src.input || src.preview.web_url, // what POST /v1/posts resolves again
      caps: src.caps,
      // restored on exit; preserve the pre-interaction selection across a nested
      // re-entry (starting a new interaction while already in one).
      prevPlatforms: state.interaction && state.interaction.prevPlatforms
        ? state.interaction.prevPlatforms : new Set(state.platforms),
    };
    state.master = "";
    state.breaks = [];
    state.tab = "shared";
    clearImages();
    ORDER.forEach((p) => { state.ov[p] = defaultOv(p); }); // drop stale per-platform overrides
    state.platforms = new Set([src.platform]);
    state.focus = src.platform;
    const tab = document.querySelector('.tab[data-view="compose"]');
    if (tab) tab.click();
    window.scrollTo({ top: 0, behavior: "smooth" });
    flash((action === "quote" ? "Quoting " : "Replying to ") + (src.preview.author_handle || src.platform));
    renderImages(); // entry cleared state.images → refresh the thumbnail strip (symmetric with exit)
    renderInteractionUI();
  };
  if (state.master.trim() || state.images.length) {
    confirmModal({
      title: "Replace your current draft?",
      body: "Starting a " + action + " will clear what you've written in Compose.",
      confirmText: "Replace draft",
      onConfirm: async () => { begin(); return true; },
    });
    return;
  }
  begin();
}

// exitInteraction returns Compose to a normal new post.
export function exitInteraction() {
  const prev = state.interaction && state.interaction.prevPlatforms;
  state.interaction = null;
  state.master = "";
  state.breaks = [];
  state.tab = "shared";
  clearImages();
  state.platforms = prev && prev.size ? new Set(prev) : new Set(ORDER);
  if (!state.platforms.has(state.focus)) state.focus = [...state.platforms][0] || "bluesky";
  renderImages();
  renderInteractionUI();
}

// loadDraft hydrates Compose from a saved draft spec (the server's draftSpecJSON
// shape — master_text, platforms, overrides, interaction, plus a media[] array
// of already-uploaded blossom_url references). Passing { text, lang } is
// supported as a legacy shape for the History translate-to-Compose flow.
export function loadDraft(input) {
  if (!input) return;

  const doLoad = () => {
    // legacy {text, lang}
    if (input.text !== undefined && input.master_text === undefined) {
      state.interaction = null;
      // An old post's text may break its thread with lines of "---".
      const conv = fromMarkers(input.text || "");
      state.master = conv.text;
      state.breaks = conv.breaks;
      state.anchors = {};
      state.tab = "shared";
      // Breaks and wording made for the text it replaces don't fit it.
      for (const p of ORDER) Object.assign(state.ov[p], { text: null, breaks: null, mediaAt: null });
      clearImages();
      if (input.lang) {
        state.ov.bluesky.langs = input.lang;
        state.ov.mastodon.language = input.lang;
      }
      state.platforms = new Set(ORDER);
      if (!state.platforms.has(state.focus)) state.focus = "bluesky";
      state.activeDraftId = null;
      state.dirty = true;
      const tab = document.querySelector('.tab[data-view="compose"]');
      if (tab) tab.click();
      window.scrollTo({ top: 0, behavior: "smooth" });
      renderImages();
      renderInteractionUI(); // re-renders chips/cards/preview and hides the banner
      flash("Translated draft loaded" + (input.lang ? " · lang " + input.lang : ""));
      return;
    }

    // full spec shape
    state.interaction = input.interaction || null;
    // Before v2 (a recovered snapshot from an older app) breaks were lines
    // of "---" in the text and media went by part number.
    const old = input.v !== 2;
    const conv = old ? fromMarkers(input.master_text || "") : null;
    state.master = old ? conv.text : input.master_text || "";
    state.breaks = old ? conv.breaks : (input.breaks || []).filter(Number.isInteger);
    state.tab = "shared";
    clearImages();
    state.platforms = new Set(input.platforms && input.platforms.length ? input.platforms : ORDER);
    if (!state.platforms.has(state.focus)) state.focus = [...state.platforms][0] || "bluesky";
    // reset overrides to defaults, then apply saved overrides
    ORDER.forEach(p => { state.ov[p] = defaultOv(p); });
    if (input.overrides) {
      for (const p of Object.keys(input.overrides)) {
        if (!state.ov[p]) continue;
        Object.assign(state.ov[p], input.overrides[p]);
        const ov = state.ov[p];
        if (old && typeof ov.text === "string") {
          const own = fromMarkers(ov.text);
          ov.text = own.text;
          ov.breaks = own.breaks.length ? own.breaks : null;
        }
        if (ov.text != null && typeof ov.text !== "string") ov.text = null;
        if (!Array.isArray(ov.breaks)) ov.breaks = null;
        if (!ov.mediaAt || typeof ov.mediaAt !== "object" || ov.text == null) ov.mediaAt = null;
      }
      // buildSpec serializes bluesky.langs as a JSON array; the editor input
      // holds it as a comma-separated string. Convert back so the field round-
      // trips cleanly through save → load → save.
      if (state.ov.bluesky && Array.isArray(state.ov.bluesky.langs)) {
        state.ov.bluesky.langs = state.ov.bluesky.langs.join(",");
      }
    }
    // images come from input.media (hydrated draft) — already-uploaded references
    state.images = (input.media || []).map(m => ({
      blossom_url: m.blossom_url, sha256: m.sha256, mime: m.mime,
      dim: m.dim, blurhash: m.blurhash, size_bytes: m.size_bytes, alt: m.alt || "",
      ordinal: m.ordinal, file: null, media_id: m.media_id || "",
      duration_secs: m.duration_secs || 0,
      video: /^video\//.test(m.mime || ""),
      poster_url: m.poster_url || "",
      // derive a displayable URL from blossom_url for the thumbnail strip
      url: m.blossom_url || "",
      // restored video drafts are already transcoded — render as ready
      phase: /^video\//.test(m.mime || "") ? "ready" : undefined,
      id: m.client_id || m.id || newKey(),
    }));
    bumpImagesGen();
    // Media positions, by attachment id (before v2: part numbers).
    state.anchors = {};
    for (const [id, v] of Object.entries(input.anchors || {})) {
      if (!Number.isInteger(v) || v <= 0) continue;
      state.anchors[id] = old ? conv.parts[Math.min(v, conv.parts.length - 1)] : v;
    }
    normalizeLines();
    state.activeDraftId = input.id || null;
    state.draftETag = input.etag || "";
    state.dirty = false;
    const num = $("#threadnum"); if (num && input.number !== undefined) num.checked = !!input.number;
    const sched = $("#schedat"); if (sched) sched.value = input.interaction ? "" : localInput(input.scheduled_at);
    const tab = document.querySelector('.tab[data-view="compose"]');
    if (tab) tab.click();
    window.scrollTo({ top: 0, behavior: "smooth" });
    renderImages();
    renderInteractionUI(); // re-renders chips/cards/preview and hides the banner
    updateSched(); // a loaded schedule shows (and the button says Schedule)
    flash(input.notice || "Draft loaded");
    if (input.onLoaded) input.onLoaded();
  };

  if (state.master.trim() || state.images.length || state.interaction) {
    confirmModal({
      title: "Replace your current draft?",
      body: "Loading this draft will replace what's in Compose.",
      confirmText: "Replace",
      onConfirm: async () => { doLoad(); return true; },
    });
    return;
  }
  doLoad();
}

// normalizeLines turns CRLF in the loaded texts into LF, as the cards show
// them, moving every position with it (a draft made through the API may
// have CRLF).
function normalizeLines() {
  const sh = toLF(state.master);
  state.master = sh.text;
  state.breaks = state.breaks.map(sh.move);
  for (const k of Object.keys(state.anchors)) state.anchors[k] = sh.move(state.anchors[k]);
  for (const p of ORDER) {
    const ov = state.ov[p];
    if (ov.text == null) {
      if (ov.breaks) ov.breaks = ov.breaks.map(sh.move);
      continue;
    }
    const own = toLF(ov.text);
    ov.text = own.text;
    if (ov.breaks) ov.breaks = ov.breaks.map(own.move);
    if (ov.mediaAt) for (const k of Object.keys(ov.mediaAt)) ov.mediaAt[k] = own.move(ov.mediaAt[k]);
  }
}

// localInput turns an RFC 3339 time into a datetime-local value (local
// wall time, to the minute); "" for none or a bad one.
function localInput(iso) {
  const d = iso ? new Date(iso) : null;
  if (!d || isNaN(d)) return "";
  const pad = n => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

// renderInteractionUI re-renders the compose chrome for the current mode.
function renderInteractionUI() {
  renderSrcBanner();
  const sched = $("#schedrow"); if (sched) sched.hidden = state.interaction != null;
  $("#submit").textContent = state.interaction ? (state.interaction.action === "quote" ? "Quote" : "Reply") : "Post";
  renderChips(); renderCards(); renderMeta(); renderEditor(); renderPreview();
}

// renderSrcBanner shows the source post being replied-to / quoted (or hides it).
function renderSrcBanner() {
  const host = $("#srcbanner"); if (!host) return;
  host.innerHTML = "";
  const it = state.interaction;
  if (!it) { host.hidden = true; return; }
  host.hidden = false;
  const verb = it.action === "quote" ? "Quoting" : "Replying to";
  host.append(el("div", { class: "srcb-head" },
    el("span", { class: "srcb-verb", text: verb + " " }),
    el("span", { class: "srcb-author", text: it.sourceAuthor || it.platform }),
    el("button", { class: "srcb-x", type: "button", text: "← cancel", onclick: exitInteraction }),
  ));
  host.append(el("div", { class: "srcb-text", text: (it.sourcePreview && it.sourcePreview.text) || "" }));
  const sp = it.sourcePreview || {};
  if (sp.media && sp.media.length) {
    const g = el("div", { class: "srcb-media" });
    for (const m of sp.media) g.append(el("img", { src: m.url, alt: m.alt || "" }));
    host.append(g);
  }
  const foot = el("div", { class: "srcb-foot" });
  const plat = (META[it.platform] && META[it.platform].label) || it.platform;
  foot.append(el("span", { class: "srcb-hint muted",
    text: it.action === "quote"
      ? plat + " gets a native quote; toggle other targets to also post a copy there."
      : "A reply goes only to " + plat + "; quote the post to share it elsewhere." }));
  if (it.sourceURL) foot.append(el("a", { class: "srcb-link", href: it.sourceURL, target: "_blank", rel: "noopener", text: "open original ↗" }));
  host.append(foot);
  // The action is offered only when allowed (interact.js); a note it came
  // with (a quote that may be shown as a link) is shown here.
  const cap = it.caps && it.caps[it.action];
  if (cap && cap.reason) host.append(el("div", { class: "srcb-note muted", text: cap.reason }));
}

// ---------------------------------------------------------------------------
// Override cards (collapsed summary → expand to edit)
// ---------------------------------------------------------------------------

function renderCards() {
  const c = $("#cards"); c.innerHTML = "";
  for (const p of ORDER) {
    if (!state.platforms.has(p)) continue;
    c.append(overrideCard(p));
  }
  renderMeta();
}

// refreshCounts updates the per-platform counts (and any open mirroring card's
// textarea) in place on every master keystroke, instead of rebuilding all the
// override cards — which is expensive and would collapse expanded cards as you
// type. Edited cards track their own text, so they're left untouched.
function refreshCounts() {
  for (const p of ORDER) {
    if (!state.platforms.has(p)) continue;
    const card = $(`#cards .ocard.p-${p}`);
    if (!card) continue;
    const limit = limitFor(p, cwOf(p));
    const n = countFor(p, effectivePostedText(p)); // full posted text (reproduction on fan-out) — matches the preview
    const span = card.querySelector(".cnt");
    if (span) { span.textContent = limit ? `${n}/${limit}` : "∞"; span.className = counterClass(n, limit); }
    const snip = card.querySelector(".ocard-snip");
    if (snip) snip.textContent = wordingNote(p);
  }
}

// wordingNote says where platform p's text comes from.
function wordingNote(p) {
  const ov = state.ov[p];
  if (ov.text != null) return `own text: "${ov.text.slice(0, 40)}"`;
  return ov.breaks ? "shared text · own breaks" : "mirrors the shared text";
}

// overrideCard is platform p's options. Its wording and breaks are edited in
// the thread editor (its tab there).
function overrideCard(p) {
  const ov = state.ov[p], meta = META[p];
  const edited = ov.text != null || ov.breaks != null;
  const limit = limitFor(p, cwOf(p)), n = countFor(p, effectivePostedText(p));
  const card = el("div", { class: "ocard p-" + p + (edited ? " edited" : "") });

  // collapsed summary row (always visible, clickable to expand)
  const cnt = el("span", { class: counterClass(n, limit), text: limit ? `${n}/${limit}` : "∞" });
  const body = el("div", { class: "ocard-body", hidden: true });
  const caret = el("span", { class: "ocard-caret", text: "▸" });
  const summary = el("button", { class: "ocard-summary", type: "button",
    onclick: () => {
      const open = body.hidden; body.hidden = !open; caret.textContent = open ? "▾" : "▸";
      if (open) { state.focus = p; renderPreview(); }
    } },
    caret,
    el("span", { class: "ocard-name", text: meta.label }),
    cnt,
    el("span", { class: "ocard-snip muted", text: wordingNote(p) }),
  );
  const wording = el("button", { class: "ghost xs", type: "button", text: `${meta.label}'s thread and wording →`,
    onclick: () => showThread(p) });
  body.append(fieldsFor(p, ov, () => { refreshCounts(); refreshTargets(); renderMeta(); renderPreview(); }), wording);

  card.append(summary, body);
  return card;
}

// ---------------------------------------------------------------------------
// Plain micro-label helper + per-platform fields
// ---------------------------------------------------------------------------

function lbl(t, input) { return el("label", {}, el("span", { class: "fl", text: t }), input); }

// langField builds the per-platform language widget. When the operator has
// configured 2+ languages (USER_LANGUAGES env, surfaced via /v1/instance →
// state.userLanguages), it's a dropdown of those codes; with 0 or 1 it's a
// static muted line — the value is still set (via defaultOv) just not editable.
// onChange receives the new code; renderPreview re-runs after a pick.
function langField(value, onChange) {
  const langs = state.userLanguages;
  if (langs.length > 1) {
    const sel = el("select", { onchange: e => { onChange(e.target.value); renderPreview(); } });
    for (const code of langs) sel.append(el("option", { value: code, text: code }));
    sel.value = langs.includes(value) ? value : langs[0];
    return sel;
  }
  return el("span", { class: "lang-static muted", text: value });
}

function fieldsFor(p, ov, onText = renderPreview) {
  const f = el("div", { class: "fields" });
  if (p === "bluesky") {
    f.append(lbl("lang (ISO 639-1)", langField(ov.langs, v => { ov.langs = v; })));
    const rsel = el("select", { onchange: e => { ov.reply = e.target.value; renderPreview(); } });
    [["", "anyone"], ["nobody", "nobody"], ["following", "following"], ["follower", "followers"], ["mention", "mentioned"]]
      .forEach(([v, t]) => rsel.append(el("option", { value: v, text: t })));
    rsel.value = ov.reply;
    f.append(lbl("replies", rsel));
    f.append(lbl("disable quotes", el("input", { type: "checkbox", checked: ov.disable_quotes, onchange: e => { ov.disable_quotes = e.target.checked; renderPreview(); } })));
  }
  if (p === "mastodon") {
    f.append(lbl("CW", el("input", { type: "text", value: ov.spoiler_text, oninput: e => { ov.spoiler_text = e.target.value; onText(); } })));
    f.append(lbl("sensitive", el("input", { type: "checkbox", checked: !!ov.sensitive, onchange: e => { ov.sensitive = e.target.checked; renderPreview(); } })));
    const sel = el("select", { onchange: e => { ov.visibility = e.target.value; renderPreview(); } });
    [["", "default"], ["public", "public"], ["unlisted", "unlisted"], ["private", "private"], ["direct", "direct"]]
      .forEach(([v, t]) => sel.append(el("option", { value: v, text: t })));
    sel.value = ov.visibility;
    f.append(lbl("visibility", sel));
    f.append(lbl("lang (ISO 639-1)", langField(ov.language, v => { ov.language = v; })));
  }
  if (p === "threads") {
    f.append(lbl("topic", el("input", { type: "text", value: ov.topic_tag, oninput: e => { ov.topic_tag = e.target.value; renderPreview(); } })));
    const tsel = el("select", { onchange: e => { ov.reply_control = e.target.value; renderPreview(); } });
    [["", "anyone"], ["accounts_you_follow", "following"], ["followers_only", "followers"], ["mentioned_only", "mentioned"], ["parent_post_author_only", "author only"]]
      .forEach(([v, t]) => tsel.append(el("option", { value: v, text: t })));
    tsel.value = ov.reply_control;
    f.append(lbl("replies", tsel));
  }
  if (p === "nostr") {
    f.append(lbl("PoW", el("input", { type: "number", value: ov.pow, min: 0, oninput: e => { ov.pow = parseInt(e.target.value || "0", 10); renderPreview(); } })));
    f.append(lbl("CW", el("input", { type: "text", value: ov.content_warning, oninput: e => { ov.content_warning = e.target.value; renderPreview(); } })));
  }
  return f;
}

// ---------------------------------------------------------------------------
// Images
// ---------------------------------------------------------------------------

// v1Form sends a multipart form to /v1 and answers its JSON, or throws the
// problem it answers with (see common.js problemError).
async function v1Form(path, fd, signal) {
  const r = await fetch(path, { method: "POST", body: fd, credentials: "same-origin", signal });
  if (!r.ok) throw problemError(r.status, await r.json().catch(() => null));
  return r;
}

// compressFile sends a File through POST /v1/media:compress and returns the
// re-encoded JPEG as a new File. Used by the per-attachment presets and the
// HEIC attach-time auto-conversion. Aborts any in-flight request for the same
// attachment (rapid preset switching must not pile up server transcodes).
async function compressFile(img, file, preset) {
  if (img._compressAbort) img._compressAbort.abort();
  const ctl = new AbortController();
  img._compressAbort = ctl;
  try {
    const fd = new FormData();
    fd.append("file", file);
    fd.append("preset", preset);
    const r = await v1Form("/v1/media:compress", fd, ctl.signal);
    const blob = await r.blob();
    const stem = (file.name || "image").replace(/\.[^.]+$/, "");
    return new File([blob], stem + ".jpg", { type: blob.type || "image/jpeg" });
  } finally {
    if (img._compressAbort === ctl) img._compressAbort = null;
  }
}

// uploadImage stores an attachment's current file through POST /v1/media,
// once per file (a preset pick makes a new file, uploaded again), so a post
// names it by id. The upload starts at attach time; Post waits for it. The
// alt text goes with the post, not the upload, so editing it needs no call.
function uploadImage(img) {
  const file = img.file;
  if (!file) return Promise.resolve();
  if (img._uploadFor === file) return img._upload || Promise.resolve();
  if (img._uploadAbort) img._uploadAbort.abort();
  const ctl = new AbortController();
  Object.assign(img, { _uploadFor: file, _uploadAbort: ctl, media_id: "", remote_url: "", upload_error: "" });
  const fd = new FormData();
  fd.append("file", file);
  const run = v1Form("/v1/media", fd, ctl.signal).then(r => r.json()).then(m => {
    if (img._uploadFor !== file) return;
    Object.assign(img, { media_id: m.id, remote_url: m.url || "", sha256: m.sha256 || "", blurhash: m.blurhash || "",
      mime: m.mime || img.mime, dim: m.dim || img.dim, size_bytes: m.size_bytes || img.size_bytes });
    renderPreview(); // the server preview can plan it now
  }).catch(err => {
    if (img._uploadFor !== file || err.name === "AbortError") return;
    img._uploadFor = null; // a retry uploads again
    img.upload_error = err.message || "upload failed";
    renderImages();
  }).finally(() => {
    if (img._uploadAbort === ctl) { img._upload = null; img._uploadAbort = null; }
  });
  img._upload = run;
  return run;
}

function fmtBytes(n) {
  return n >= 1048576 ? (n / 1048576).toFixed(1) + " MB" : Math.max(1, Math.round(n / 1024)) + " KB";
}

const isHeic = (f) => /^image\/hei[cf]$/.test(f.type) || /\.hei[cf]$/i.test(f.name || "");

function fmtDuration(s) { const m = Math.floor(s / 60); return m ? m + "m" + String(s % 60).padStart(2, "0") + "s" : s + "s"; }

// updateVideoProgress refreshes a video tile's progress without rebuilding the
// whole strip; falls back to a full render on phase changes or detached nodes.
function updateVideoProgress(img) {
  if (img._renderedPhase === img.phase && img._fillEl?.isConnected) {
    const pct = Math.round((img.pct || 0) * 100);
    img._labelEl.textContent = img.phase + " " + pct + "%";
    img._fillEl.style.width = pct + "%";
    return;
  }
  renderImages();
}

// One video XOR images per post (v1); a video uploads + transcodes
// immediately via the async job endpoint with three-phase progress:
// uploading (XHR), transcoding and storing (1 s job poll). The quality preset
// is chosen at attach time (#vidpreset) — re-deriving would be a minutes-long
// transcode and the raw original is deleted after normalization.
function attachVideo(file) {
  if (state.images.length) { flash(state.images.some(i => i.video) ? "Only one video per post" : "Remove the images first (one video OR images per post)"); return; }
  const entry = { video: true, _file: file, _preset: $("#vidpreset")?.value || "1080p",
                  url: "", alt: "", phase: "uploading", pct: 0, _xhr: null, _jobTimer: null };
  entry.id = newKey();
  state.images.push(entry);
  renderImages();
  startVideoUpload(entry);
}

function startVideoUpload(entry) {
  const gen = imagesGen;
  const xhr = new XMLHttpRequest();
  entry._xhr = xhr;
  // XHR, not fetch: only XHR reports upload progress.
  xhr.open("POST", "/v1/media");
  xhr.upload.onprogress = e => {
    if (e.lengthComputable) { entry.pct = e.loaded / e.total; updateVideoProgress(entry); }
  };
  xhr.onerror = () => failVideo(entry, gen, "upload failed");
  xhr.onload = () => {
    if (gen !== imagesGen) return;
    let m = null;
    try { m = JSON.parse(xhr.responseText); } catch { /* not json */ }
    if (xhr.status !== 202 && xhr.status !== 201) {
      failVideo(entry, gen, problemError(xhr.status, m).message);
      return;
    }
    entry._xhr = null;
    entry.phase = "queued"; entry.pct = 0; renderImages();
    const done = media => {
      clearInterval(entry._jobTimer); entry._jobTimer = null;
      Object.assign(entry, { phase: "ready", pct: 1, media_id: media.id, blossom_url: media.url, remote_url: media.url,
        sha256: media.sha256, mime: media.mime, dim: media.dim, size_bytes: media.size_bytes,
        duration_secs: media.duration_secs || 0, url: media.url, poster_url: media.poster_url || "" });
      entry._file = null;
      renderImages();
    };
    if (m.status === "ready") { done(m); return; }
    // GET /v1/media/{id} says how far processing is: its stage (queued,
    // transcoding, uploading to media storage) and progress, until the video
    // is ready or failed.
    entry._jobTimer = setInterval(async () => {
      if (gen !== imagesGen) { clearInterval(entry._jobTimer); return; }
      try {
        const cur = await v1("/v1/media/" + encodeURIComponent(m.id));
        if (gen !== imagesGen || !entry._jobTimer) return;
        if (cur.status === "failed") throw new Error(cur.error || "processing failed");
        if (cur.status === "ready") { done(cur); return; }
        const prevPhase = entry.phase, prevPct = entry.pct;
        const stage = (cur.processing && cur.processing.stage) || "queued";
        entry.phase = stage === "uploading" ? "storing" : stage;
        entry.pct = (cur.processing && cur.processing.progress) || 0;
        if (entry.phase !== prevPhase || entry.pct !== prevPct) updateVideoProgress(entry);
      } catch (err) {
        clearInterval(entry._jobTimer); entry._jobTimer = null;
        failVideo(entry, gen, err.message);
      }
    }, 1000);
  };
  const fd = new FormData();
  fd.append("preset", entry._preset || "1080p");
  fd.append("file", entry._file);
  xhr.send(fd);
}

function failVideo(entry, gen, msg) {
  if (gen !== imagesGen) return;
  entry.phase = "failed"; entry.error = msg; entry.pct = 0;
  entry._xhr = null; entry._jobTimer = null;
  renderImages();
}

export function renderImages() {
  const c = $("#images"); c.innerHTML = "";
  state.images.forEach((img, i) => {
    if (img.video) {
      const tile = el("div", { class: "thumb vthumb" });
      if (img.phase === "ready") {
        const vAttrs = { src: img.url, preload: "metadata", muted: "muted", controls: "controls", playsinline: "playsinline" };
        if (img.poster_url) vAttrs.poster = img.poster_url;
        tile.append(el("video", vAttrs),
          el("input", { type: "text", placeholder: "alt text", value: img.alt,
            oninput: e => { img.alt = e.target.value; renderPreview(); } }),
          el("div", { class: "imgsize", text: fmtBytes(img.size_bytes) + " · " + fmtDuration(img.duration_secs) }));
      } else if (img.phase === "failed") {
        const fr = el("div", { class: "vprog" },
          el("div", { class: "vprog-label vfail", text: "failed: " + (img.error || "unknown") }));
        if (img._file) {
          fr.append(el("button", { class: "rm", type: "button", text: "retry",
            onclick: () => { img.phase = "uploading"; img.pct = 0; img.error = ""; renderImages(); startVideoUpload(img); } }));
        }
        tile.append(fr);
      } else {
        const label = el("div", { class: "vprog-label", text: img.phase + " " + Math.round((img.pct || 0) * 100) + "%" });
        const fill = el("div", { class: "vprog-fill", style: "width:" + Math.round((img.pct || 0) * 100) + "%" });
        img._labelEl = label; img._fillEl = fill; img._renderedPhase = img.phase;
        tile.append(el("div", { class: "vprog" }, label, el("div", { class: "vprog-bar" }, fill)));
      }
      tile.append(el("button", { class: "rm", type: "button", text: "remove",
        onclick: () => {
          if (img._xhr) img._xhr.abort();           // mid-upload: cancel the transfer
          if (img._jobTimer) clearInterval(img._jobTimer); // job continues server-side by design
          state.images.splice(i, 1); renderImages();
        } }));
      c.append(tile);
      return;
    }
    const thumb = el("img", { src: img.url, alt: "",
      onload: e => {
        // Capture intrinsic dims once; the preview sends them as fit-planning
        // metadata. Guard prevents a render loop.
        if (!img.dim && e.target.naturalWidth) {
          img.dim = e.target.naturalWidth + "x" + e.target.naturalHeight;
          renderPreview();
        }
      } });
    const kids = [thumb,
      el("input", { type: "text", placeholder: "alt text", value: img.alt,
        oninput: e => { img.alt = e.target.value; renderPreview(); } })];
    // Size line + compress preset menu — fresh attachments only (restored
    // draft images are Blossom references without local bytes).
    if (img.orig) {
      const sizeTxt = img.file !== img.orig
        ? `${fmtBytes(img.orig.size)} → ${fmtBytes(img.file.size)}`
        : fmtBytes(img.file.size);
      kids.push(el("div", { class: "imgsize", text: sizeTxt }));
      const sel = el("select", { class: "preset", title: "compress",
        onchange: async ev => {
          const v = ev.target.value;
          // A pending compress for this attachment is now superseded no
          // matter what was picked — original included.
          if (img._compressAbort) { img._compressAbort.abort(); img._compressAbort = null; }
          // Until the new file is uploaded, the post must not take the old one.
          if (img._uploadAbort) img._uploadAbort.abort();
          Object.assign(img, { media_id: "", _uploadFor: null, _upload: null });
          const pick = v === "original" ? Promise.resolve(img.orig) : compressFile(img, img.orig, v);
          img._compressing = pick;
          try {
            const f = await pick;
            if (img._compressing === pick) img._compressing = null;
            URL.revokeObjectURL(img.url);
            img.file = f; img.url = URL.createObjectURL(f); img.preset = v;
            img.size_bytes = f.size; img.mime = f.type; img.dim = "";
            uploadImage(img);
            renderImages();
          } catch (err) {
            if (img._compressing === pick) img._compressing = null;
            if (err.name === "AbortError") return; // superseded by a newer pick
            flash("Compress failed: " + err.message);
            ev.target.value = img.preset;
            uploadImage(img); // the file it keeps
          }
        } });
      for (const [v, label] of [["original", "original"], ["large", "large ≤2048px"],
                                ["medium", "medium ≤1600px"], ["small", "small ≤1080px"]]) {
        sel.append(el("option", { value: v, text: label }));
      }
      sel.value = img.preset || "original";
      kids.push(sel);
    } else if (img.size_bytes) {
      kids.push(el("div", { class: "imgsize", text: fmtBytes(img.size_bytes) }));
    }
    if (img.upload_error) {
      kids.push(el("div", { class: "imgsize bad", text: "upload failed: " + img.upload_error }),
        el("button", { class: "rm", type: "button", text: "retry upload",
          onclick: () => { img.upload_error = ""; uploadImage(img); renderImages(); } }));
    } else if (!img.file && !img.media_id) {
      // A draft attachment the server couldn't turn into media to post.
      kids.push(el("div", { class: "imgsize bad", text: "can't be posted: attach it again" }));
    }
    kids.push(el("button", { class: "rm", type: "button", text: "remove",
      onclick: () => {
        if (img._compressAbort) img._compressAbort.abort();
        if (img._uploadAbort) img._uploadAbort.abort();
        if (img.file) URL.revokeObjectURL(img.url);
        state.images.splice(i, 1); renderImages();
      } }));
    c.append(el("div", { class: "thumb" }, ...kids));
  });
  const vp = $("#vidpreset"); if (vp) vp.hidden = state.images.some(i => i.video);
  renderEditor(); // the attachments on its cards
  renderMeta(); renderPreview();
}

// ---------------------------------------------------------------------------
// Meta / preflight summary
// ---------------------------------------------------------------------------

function renderMeta() {
  const mm = $("#mastermeta");
  if (mm) mm.textContent = `${state.master.length} chars · ${wcount(state.master)} words`;
  const sum = $("#summary");
  if (!sum) return;
  sum.innerHTML = "";
  const plats = [...state.platforms];
  if (plats.length === 0) { sum.append(el("span", { class: "bad", text: "⚠ no targets selected" })); return; }
  sum.append(el("span", { text: `→ ${plats.length} target${plats.length > 1 ? "s" : ""} · ${state.images.length} image${state.images.length === 1 ? "" : "s"}` }));
  // A refusal the server would give on Post (a 422), from the last plan.
  const err = planError();
  const refused = err && err.status === 422;
  sum.append(refused
    ? el("span", { class: "warn", text: "⚠ " + err.message })
    : el("span", { class: "ok", text: "ready ✓" }));
}

// ---------------------------------------------------------------------------
// Schedule UI — ported verbatim from the pre-module monolith
// ---------------------------------------------------------------------------

function schedNote(v) {
  const d = new Date(v);
  if (isNaN(d.getTime())) return "";
  const paris = d.toLocaleString("fr-FR", { timeZone: "Europe/Paris", weekday: "short", day: "2-digit", month: "short", hour: "2-digit", minute: "2-digit" });
  const mins = Math.round((d.getTime() - Date.now()) / 60000);
  let rel;
  if (mins < 1) rel = "now";
  else if (mins < 60) rel = `in ${mins} min`;
  else if (mins < 1440) rel = `in ${Math.round(mins / 60)}h`;
  else rel = `in ${Math.round(mins / 1440)}d`;
  return `⏳ fires ${rel} · ${paris}`;
}

function updateSched() {
  const v = $("#schedat").value;
  $("#submit").textContent = v ? "Schedule" : "Post";
  $("#schedclear").hidden = !v;
  const note = $("#schednote");
  if (!v) { note.textContent = ""; note.className = "sched-note muted"; return; }
  const d = new Date(v);
  const past = !isNaN(d.getTime()) && d.getTime() <= Date.now();
  note.textContent = past ? "⚠ that time is in the past" : schedNote(v);
  note.className = "sched-note " + (past ? "bad" : "set");
}

// ---------------------------------------------------------------------------
// Submit — ported verbatim from the pre-module monolith
// ---------------------------------------------------------------------------

// resetComposer clears the draft out of the composer after it's been committed
// (posted or scheduled): text, images, interaction, and per-platform overrides
// all return to empty, and every dependent view is refreshed so no stale text,
// thumbnail, count, thread badge, or preview lingers.
export function resetComposer() {
  clearImages();
  state.master = "";
  state.breaks = [];
  state.anchors = {};
  state.tab = "shared";
  state.interaction = null;
  for (const p of Object.keys(state.ov)) state.ov[p] = defaultOv(p);
  const sched = $("#schedat"); if (sched) { sched.value = ""; updateSched(); }
  // A posted draft's tags don't carry over to the next one.
  const tags = document.querySelector("#draft-tags-input .tag-chips"); if (tags) tags.replaceChildren();
  renderImages();
  refreshCounts();
  refreshTargets();
  renderCards();
  renderMeta();
  renderInteractionUI();
  renderPreview();
}

// The last submit that may not have reached the server: its body and its
// Idempotency-Key. Posting the same body again reuses the key, so a post the
// server did save (the answer was lost) isn't posted twice.
const lastSends = {};

// sendPost sends a POST /v1/posts body with an Idempotency-Key and answers
// the post (202: saved; the workers send it). slot keeps the composer's and
// the Interact tab's retries apart.
export async function sendPost(body, slot = "compose") {
  const json = JSON.stringify(body);
  let lastSend = lastSends[slot];
  if (!lastSend || lastSend.json !== json) lastSend = lastSends[slot] = { json, key: newKey() };
  const key = lastSend.key;
  let post;
  try {
    post = await v1("/v1/posts", { method: "POST", body, headers: { "Idempotency-Key": key } });
  } catch (e) {
    // An answer means the server decided (refused, or the key is in use):
    // a changed body gets a new key anyway. Without one, keep the key.
    if (e.status && e.status !== 409 && lastSends[slot] === lastSend) delete lastSends[slot];
    throw e;
  }
  if (lastSends[slot] === lastSend) delete lastSends[slot];
  return post;
}

// missingAccounts names the selected platforms with no account to post from.
function missingAccounts() {
  return ORDER.filter(p => state.platforms.has(p) && !accountFor(p)).map(p => META[p].label);
}

// mediaReady waits for the attachments' uploads; false (with a note) when
// one failed or can't be posted.
export async function mediaReady() {
  // A preset being applied makes a new file to upload first.
  await Promise.all(state.images.map(i => i._compressing && i._compressing.catch(() => {})));
  await Promise.all(state.images.map(i => (i.file && !i.media_id ? uploadImage(i) : null)));
  const bad = state.images.filter(i => !i.media_id);
  if (bad.length) {
    // A file can be uploaded again; a draft's attachment without one can't.
    flash(bad.some(i => !i.file && !i._file)
      ? "An attachment from the draft can't be posted: remove it and attach it again"
      : bad.length === 1 ? "An image isn't uploaded: retry or remove it" : bad.length + " images aren't uploaded: retry or remove them");
    renderImages();
    return false;
  }
  return true;
}

// doPost posts (or schedules, or replies/quotes: v1PostBody reads the mode)
// what the composer holds.
async function doPost() {
  const btn = $("#submit");
  const it = state.interaction;
  const label = it ? (it.action === "quote" ? "Quote" : "Reply") : ($("#schedat").value ? "Schedule" : "Post");
  btn.disabled = true;
  btn.textContent = it ? label + "ing…" : $("#schedat").value ? "Scheduling…" : "Posting…";
  try {
    if (!(await mediaReady())) return;
    const draftId = state.activeDraftId;
    const body = v1PostBody();
    // The server uses the draft up once the post is saved.
    if (draftId) body.draft = draftId;
    let post;
    try {
      post = await sendPost(body);
    } catch (e) {
      // Deleted meanwhile (another tab posted it): post without it.
      const gone = e.problem && (e.problem.errors || []).some(fe => fe.pointer === "/draft" && fe.code === "unknown_draft");
      if (!gone) throw e;
      delete body.draft;
      post = await sendPost(body);
    }
    if (post.status === "scheduled" && post.scheduled_at) {
      flash("Scheduled for " + new Date(post.scheduled_at).toLocaleString("fr-FR", { timeZone: "Europe/Paris" }));
    } else {
      setInflight(post.id);
      openProgressModal(post.id, post);
    }
    state.activeDraftId = null;
    state.draftETag = "";
    state.dirty = false;
    // Committed → empty the composer so the posted text and images don't
    // linger as if still unsent (and leave a reply or quote).
    if (it) exitInteraction(); else resetComposer();
    import("./drafts_recovery.js").then(m => m.clearRecovery());
    import("./drafts.js").then(m => m.loadDraftList && m.loadDraftList());
  } catch (e) {
    flash("Error: " + e.message);
  } finally {
    // Label from the composer as it is now: a commit leaves interaction mode
    // and clears #schedat.
    btn.disabled = false;
    btn.textContent = state.interaction ? (state.interaction.action === "quote" ? "Quote" : "Reply")
      : $("#schedat").value ? "Schedule" : "Post";
  }
}

// submit checks what the server would refuse before posting: a video still
// processing, a platform with no account. Text over a platform's limit is
// not one: the server threads it (the preview shows how).
function submit() {
  if (state.images.some(i => i.video && i.phase !== "ready")) {
    flash(state.images.some(i => i.video && i.phase === "failed")
      ? "The video failed — retry or remove it first"
      : "Wait for the video to finish processing");
    return;
  }
  if (state.platforms.size === 0) { flash("Select at least one platform."); return; }
  const missing = missingAccounts();
  if (missing.length) { flash("No account to post to " + missing.join(", ") + " is configured"); return; }
  doPost();
}

// ---------------------------------------------------------------------------
// Progress modal — a post's targets as they send
// ---------------------------------------------------------------------------

// progressClass maps a /v1 status onto the task tree's row classes.
function progressClass(s) {
  if (s === "published") return "success";
  if (s === "ok" || s === "failed" || s === "partial" || s === "skipped") return s;
  if (s === "publishing" || s === "sending") return "running";
  if (s === "unknown" || s === "needs_attention") return "partial";
  if (s === "canceled" || s === "missed") return "skipped";
  return "queued";
}

// targetDetail is a target row's note: how far a thread got, or what went
// wrong and whether it is retried.
function targetDetail(t) {
  const items = t.items || [];
  const failed = items.find(it => it.status === "failed" || it.status === "unknown");
  if (failed) {
    const retry = failed.next_attempt_at && !t.gave_up_at ? " · retrying" : "";
    if (failed.status === "unknown") return "may have been posted: check it in History";
    return (failed.error || "failed") + retry;
  }
  if (items.length > 1) {
    const sent = items.filter(it => it.status === "published" || it.status === "partial").length;
    return `${sent}/${items.length} posts`;
  }
  if (t.status === "canceled") return "canceled";
  return "";
}

// openProgressModal shows a post's targets as they send: the post's events
// (GET /v1/posts/{id}/events) wake a re-read until it stops publishing.
// Closing only stops watching — the post goes on server-side. first is the
// post as the create answered it, shown at once.
export function openProgressModal(postId, first) {
  const id = postId.startsWith("pst_") ? postId : "pst_" + postId;
  const bk = el("div", { class: "modal-bk" });
  const card = el("div", { class: "modal pmodal" });
  const title = el("p", { class: "pm-title" });
  const tree = el("div", { class: "tasktree" });
  const foot = el("div", { class: "pm-foot" });
  card.append(title, tree, foot);
  bk.append(card);
  document.body.append(bk);

  let stop = null;
  const close = () => { if (stop) stop(); stop = null; clearInflight(); bk.remove(); };

  const render = (post) => {
    const settled = post.status !== "publishing";
    title.textContent = !settled ? "Posting…"
      : post.status === "published" ? "✓ Posted"
      : post.status === "failed" ? "⚠ Posting failed"
      : post.status === "canceled" ? "⊘ Canceled"
      : post.status === "missed" ? "⏰ Not posted (missed)" : "⚠ Posted with issues";
    tree.innerHTML = "";
    for (const t of (post.targets || [])) {
      tree.append(el("div", { class: "trow lvl1 st-" + progressClass(t.status) },
        el("span", { class: "ic" }),
        el("span", { text: (META[t.type] && META[t.type].label) || t.type }),
        el("span", { class: "meta", text: targetDetail(t) })));
      const ds = (t.items && t.items[0] && t.items[0].deliveries) || [];
      for (const d of ds) {
        tree.append(el("div", { class: "trow lvl2 st-" + progressClass(d.status) },
          el("span", { class: "ic" }),
          el("span", { text: d.endpoint.replace(/^wss?:\/\//, "") }),
          el("span", { class: "meta", text: d.message || "" })));
      }
    }
    foot.innerHTML = "";
    if (settled) {
      clearInflight();
      if (post.status !== "published") {
        foot.append(el("a", { class: "pm-link", href: "#", text: "Open in history ↗",
          onclick: (e) => {
            e.preventDefault();
            close();
            document.querySelector('.tab[data-view="history"]')?.click();
            openDetail(post.id);
          } }));
      }
      foot.append(el("button", { class: "pm-btn primary", type: "button", text: "Done", onclick: close }));
    } else {
      foot.append(el("button", { class: "pm-btn", type: "button", text: "Close", onclick: close }));
    }
  };

  if (first && first.targets && first.targets.some(t => t.items)) render(first);
  else { title.textContent = "Posting…"; foot.append(el("button", { class: "pm-btn", type: "button", text: "Close", onclick: close })); }
  let last = first;
  stop = followPost(id, {
    onPost: p => { last = p; render(p); },
    // Following gives up after a while: say so rather than spin.
    onStop: () => {
      if (!bk.isConnected || (last && last.status !== "publishing")) return;
      clearInflight();
      title.textContent = "Still sending: History shows how it goes";
    },
    until: p => p.status !== "publishing",
    onGone: close, // a post that's gone (deleted, or from before an upgrade)
  });
}

// ---------------------------------------------------------------------------
// Init
// ---------------------------------------------------------------------------

export function composeInit() {
  // A new plan from the server: thread counts, the notice and the summary
  // read it.
  document.addEventListener("plan", () => { refreshTargets(); renderMeta(); renderPreview(); });
  editorInit({
    changed: () => { refreshCounts(); refreshTargets(); renderMeta(); renderPreview(); },
    tab: t => { if (t !== "shared") state.focus = t; renderPreview(); },
  });
  $("#addimg").addEventListener("click", () => $("#imgfile").click());
  $("#imgfile").addEventListener("change", async e => {
    const file = e.target.files[0]; if (!file) return;
    e.target.value = "";
    const isVid = /^video\//.test(file.type) || /\.(mp4|mov|webm)$/i.test(file.name || "");
    if (isVid) { attachVideo(file); return; }
    if (state.images.some(i => i.video)) { flash("Remove the video first (one video OR images per post)"); return; }
    if (state.images.length >= 10) { flash("Max 10 images"); return; }
    // orig is kept so presets always re-derive from the as-attached file (no
    // generational quality stacking) and "original" is a true client-side revert.
    const entry = { file, orig: file, url: "", alt: "", preset: "original",
                    size_bytes: file.size, mime: file.type, dim: "" };
    const gen = imagesGen;
    if (isHeic(file)) {
      // Chrome can't render HEIC previews — convert server-side immediately so
      // the thumbnail works and nothing downstream ever sees HEIC. The
      // converted JPEG becomes this entry's "original".
      try {
        entry.file = await compressFile(entry, file, "convert");
        entry.orig = entry.file;
        entry.size_bytes = entry.file.size; entry.mime = entry.file.type;
      } catch (err) { flash("HEIC convert failed: " + err.message); return; }
    }
    if (gen !== imagesGen) return; // composer was cleared/replaced mid-convert
    if (state.images.length >= 10) { flash("Max 10 images"); return; }
    entry.url = URL.createObjectURL(entry.file);
    entry.id = newKey();
    state.images.push(entry);
    uploadImage(entry);
    renderImages();
  });
  $("#schedat").addEventListener("input", updateSched);
  $("#schedclear").addEventListener("click", () => { $("#schedat").value = ""; updateSched(); });
  $("#submit").addEventListener("click", submit);
  document.getElementById("threadnum")?.addEventListener("change", () => { refreshTargets(); renderPreview(); });
  renderChips(); renderImages(); renderCards(); renderMeta(); renderPreview();
  reattachInflight();
}

// reattachInflight reopens the progress of a post that was sending when the
// page was left, if it still is: asking the list (not the post) keeps a post
// that is gone from answering 404.
async function reattachInflight() {
  const pending = getInflight();
  if (!pending) return;
  const id = pending.startsWith("pst_") ? pending : "pst_" + pending;
  let page;
  try { page = await v1("/v1/posts?status=publishing&limit=100"); } catch { return; }
  if (getInflight() !== pending) return; // a new post took its place meanwhile
  const p = ((page && page.data) || []).find(x => x.id === id);
  if (p) openProgressModal(id);
  else clearInflight();
}

// newKey is a random id: crypto.randomUUID needs a secure context (https or
// localhost); a plain-http LAN install falls back to getRandomValues.
export function newKey() {
  if (crypto.randomUUID) return crypto.randomUUID();
  const b = crypto.getRandomValues(new Uint8Array(16));
  b[6] = (b[6] & 0x0f) | 0x40; b[8] = (b[8] & 0x3f) | 0x80;
  const h = [...b].map(x => x.toString(16).padStart(2, "0")).join("");
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
}
