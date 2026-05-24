"use strict";
import { el, $, gcount, wcount, flash, confirmModal, META, ORDER } from "./common.js";
import { state, effectiveText, buildSpec } from "./state.js";
import { renderPreview } from "./preview.js";
import { resultRow, openDetail } from "./history.js";

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
  for (const p of ORDER) {
    const on = state.platforms.has(p);
    c.append(el("button", {
      class: "chip p-" + p + (on ? " on" : ""), type: "button", text: META[p].label,
      onclick: () => {
        on ? state.platforms.delete(p) : state.platforms.add(p);
        if (!state.platforms.has(state.focus)) state.focus = p;
        renderChips(); renderCards(); renderPreview();
      },
    }));
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

function overrideCard(p) {
  const ov = state.ov[p], meta = META[p];
  const edited = ov.text != null;
  const text = effectiveText(p), n = gcount(text);
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
      const m = gcount(e.target.value);
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

function fieldsFor(p, ov) {
  const f = el("div", { class: "fields" });
  if (p === "bluesky") {
    f.append(lbl("langs", el("input", { type: "text", value: ov.langs, oninput: e => { ov.langs = e.target.value; } })));
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
    f.append(lbl("lang", el("input", { type: "text", value: ov.language, oninput: e => { ov.language = e.target.value; } })));
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
    const n = gcount(effectiveText(p));
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

// submit guards against posting over a platform's limit: if any selected platform
// is over, confirm first (those targets may be cut/rejected — the rest still post).
// Otherwise post immediately.
function submit() {
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

function showResultModal(data) {
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
  $("#master").addEventListener("input", e => { state.master = e.target.value; renderCards(); renderPreview(); });
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
  renderChips(); renderImages(); renderCards(); renderMeta(); renderPreview();
}
