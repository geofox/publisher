"use strict";
import { el, $, gcount, wcount, flash, confirmModal, META, ORDER } from "./common.js";
import { state, effectiveText, postedText, buildSpec, buildInteractSpec, defaultOv, setInflight, getInflight, clearInflight, clearImages, imagesGen, bumpImagesGen, masterParts } from "./state.js";
import { renderPreview, mediaMax, previewMedia } from "./preview.js";
import { resultRow, openDetail } from "./history.js";
import { brandTile, icon, PLATFORM_META } from "./brands.js";

// autoGrow sizes a textarea to its content (up to its CSS max-height, then it
// scrolls). No-op when the element is hidden (scrollHeight is 0 then).
function autoGrow(ta) {
  if (!ta || ta.offsetParent === null) return;
  ta.style.height = "auto";
  ta.style.height = ta.scrollHeight + "px";
}

export function markDirty() {
  state.dirty = true;
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
// Auto-threading estimate (client-side, mirrors Go dispatch + the design's
// splitThread: word-boundary wrap reserving room for a " n/n" suffix). Used for
// the live thread badge, banner, and the review sheet's segments; the live
// preview pane still fetches the authoritative split from /api/thread-preview.
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

function numberingOn() { return document.getElementById("threadnum")?.checked ?? true; }

// splitMarkers mirrors internal/thread.splitMarkers: break on lines that are
// solely "---", trimming each block and dropping empties. Manual breaks let the
// operator force a thread (and shape it via the Edit-split sheet).
function splitMarkers(text) {
  const blocks = []; let cur = [];
  for (const ln of text.split(/\r?\n/)) {
    if (ln.trim() === "---") { blocks.push(cur.join("\n")); cur = []; }
    else cur.push(ln);
  }
  blocks.push(cur.join("\n"));
  return blocks.map(b => b.trim()).filter(Boolean);
}

// splitLength wraps one block on word boundaries, reserving room for a " n/n"
// suffix when numbering is on. limit 0 (Nostr) → never length-splits.
function splitLength(text, limit, number) {
  if (!limit || gcount(text) <= limit) return [text];
  const reserve = number ? 6 : 0; // " 99/99"
  const eff = Math.max(limit - reserve, 1);
  const parts = []; let cur = "";
  for (const w of text.split(/(\s+)/)) {
    if (gcount(cur + w) > eff && cur.trim()) { parts.push(cur.trim()); cur = w.replace(/^\s+/, ""); }
    else cur += w;
  }
  if (cur.trim()) parts.push(cur.trim());
  return parts.length ? parts : [text];
}

// splitThread mirrors the backend: honor manual --- markers first, then
// length-split each block. A chain forms when there are multiple blocks OR a
// single block exceeds the limit.
function splitThread(text, limit, number = true) {
  const blocks = splitMarkers(text);
  if (blocks.length > 1) {
    const out = [];
    for (const b of blocks) out.push(...splitLength(b, limit, number));
    return out.length ? out : [text];
  }
  return splitLength(text, limit, number);
}

function threadInfoFor(p) {
  const parts = splitThread(effectivePostedText(p), META[p].limit, numberingOn());
  // Media overflow forms a chain too: images beyond the platform cap spill
  // into appended image-only posts (mirrors thread.PlanMedia's totals).
  const mmax = mediaMax(p);
  const mediaPosts = mmax > 0 ? Math.ceil(previewMedia(p).length / mmax) : 0;
  const posts = Math.max(parts.length, mediaPosts, 1);
  return { posts, threaded: posts > 1, parts };
}

// targetCount → {text, cls} for a platform's live count. State colors mirror the
// design: under 90% → ink2 (no class), >90% → amber, over → indigo (will thread,
// informational), no-limit → teal ∞.
function targetCount(p) {
  const limit = META[p].limit, n = gcount(effectivePostedText(p));
  if (!limit) return { text: "∞", cls: "inf", n };
  const pct = n / limit;
  return { text: `${n}/${limit}`, cls: n > limit ? "over" : pct > 0.9 ? "near" : "", n };
}

// Token highlighter for the thread sheet; .pv-tok picks up the platform tint from
// the --pa set by the p-{platform} ancestor class.
const SHEET_TOKEN = /(https?:\/\/[^\s]+|#[\w]+|@[\w.]+|\b[a-z0-9-]+\.(?:one|com|social|net|io)\b[^\s]*)/gi;
function highlightTokens(text) {
  const out = []; let last = 0, m; SHEET_TOKEN.lastIndex = 0;
  while ((m = SHEET_TOKEN.exec(text)) !== null) {
    if (m.index > last) out.push(document.createTextNode(text.slice(last, m.index)));
    out.push(el("span", { class: "pv-tok", text: m[0] }));
    last = m.index + m[0].length;
  }
  if (last < text.length) out.push(document.createTextNode(text.slice(last)));
  return out;
}

// ---------------------------------------------------------------------------
// Targets — selectable rows with brand tile, live count, switch, thread badge
// ---------------------------------------------------------------------------

function iosSwitch(on) { return el("div", { class: "ios-switch" + (on ? " on" : "") }, el("div", { class: "knob" })); }

function threadBadge(p) {
  const ti = threadInfoFor(p);
  const badge = el("span", { class: "thread-badge",
    onclick: e => { e.stopPropagation(); openThreadSheet(p); } });
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
    const meta = PLATFORM_META[p] || {};
    const tc = targetCount(p);
    const ti = threadInfoFor(p);

    const top = el("div", { class: "target-top" }, el("span", { class: "t-name", text: META[p].label }));
    if (it) top.append(el("span", { class: "t-tag" + (locked ? " native" : ""), text: locked ? "native" : "copy" }));
    if (on && ti.threaded) top.append(threadBadge(p));

    const row = el("div", {
      class: "target-row p-" + p + (on ? "" : " off") + (locked ? " locked" : ""),
      onclick: () => {
        if (locked) return; // source platform stays selected in interaction mode
        on ? state.platforms.delete(p) : state.platforms.add(p);
        if (!state.platforms.has(state.focus)) state.focus = p;
        renderChips(); renderCards(); renderPreview();
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
// operator's real profile (from GET /api/identity). Called once after boot;
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

// renderThreadNotice shows the calm informational banner above POST TO whenever
// ≥1 selected target overflows; tapping it opens the review sheet.
function renderThreadNotice() {
  const host = $("#threadnotice"); if (!host) return;
  host.innerHTML = "";
  const threaded = ORDER.filter(p => state.platforms.has(p) && threadInfoFor(p).threaded);
  if (!threaded.length) return;
  const names = threaded.map(p => `${META[p].label} (${threadInfoFor(p).posts})`).join(", ");
  const banner = el("div", { class: "thread-notice", onclick: () => openThreadSheet(threaded[0]) });
  const ic = icon("thread", { size: 20, sw: 1.8 }); if (ic) banner.append(ic);
  banner.append(el("div", { class: "tn-body" },
    el("div", { class: "tn-title", text: `Auto-threading on ${names}` }),
    el("div", { class: "tn-sub", text: "Over the limit — we'll split it into a numbered thread." }),
  ));
  const ch = icon("chevron", { size: 16 }); if (ch) banner.append(ch);
  host.append(banner);
}

// openThreadSheet presents the glass bottom sheet reviewing how an over-limit
// draft splits for one platform — and, via "Edit split", lets the operator
// reshape the breakpoints (saved back into the source text as \n---\n markers,
// which the backend honors verbatim on send).
function openThreadSheet(p) {
  const meta = META[p];
  const it = state.interaction;
  const ov = state.ov[p];
  // Edit-split writes back to the text that produced the segments: a per-platform
  // override if one is set, else the master draft. Disabled in interaction mode
  // (the previewed text there is an assembled reproduction, not directly owned).
  const editable = !it;
  const srcIsOverride = ov.text != null;
  const number = numberingOn();
  const sourceText = effectivePostedText(p);

  let segs = splitThread(sourceText, meta.limit, number);
  let editing = false;
  let segInputs = [];

  const bk = el("div", { class: "sheet-bk" });
  const close = () => bk.remove();
  bk.addEventListener("click", e => { if (e.target === bk) close(); });
  const sheet = el("div", { class: "sheet p-" + p });
  bk.append(sheet);

  const collectRaw = () => segInputs.map(t => t.value);

  // segLen = a segment's length including the " n/n" numbering it will gain on
  // send; over = whether that pushes it past the limit.
  function segLen(seg, total) {
    const n = number ? gcount(seg) + (` ${total}/${total}`).length : gcount(seg);
    return { n, over: meta.limit ? n > meta.limit : false };
  }

  function viewTimeline() {
    const tl = el("div", { class: "seg-timeline" });
    segs.forEach((seg, i) => {
      const { n, over } = segLen(seg, segs.length);
      const rail = el("div", { class: "seg-rail" }, el("div", { class: "seg-node", text: String(i + 1) }));
      if (i < segs.length - 1) rail.append(el("div", { class: "seg-conn" }));
      const txt = el("div", { class: "sc-text" }, ...highlightTokens(seg));
      if (number) txt.append(el("span", { class: "sc-num", text: ` ${i + 1}/${segs.length}` }));
      const card = el("div", { class: "seg-card" }, txt,
        el("div", { class: "sc-meta" },
          el("span", { class: "sc-count" + (over ? " over" : ""), text: meta.limit ? `${n}/${meta.limit}` : `${n} chars` }),
          meta.limit ? el("div", { class: "track" }, el("i", { style: `width:${Math.min(n / meta.limit * 100, 100)}%` })) : null,
        ),
      );
      tl.append(el("div", { class: "seg-item" }, rail, card));
    });
    return tl;
  }

  function editTimeline() {
    segInputs = [];
    const tl = el("div", { class: "seg-timeline" });
    segs.forEach((seg, i) => {
      const rail = el("div", { class: "seg-rail" }, el("div", { class: "seg-node", text: String(i + 1) }));
      if (i < segs.length - 1) rail.append(el("div", { class: "seg-conn" }));

      const count = el("span", { class: "sa-count" });
      const updateCount = (val) => {
        const { n, over } = segLen(val, segs.length);
        count.textContent = meta.limit ? `${n}/${meta.limit}` : `${n} chars`;
        count.className = "sa-count" + (over ? " over" : "");
      };
      const ta = el("textarea", { class: "seg-edit", rows: 3,
        oninput: e => updateCount(e.target.value) });
      ta.value = seg; updateCount(seg);
      segInputs.push(ta);

      const actions = el("div", { class: "seg-actions" });
      if (i > 0) actions.append(el("button", { class: "sa-btn", type: "button", text: "⤺ merge up",
        onclick: () => { const cur = collectRaw(); cur[i - 1] = (cur[i - 1].trimEnd() + " " + cur[i].trimStart()).trim(); cur.splice(i, 1); segs = cur; render(); } }));
      actions.append(el("button", { class: "sa-btn", type: "button", text: "✂ break here",
        onclick: () => { const cur = collectRaw(); const caret = ta.selectionStart ?? cur[i].length; cur.splice(i, 1, cur[i].slice(0, caret), cur[i].slice(caret)); segs = cur; render(); } }));
      actions.append(count);

      const card = el("div", { class: "seg-card" }, ta, actions);
      tl.append(el("div", { class: "seg-item" }, rail, card));
    });
    return tl;
  }

  function save() {
    const next = collectRaw().map(s => s.trim()).filter(Boolean);
    if (!next.length) { close(); return; }
    const joined = next.join("\n---\n");
    if (srcIsOverride) ov.text = joined;
    else { state.master = joined; const m = $("#master"); if (m) { m.value = joined; autoGrow(m); } }
    markDirty();
    renderChips(); renderCards(); refreshCounts(); renderMeta(); renderPreview();
    close();
    flash(`Thread split saved · ${next.length} posts`);
  }

  function render() {
    sheet.innerHTML = "";
    sheet.append(el("div", { class: "sheet-grabber" }));
    const total = gcount(sourceText);
    const head = el("div", { class: "sheet-head" },
      brandTile(p, { size: 30 }),
      el("div", { class: "sh-main" },
        el("div", { class: "sh-title", text: `${meta.label} thread` }),
        el("div", { class: "sh-sub", text: editing
          ? `${segs.length} posts · edit text, break, or merge`
          : (meta.limit ? `${total} chars over ${meta.limit} → ${segs.length} posts` : `${segs.length} posts`) }),
      ),
    );
    if (editable) head.append(el("button", { class: "sh-edit", type: "button",
      text: editing ? "Done" : "Edit split",
      onclick: () => { if (editing) segs = collectRaw().map(s => s.trim()).filter(Boolean); editing = !editing; render(); } }));
    sheet.append(head);

    sheet.append(editing ? editTimeline() : viewTimeline());

    const foot = el("div", { class: "sheet-foot" });
    const lk = icon("link", { size: 16 }); if (lk) foot.append(lk);
    foot.append(el("span", { text: editing
      ? "Manual breaks apply to every network · numbering added on send"
      : "Replies chain automatically · numbering added on send" }));
    sheet.append(foot);

    sheet.append(editing
      ? el("button", { class: "sheet-cta", type: "button", text: "Save split", onclick: save })
      : el("button", { class: "sheet-cta", type: "button", text: "Looks good — keep thread", onclick: close }));
  }

  render();
  document.body.append(bk);
}

// ---------------------------------------------------------------------------
// Interaction mode — source banner + constrained chips
// ---------------------------------------------------------------------------

// startInteraction puts Compose into interaction mode for a resolved source post
// (src is a /api/resolve SourceRef) and switches to the Compose tab. action is
// "reply" | "quote".
export function startInteraction(src, action) {
  const begin = () => {
    state.interaction = {
      action, platform: src.platform, ref: src.ref,
      sourcePreview: src.preview, sourceURL: src.preview.web_url, sourceAuthor: src.preview.author_handle,
      caps: src.caps, force: false,
      // restored on exit; preserve the pre-interaction selection across a nested
      // re-entry (starting a new interaction while already in one).
      prevPlatforms: state.interaction && state.interaction.prevPlatforms
        ? state.interaction.prevPlatforms : new Set(state.platforms),
    };
    state.master = "";
    clearImages();
    ORDER.forEach((p) => { state.ov[p] = defaultOv(p); }); // drop stale per-platform overrides
    state.platforms = new Set([src.platform]);
    state.focus = src.platform;
    const tab = document.querySelector('.tab[data-view="compose"]');
    if (tab) tab.click();
    const m = $("#master"); if (m) { m.value = ""; autoGrow(m); }
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
  clearImages();
  const m = $("#master"); if (m) { m.value = ""; autoGrow(m); }
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
      state.master = input.text || "";
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
      const m = $("#master"); if (m) { m.value = state.master; autoGrow(m); }
      window.scrollTo({ top: 0, behavior: "smooth" });
      renderImages();
      renderInteractionUI(); // re-renders chips/cards/preview and hides the banner
      flash("Translated draft loaded" + (input.lang ? " · lang " + input.lang : ""));
      return;
    }

    // full spec shape
    state.interaction = input.interaction || null;
    state.master = input.master_text || "";
    clearImages();
    state.platforms = new Set(input.platforms && input.platforms.length ? input.platforms : ORDER);
    if (!state.platforms.has(state.focus)) state.focus = [...state.platforms][0] || "bluesky";
    // reset overrides to defaults, then apply saved overrides
    ORDER.forEach(p => { state.ov[p] = defaultOv(p); });
    if (input.overrides) {
      for (const p of Object.keys(input.overrides)) {
        if (!state.ov[p]) continue;
        Object.assign(state.ov[p], input.overrides[p]);
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
      ordinal: m.ordinal, file: null,
      duration_secs: m.duration_secs || 0,
      video: /^video\//.test(m.mime || ""),
      poster_url: m.poster_url || "",
      // derive a displayable URL from blossom_url for the thumbnail strip
      url: m.blossom_url || "",
      // restored video drafts are already transcoded — render as ready
      phase: /^video\//.test(m.mime || "") ? "ready" : undefined,
      id: m.client_id || crypto.randomUUID(),
    }));
    bumpImagesGen();
    state.activeDraftId = input.id || null;
    state.dirty = false;
    const tab = document.querySelector('.tab[data-view="compose"]');
    if (tab) tab.click();
    const master = $("#master"); if (master) { master.value = state.master; autoGrow(master); }
    window.scrollTo({ top: 0, behavior: "smooth" });
    renderImages();
    renderInteractionUI(); // re-renders chips/cards/preview and hides the banner
    flash("Draft loaded");
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

// renderInteractionUI re-renders the compose chrome for the current mode.
function renderInteractionUI() {
  renderSrcBanner();
  const sched = $("#schedrow"); if (sched) sched.hidden = state.interaction != null;
  $("#submit").textContent = state.interaction ? (state.interaction.action === "quote" ? "Quote" : "Reply") : "Post";
  renderChips(); renderCards(); renderMeta(); renderPreview();
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
  foot.append(el("span", { class: "srcb-hint muted",
    text: it.platform.charAt(0).toUpperCase() + it.platform.slice(1) +
      " gets a native " + it.action + "; toggle other targets to also post a copy there." }));
  if (it.sourceURL) foot.append(el("a", { class: "srcb-link", href: it.sourceURL, target: "_blank", rel: "noopener", text: "open original ↗" }));
  host.append(foot);
  const cap = it.caps && it.caps[it.action];
  if (cap && !cap.allowed) {
    const cb = el("input", { type: "checkbox", onchange: (e) => { it.force = e.target.checked; } });
    cb.checked = !!it.force;
    host.append(el("label", { class: "srcb-force" }, cb,
      el("span", { text: " " + (cap.reason || "blocked") + " — try anyway" +
        (it.platform === "bluesky" ? " (Bluesky may silently drop it)" : "") })));
  }
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
    if (!state.platforms.has(p) || state.ov[p].text != null) continue;
    const card = $(`#cards .ocard.p-${p}`);
    if (!card) continue;
    const limit = META[p].limit;
    const n = gcount(effectivePostedText(p)); // full posted text (reproduction on fan-out) — matches the preview
    const span = card.querySelector(".cnt");
    if (span) { span.textContent = limit ? `${n}/${limit}` : "∞"; span.className = counterClass(n, limit); }
    const ta = card.querySelector("textarea");
    if (ta && document.activeElement !== ta) ta.value = state.master;
  }
}

function overrideCard(p) {
  const ov = state.ov[p], meta = META[p];
  const edited = ov.text != null;
  // textarea/snippet show your editable commentary; the count reflects the full
  // posted text (the reproduction on a fan-out platform), matching the preview.
  const text = effectiveText(p), n = gcount(effectivePostedText(p));
  const card = el("div", { class: "ocard p-" + p + (edited ? " edited" : "") });

  // collapsed summary row (always visible, clickable to expand)
  const cnt = el("span", { class: counterClass(n, meta.limit), text: meta.limit ? `${n}/${meta.limit}` : "∞" });
  const snippet = edited ? `"${text.slice(0, 40)}"` : "mirrors master";
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
    el("span", { class: "ocard-snip muted", text: snippet }),
  );

  // expanded editor + fields
  const resetBtn = el("button", { class: "reset", type: "button", text: "reset to master", hidden: !edited,
    onclick: () => { ov.text = null; renderCards(); refreshTargets(); renderPreview(); } });
  const ta = el("textarea", {
    oninput: e => {
      ov.text = e.target.value;
      const m = gcount(effectivePostedText(p));
      cnt.textContent = meta.limit ? `${m}/${meta.limit}` : "∞";
      cnt.className = counterClass(m, meta.limit);
      card.classList.add("edited");
      resetBtn.hidden = false;
      // refreshTargets so this platform's thread badge + the thread notice track
      // the per-platform text (every other text-change path refreshes them too).
      refreshTargets();
      renderMeta(); renderPreview();
    },
  });
  ta.value = text;
  body.append(ta, fieldsFor(p, ov), resetBtn);

  card.append(summary, body);
  return card;
}

// ---------------------------------------------------------------------------
// Plain micro-label helper + per-platform fields
// ---------------------------------------------------------------------------

function lbl(t, input) { return el("label", {}, el("span", { class: "fl", text: t }), input); }

// langField builds the per-platform language widget. When the operator has
// configured 2+ languages (USER_LANGUAGES env, surfaced via /api/config →
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

function fieldsFor(p, ov) {
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
    f.append(lbl("CW", el("input", { type: "text", value: ov.spoiler_text, oninput: e => { ov.spoiler_text = e.target.value; renderPreview(); } })));
    f.append(lbl("sensitive", el("input", { type: "checkbox", onchange: e => { ov.sensitive = e.target.checked; renderPreview(); } })));
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

// compressFile sends a File through POST /api/media/compress and returns the
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
    const r = await fetch("/api/media/compress", { method: "POST", body: fd, credentials: "same-origin", signal: ctl.signal });
    if (!r.ok) {
      let msg = "HTTP " + r.status;
      try { msg = (await r.json()).error || msg; } catch { /* body not json */ }
      throw new Error(msg);
    }
    const blob = await r.blob();
    const stem = (file.name || "image").replace(/\.[^.]+$/, "");
    return new File([blob], stem + ".jpg", { type: blob.type || "image/jpeg" });
  } finally {
    if (img._compressAbort === ctl) img._compressAbort = null;
  }
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
  entry.id = crypto.randomUUID();
  state.images.push(entry);
  renderImages();
  startVideoUpload(entry);
}

function startVideoUpload(entry) {
  const gen = imagesGen;
  const xhr = new XMLHttpRequest();
  entry._xhr = xhr;
  xhr.open("POST", "/api/media/video?preset=" + encodeURIComponent(entry._preset || "1080p"));
  xhr.upload.onprogress = e => {
    if (e.lengthComputable) { entry.pct = e.loaded / e.total; updateVideoProgress(entry); }
  };
  xhr.onerror = () => failVideo(entry, gen, "upload failed");
  xhr.onload = () => {
    if (gen !== imagesGen) return;
    if (xhr.status !== 202) {
      let msg = "HTTP " + xhr.status;
      try { msg = JSON.parse(xhr.responseText).error || msg; } catch { /* not json */ }
      failVideo(entry, gen, msg);
      return;
    }
    const jobId = JSON.parse(xhr.responseText).job_id;
    entry._xhr = null;
    entry.phase = "queued"; entry.pct = 0; renderImages();
    entry._jobTimer = setInterval(async () => {
      if (gen !== imagesGen) { clearInterval(entry._jobTimer); return; }
      try {
        const r = await fetch("/api/media/video/" + jobId, { credentials: "same-origin" });
        const j = await r.json();
        if (!r.ok) throw new Error(j.error || ("HTTP " + r.status));
        if (j.state === "error") throw new Error(j.error || "transcode failed");
        const prevPhase = entry.phase;
        const prevPct = entry.pct;
        entry.phase = j.state === "uploading" ? "storing" : j.state;
        entry.pct = j.pct || 0;
        if (j.state === "done") {
          clearInterval(entry._jobTimer); entry._jobTimer = null;
          const m = j.media;
          Object.assign(entry, { phase: "ready", pct: 1, blossom_url: m.url, sha256: m.sha256,
            mime: m.mime, dim: m.dim, size_bytes: m.size, duration_secs: m.duration_secs || 0,
            url: m.url, poster_url: m.poster_url || "" });
          entry._file = null;
          renderImages();
        } else if (entry.phase !== prevPhase || entry.pct !== prevPct) {
          updateVideoProgress(entry);
        }
      } catch (err) {
        clearInterval(entry._jobTimer); entry._jobTimer = null;
        failVideo(entry, gen, err.message);
      }
    }, 1000);
  };
  const fd = new FormData();
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
          try {
            const f = v === "original" ? img.orig : await compressFile(img, img.orig, v);
            URL.revokeObjectURL(img.url);
            img.file = f; img.url = URL.createObjectURL(f); img.preset = v;
            img.size_bytes = f.size; img.mime = f.type; img.dim = "";
            renderImages();
          } catch (err) {
            if (err.name === "AbortError") return; // superseded by a newer pick
            flash("Compress failed: " + err.message);
            ev.target.value = img.preset;
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
    if (masterParts().length >= 2 && !state.interaction) {
      const sel = el("select", { class: "place-chip",
        onchange: e => { state.anchors[img.id] = parseInt(e.target.value, 10); markDirty(); renderPreview(); refreshCounts(); } });
      const n = masterParts().length;
      for (let p = 0; p < n; p++) {
        const o = el("option", { value: String(p), text: `▸ part ${p + 1}` });
        if ((state.anchors[img.id] || 0) === p) o.selected = true;
        sel.append(o);
      }
      kids.push(sel);
    }
    kids.push(el("button", { class: "rm", type: "button", text: "remove",
      onclick: () => { if (img._compressAbort) img._compressAbort.abort(); URL.revokeObjectURL(img.url); state.images.splice(i, 1); renderImages(); } }));
    c.append(el("div", { class: "thumb" }, ...kids));
  });
  const vp = $("#vidpreset"); if (vp) vp.hidden = state.images.some(i => i.video);
  renderMeta(); renderPreview();
}

// ---------------------------------------------------------------------------
// Meta / preflight summary
// ---------------------------------------------------------------------------

// overLimitPlatforms returns selected platforms whose effective text exceeds the
// platform limit, as {label, over} — shared by the pre-flight summary and the
// over-limit confirm guard on Post.
function overLimitPlatforms() {
  const out = [];
  for (const p of ORDER) {
    if (!state.platforms.has(p) || !META[p].limit) continue;
    const n = gcount(effectivePostedText(p));
    if (n > META[p].limit) out.push({ label: META[p].label, over: n - META[p].limit });
  }
  return out;
}

function renderMeta() {
  const mm = $("#mastermeta");
  if (mm) mm.textContent = `${state.master.length} chars · ${wcount(state.master)} words`;
  const sum = $("#summary");
  if (!sum) return;
  sum.innerHTML = "";
  const plats = [...state.platforms];
  if (plats.length === 0) { sum.append(el("span", { class: "bad", text: "⚠ no targets selected" })); return; }
  const over = overLimitPlatforms();
  sum.append(el("span", { text: `→ ${plats.length} target${plats.length > 1 ? "s" : ""} · ${state.images.length} image${state.images.length === 1 ? "" : "s"}` }));
  sum.append(over.length
    ? el("span", { class: "warn", text: `⚠ over limit: ${over.map(o => `${o.label} +${o.over}`).join(", ")}` })
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
  state.interaction = null;
  for (const p of Object.keys(state.ov)) state.ov[p] = defaultOv(p);
  const m = $("#master"); if (m) { m.value = ""; autoGrow(m); }
  const sched = $("#schedat"); if (sched) { sched.value = ""; updateSched(); }
  renderImages();
  refreshCounts();
  refreshTargets();
  renderCards();
  renderMeta();
  renderInteractionUI();
  renderPreview();
}

async function doPost() {
  const btn = $("#submit");
  const isScheduled = !!$("#schedat").value;
  btn.disabled = true; btn.textContent = isScheduled ? "Scheduling…" : "Posting…";
  const fd = new FormData();
  fd.append("spec", JSON.stringify(buildSpec()));
  // Only fresh images carry a File; already-uploaded ones (a restored/loaded
  // draft) ride as blossom_url references in the spec and the server re-fetches
  // their bytes. Appending a null file would send a junk "null" form value.
  for (const img of state.images) if (img.file) fd.append("image", img.file);
  try {
    const r = await fetch("/api/post", { method: "POST", body: fd, credentials: "same-origin" });
    const data = await r.json();
    if (!r.ok) throw new Error(data.error || ("HTTP " + r.status));
    if (data.status === "scheduled" && data.scheduled_at) {
      flash("Scheduled for " + new Date(data.scheduled_at).toLocaleString("fr-FR", { timeZone: "Europe/Paris" }));
      $("#schedat").value = ""; updateSched();
    } else if (data.post_id) {
      setInflight(data.post_id);
      openProgressModal(data.post_id);
    } else {
      showResultModal(data); // fallback if a full record came back (no registry)
    }
    // Drafts integration: if we just published a saved draft, clear the
    // active-draft state and the recovery snapshot, then refresh the sidebar.
    if (state.activeDraftId) {
      state.activeDraftId = null;
    }
    state.dirty = false;
    // Committed successfully → empty the composer so the posted text and images
    // don't linger as if still unsent.
    resetComposer();
    import("./drafts_recovery.js").then(m => m.clearRecovery());
    import("./drafts.js").then(m => m.loadDraftList && m.loadDraftList());
  } catch (e) {
    flash("Error: " + e.message);
  } finally { btn.disabled = false; btn.textContent = isScheduled ? "Schedule" : "Post"; }
}

async function doInteract() {
  const it = state.interaction;
  const label = it.action === "quote" ? "Quote" : "Reply";
  const btn = $("#submit");
  btn.disabled = true; btn.textContent = label + "ing…";
  const fd = new FormData();
  fd.append("spec", JSON.stringify(buildInteractSpec()));
  // Only fresh images carry a File; references ride in the spec (see doPost).
  for (const img of state.images) if (img.file) fd.append("image", img.file);
  try {
    const r = await fetch("/api/interact", { method: "POST", body: fd, credentials: "same-origin" });
    const data = await r.json();
    if (!r.ok) throw new Error(data.error || ("HTTP " + r.status));
    if (data.post_id) {
      setInflight(data.post_id);
      openProgressModal(data.post_id);
    } else {
      showResultModal({ post_id: data.id, status: data.status, targets: data.targets }); // fallback
    }
    exitInteraction(); // back to a normal composer after a successful interaction
  } catch (e) {
    flash("Error: " + e.message);
  } finally {
    btn.disabled = false;
    // exitInteraction() (on success) already reset the label via renderInteractionUI;
    // on error we stay in interaction mode, so restore the action label.
    if (state.interaction) btn.textContent = label;
  }
}

// submit guards against posting over a platform's limit: if any selected platform
// is over, confirm first (those targets may be cut/rejected — the rest still post).
// Otherwise post immediately.
function submit() {
  if (state.images.some(i => i.video && i.phase !== "ready")) {
    flash(state.images.some(i => i.video && i.phase === "failed")
      ? "The video failed — retry or remove it first"
      : "Wait for the video to finish processing");
    return;
  }
  if (state.interaction) {
    const it = state.interaction;
    const cap = it.caps && it.caps[it.action];
    if (cap && !cap.allowed && !it.force) {
      flash((cap.reason || "This action is blocked") + " — tick “try anyway” to override");
      return;
    }
    doInteract();
    return;
  }
  if (state.platforms.size === 0) { flash("Select at least one platform."); return; }
  const over = overLimitPlatforms();
  if (over.length) {
    const list = over.map(o => `${o.label} +${o.over}`).join(", ");
    confirmModal({
      title: "Over the character limit",
      body: `${list} — over-limit targets may be cut or rejected (other platforms still post). Post anyway?`,
      confirmText: "Post anyway",
      onConfirm: async () => { await doPost(); return true; },
    });
    return;
  }
  doPost();
}

// ---------------------------------------------------------------------------
// Progress modal — live SSE task tree for a dispatched post
// ---------------------------------------------------------------------------

// openProgressModal renders the live task tree for postId by subscribing to the
// SSE stream, re-rendering on each snapshot. On a terminal status it swaps the
// footer to Done / "open in history". Closing only closes the stream — the post
// continues server-side.
export function openProgressModal(postId) {
  const bk = el("div", { class: "modal-bk" });
  const card = el("div", { class: "modal pmodal" });
  const title = el("p", { class: "pm-title" });
  const tree = el("div", { class: "tasktree" });
  const foot = el("div", { class: "pm-foot" });
  card.append(title, tree, foot);
  bk.append(card);
  document.body.append(bk);

  let es = null;
  const close = () => { if (es) es.close(); es = null; clearInflight(); bk.remove(); };

  const render = (snap) => {
    const terminal = snap.status !== "running" && snap.status !== "queued";
    title.textContent = snap.status === "running" ? "Posting…"
      : snap.status === "success" ? "✓ Posted"
      : snap.status === "failed" ? "⚠ Posting failed" : "⚠ Posted with issues";
    tree.innerHTML = "";
    for (const p of (snap.platforms || [])) {
      tree.append(el("div", { class: "trow lvl1 st-" + p.status },
        el("span", { class: "ic" }),
        el("span", { text: (META[p.platform] && META[p.platform].label) || p.platform }),
        el("span", { class: "meta", text: p.detail || "" })));
      for (const rl of (p.relays || [])) {
        tree.append(el("div", { class: "trow lvl2 st-" + rl.status },
          el("span", { class: "ic" }),
          el("span", { text: rl.url.replace(/^wss?:\/\//, "") }),
          el("span", { class: "meta", text: rl.message || "" })));
      }
    }
    foot.innerHTML = "";
    if (terminal) {
      clearInflight();
      if (snap.status !== "success") {
        foot.append(el("a", { class: "pm-link", href: "#", text: "Open in history ↗",
          onclick: (e) => {
            e.preventDefault();
            close();
            document.querySelector('.tab[data-view="history"]')?.click();
            openDetail(snap.post_id || postId);
          } }));
      }
      foot.append(el("button", { class: "pm-btn primary", type: "button", text: "Done", onclick: close }));
    } else {
      foot.append(el("button", { class: "pm-btn", type: "button", text: "Close", onclick: close }));
    }
  };

  es = new EventSource("/api/posts/" + encodeURIComponent(postId) + "/progress");
  es.onmessage = (ev) => {
    try {
      const snap = JSON.parse(ev.data);
      render(snap);
      const terminal = snap.status !== "running" && snap.status !== "queued";
      if (terminal && es) { es.close(); es = null; }
    } catch {}
  };
  es.onerror = () => { /* no-op: genuine mid-flight network errors auto-reconnect */ };
}

// ---------------------------------------------------------------------------
// Result modal — ported from the pre-module monolith, with switchTab replaced
// by DOM click to avoid compose↔main import cycle
// ---------------------------------------------------------------------------

export function showResultModal(data) {
  const bk = el("div", { class: "modal-bk" });
  const close = () => bk.remove();
  bk.addEventListener("click", e => { if (e.target === bk) close(); });
  const goHistory = async () => {
    close();
    document.querySelector('.tab[data-view="history"]')?.click(); // main.js wires this to switchTab → loadHistory
    await openDetail(data.post_id);
    $("#hdetail").scrollIntoView({ behavior: "smooth", block: "start" });
  };
  const tg = data.targets || [];
  let ok = 0, fail = 0; for (const t of tg) { t.status === "success" ? ok++ : fail++; }
  const card = el("div", { class: "modal", onclick: goHistory });
  card.append(el("button", { class: "modal-x", type: "button", text: "×",
    onclick: e => { e.stopPropagation(); close(); } }));
  const scls = data.status === "success" ? "ok" : data.status === "failed" ? "bad" : "warn";
  card.append(el("p", { class: "modal-title " + scls, text: `result · ${data.status} · ✓${ok} ✗${fail}` }));
  for (const t of tg) card.append(resultRow(t, true));
  card.append(el("p", { class: "modal-hint", text: fail ? "tap to open in history & retry ↗" : "tap to view in history ↗" }));
  bk.append(card);
  document.body.append(bk);
}

// ---------------------------------------------------------------------------
// Init
// ---------------------------------------------------------------------------

export function composeInit() {
  $("#master").addEventListener("input", e => { state.master = e.target.value; refreshCounts(); refreshTargets(); renderMeta(); renderPreview(); renderImages(); autoGrow(e.target); });
  autoGrow($("#master")); // size correctly if a draft was already in the field at init
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
    entry.id = crypto.randomUUID();
    state.images.push(entry);
    renderImages();
  });
  $("#schedat").addEventListener("input", updateSched);
  $("#schedclear").addEventListener("click", () => { $("#schedat").value = ""; updateSched(); });
  $("#submit").addEventListener("click", submit);
  document.getElementById("threadnum")?.addEventListener("change", () => { refreshTargets(); renderPreview(); });
  renderChips(); renderImages(); renderCards(); renderMeta(); renderPreview();
  const pending = getInflight();
  if (pending) openProgressModal(pending);
}
