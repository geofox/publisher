"use strict";
import { el, $, gcount, wcount, flash, confirmModal, META, ORDER } from "./common.js";
import { state, effectiveText, postedText, buildSpec, buildInteractSpec, defaultOv } from "./state.js";
import { renderPreview } from "./preview.js";
import { resultRow, openDetail } from "./history.js";

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
// Platform chips
// ---------------------------------------------------------------------------

function renderChips() {
  const c = $("#chips"); c.innerHTML = "";
  const it = state.interaction;
  for (const p of ORDER) {
    const on = state.platforms.has(p);
    const locked = it != null && p === it.platform;
    let label = META[p].label;
    if (it) label += p === it.platform ? " · native" : " · copy";
    c.append(el("button", {
      class: "chip p-" + p + (on ? " on" : "") + (locked ? " locked" : ""), type: "button", text: label,
      onclick: () => {
        if (locked) return; // source platform stays selected in interaction mode
        on ? state.platforms.delete(p) : state.platforms.add(p);
        if (!state.platforms.has(state.focus)) state.focus = p;
        renderChips(); renderCards(); renderPreview();
      },
    }));
  }
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
    state.images.forEach((i) => URL.revokeObjectURL(i.url));
    state.images = [];
    ORDER.forEach((p) => { state.ov[p] = defaultOv(p); }); // drop stale per-platform overrides
    state.platforms = new Set([src.platform]);
    state.focus = src.platform;
    const tab = document.querySelector('.tab[data-view="compose"]');
    if (tab) tab.click();
    const m = $("#master"); if (m) m.value = "";
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
  state.images.forEach((i) => URL.revokeObjectURL(i.url));
  state.images = [];
  const m = $("#master"); if (m) m.value = "";
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
      state.images.forEach((i) => URL.revokeObjectURL(i.url));
      state.images = [];
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
      const m = $("#master"); if (m) m.value = state.master;
      window.scrollTo({ top: 0, behavior: "smooth" });
      renderImages();
      renderInteractionUI(); // re-renders chips/cards/preview and hides the banner
      flash("Translated draft loaded" + (input.lang ? " · lang " + input.lang : ""));
      return;
    }

    // full spec shape
    state.interaction = input.interaction || null;
    state.master = input.master_text || "";
    state.images.forEach((i) => URL.revokeObjectURL(i.url));
    state.platforms = new Set(input.platforms && input.platforms.length ? input.platforms : ORDER);
    if (!state.platforms.has(state.focus)) state.focus = [...state.platforms][0] || "bluesky";
    // reset overrides to defaults, then apply saved overrides
    ORDER.forEach(p => { state.ov[p] = defaultOv(p); });
    if (input.overrides) {
      for (const p of Object.keys(input.overrides)) {
        if (!state.ov[p]) continue;
        Object.assign(state.ov[p], input.overrides[p]);
      }
    }
    // images come from input.media (hydrated draft) — already-uploaded references
    state.images = (input.media || []).map(m => ({
      blossom_url: m.blossom_url, sha256: m.sha256, mime: m.mime,
      dim: m.dim, blurhash: m.blurhash, size_bytes: m.size_bytes, alt: m.alt || "",
      ordinal: m.ordinal, file: null,
      // derive a displayable URL from blossom_url for the thumbnail strip
      url: m.blossom_url || "",
    }));
    state.activeDraftId = input.id || null;
    state.dirty = false;
    const tab = document.querySelector('.tab[data-view="compose"]');
    if (tab) tab.click();
    const master = $("#master"); if (master) master.value = state.master;
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
    const n = gcount(postedText(p)); // full posted text (reproduction on fan-out) — matches the preview
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
  const text = effectiveText(p), n = gcount(postedText(p));
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
    onclick: () => { ov.text = null; renderCards(); renderPreview(); } });
  const ta = el("textarea", {
    oninput: e => {
      ov.text = e.target.value;
      const m = gcount(postedText(p));
      cnt.textContent = meta.limit ? `${m}/${meta.limit}` : "∞";
      cnt.className = counterClass(m, meta.limit);
      card.classList.add("edited");
      resetBtn.hidden = false;
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

function renderImages() {
  const c = $("#images"); c.innerHTML = "";
  state.images.forEach((img, i) => {
    c.append(el("div", { class: "thumb" },
      el("img", { src: img.url, alt: "" }),
      el("input", { type: "text", placeholder: "alt text", value: img.alt,
        oninput: e => { img.alt = e.target.value; renderPreview(); } }),
      el("button", { class: "rm", type: "button", text: "remove",
        onclick: () => { URL.revokeObjectURL(img.url); state.images.splice(i, 1); renderImages(); } }),
    ));
  });
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
    const n = gcount(postedText(p));
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

async function doPost() {
  const btn = $("#submit");
  const isScheduled = !!$("#schedat").value;
  btn.disabled = true; btn.textContent = isScheduled ? "Scheduling…" : "Posting…";
  const fd = new FormData();
  fd.append("spec", JSON.stringify(buildSpec()));
  for (const img of state.images) fd.append("image", img.file);
  try {
    const r = await fetch("/api/post", { method: "POST", body: fd, credentials: "same-origin" });
    const data = await r.json();
    if (!r.ok) throw new Error(data.error || ("HTTP " + r.status));
    if (data.status === "scheduled" && data.scheduled_at) {
      flash("Scheduled for " + new Date(data.scheduled_at).toLocaleString("fr-FR", { timeZone: "Europe/Paris" }));
      $("#schedat").value = ""; updateSched();
    } else {
      showResultModal(data);
    }
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
  for (const img of state.images) fd.append("image", img.file);
  try {
    const r = await fetch("/api/interact", { method: "POST", body: fd, credentials: "same-origin" });
    const data = await r.json();
    if (!r.ok) throw new Error(data.error || ("HTTP " + r.status));
    showResultModal({ post_id: data.id, status: data.status, targets: data.targets });
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
  $("#master").addEventListener("input", e => { state.master = e.target.value; refreshCounts(); renderMeta(); renderPreview(); });
  $("#addimg").addEventListener("click", () => $("#imgfile").click());
  $("#imgfile").addEventListener("change", e => {
    const file = e.target.files[0]; if (!file) return;
    if (state.images.length >= 4) { flash("Max 4 images"); return; }
    state.images.push({ file, url: URL.createObjectURL(file), alt: "" });
    e.target.value = ""; renderImages();
  });
  $("#schedat").addEventListener("input", updateSched);
  $("#schedclear").addEventListener("click", () => { $("#schedat").value = ""; updateSched(); });
  $("#submit").addEventListener("click", submit);
  document.getElementById("threadnum")?.addEventListener("change", renderPreview);
  renderChips(); renderImages(); renderCards(); renderMeta(); renderPreview();
}
