"use strict";
import { el, $, api, flash, relTime, shortID, shortRef, META, ORDER, confirmModal } from "./common.js";
import { state } from "./state.js";
import { loadDraft } from "./compose.js";

// ---------------------------------------------------------------------------
// Shared result-row primitives (also imported by compose.js for the result modal)
// ---------------------------------------------------------------------------

export function tgPending(s) { return s === "scheduled" || s === "sending" || s === "missed"; }
export function tgBadge(s) {
  if (s === "success") return { cls: "s", mark: "✓" };
  if (s === "partial") return { cls: "p", mark: "⚠" };
  if (s === "missed")  return { cls: "q", mark: "⏰" };
  if (s === "scheduled" || s === "sending") return { cls: "q", mark: "⏳" };
  return { cls: "f", mark: "✗" };
}

// segmentChain renders a threaded post's per-segment delivery list. When the
// target is "partial", a Resume button re-posts only the missing segments.
function segmentChain(post, t) {
  if (!t.segments || !t.segments.length) return null;
  const wrap = el("div", { class: "seg-chain" });
  for (const s of t.segments) {
    const row = el("div", { class: "seg-row" + (s.status === "failed" ? " seg-failed" : "") });
    row.append(el("span", { class: "seg-n", text: `${s.ordinal + 1}.` }));
    const body = el("span", { class: "seg-text", text: s.text });
    if (s.remote_url) {
      const a = el("a", { href: s.remote_url, target: "_blank", rel: "noopener", text: "↗" });
      row.append(body, a);
    } else {
      row.append(body);
    }
    if (s.error) row.append(el("span", { class: "seg-err", text: s.error }));
    wrap.append(row);
  }
  if (t.status === "partial") {
    const plat = t.platform;
    const btn = el("button", { class: "ghost sm", type: "button", text: "Resume" });
    btn.addEventListener("click", async () => {
      btn.disabled = true;
      try {
        const rr = await fetch(`/api/posts/${encodeURIComponent(post.id)}/retry`, {
          method: "POST", credentials: "same-origin",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ platforms: [plat] }),
        });
        const np = await rr.json(); if (!rr.ok) throw new Error(np.error || ("HTTP " + rr.status));
        const st = targetStatus(np, plat);
        renderDetail(np);
        flash(`${plat}: resume ${st === "success" ? "succeeded ✓" : "still failing ✗"}`);
      } catch (e) {
        btn.disabled = false;
        flash("Resume error: " + e.message);
      }
    });
    wrap.append(btn);
  }
  return wrap;
}

// relayBlock renders a nostr target's per-relay delivery. retryFn(relayUrl, btn)
// is optional; when provided, failed relays get a [↻] button (history detail only).
export function relayBlock(t, retryFn) {
  if (t.platform !== "nostr" || !t.relays || !t.relays.length) return null;
  const attempted = t.relays.filter(r => r.status !== "skipped").length;
  const ok = t.relays.filter(r => r.status === "ok").length;
  const wrap = el("div", { class: "relays" });
  wrap.append(el("div", { class: "relays-head", text: `${ok}/${attempted} relays` }));
  for (const r of t.relays) {
    const mark = r.status === "ok" ? "✓" : r.status === "failed" ? "✗" : "·";
    const cls = r.status === "ok" ? "rok" : r.status === "failed" ? "rfail" : "rskip";
    const row = el("div", { class: "relay " + cls },
      el("span", { class: "rmark", text: mark }),
      el("span", { class: "rurl", text: r.url.replace(/^wss?:\/\//, "") }),
      r.status === "skipped" ? el("span", { class: "rnote", text: "skipped · no Tor" }) : null,
      r.status === "failed" && r.message ? el("span", { class: "rnote", text: r.message }) : null,
    );
    if (retryFn && r.status === "failed") {
      row.append(el("button", { class: "retry sm", type: "button", text: "↻",
        onclick: ev => { ev.stopPropagation(); retryFn(r.url, ev.target); } }));
    }
    wrap.append(row);
  }
  return wrap;
}

// resultRow renders one platform outcome line shared by the modal and detail.
// stop=true keeps the success link from bubbling (used inside the clickable modal).
export function resultRow(t, stop) {
  let mid;
  if (t.status === "success" || t.status === "partial") {
    if (t.remote_url) mid = el("a", { class: "lnk grow", href: t.remote_url, target: "_blank", rel: "noopener",
      text: "view ↗", onclick: stop ? (e => e.stopPropagation()) : null });
    else mid = el("span", { class: "mono-ref grow", text: t.remote_id ? "id " + shortRef(t.remote_id) : "ok" });
  } else if (t.status === "scheduled" || t.status === "sending") {
    mid = el("span", { class: "muted grow", text: "⏳ pending" });
  } else if (t.status === "missed") {
    mid = el("span", { class: "muted grow", text: "⏰ not sent (missed)" });
  } else {
    mid = el("span", { class: "bad grow", text: t.error || "failed" });
  }
  const b = tgBadge(t.status);
  const row = el("div", { class: "res" },
    el("span", { class: "bdg " + b.cls, text: `${t.platform} ${b.mark}` }),
    mid,
    el("span", { class: "meta", text: t.latency_ms ? t.latency_ms + "ms" : "" }),
  );
  const relays = relayBlock(t, null); // read-only in modal
  return relays ? el("div", { class: "res-wrap" }, row, relays) : row;
}

// interactionBadge renders "↩ replied to / ❝ quoted / 🔁 reposted <author>" with a
// link to the source, for posts created via the Interact tab. Returns null for
// normal posts.
function interactionBadge(post) {
  const i = post.interaction;
  if (!i) return null;
  const verb = i.action === "reply" ? "↩ replied to" : i.action === "quote" ? "❝ quoted" : "🔁 reposted";
  const who = i.source_author || i.source_platform || "source";
  const link = i.source_url
    ? el("a", { href: i.source_url, target: "_blank", rel: "noopener", text: who })
    : el("span", { text: who });
  return el("div", { class: "hist-interaction" }, el("span", { text: verb + " " }), link);
}

// ---------------------------------------------------------------------------
// List state
// ---------------------------------------------------------------------------

let hfilter = "all", hquery = "", hoffset = 0, hlimit = 50, hdone = false, hposts = [];
let hloading = false;

export async function loadHistory(reset = true) {
  if (hloading) return;
  hloading = true;
  if (reset) { hoffset = 0; hdone = false; hposts = []; }
  try {
    const qs = `?status=${encodeURIComponent(hfilter)}&q=${encodeURIComponent(hquery)}&limit=${hlimit}&offset=${hoffset}`;
    const page = await api("/api/posts" + qs).catch(e => { flash("History: " + e.message); return []; });
    if (page.length < hlimit) hdone = true;
    hposts = reset ? page : hposts.concat(page);
    hoffset += page.length;
    renderList();
  } finally {
    hloading = false;
  }
}

function renderList() {
  const list = $("#hlist"); list.innerHTML = "";
  if (!hposts.length) { list.append(el("p", { class: "muted", text: "No posts." })); return; }
  // Scheduled/posted split: pending first, each group under a subheader (only in 'all').
  // Use isPendingPost (scheduled|sending only) so 'missed' falls into the posted group.
  const pending = hposts.filter(p => isPendingPost(p));
  const posted  = hposts.filter(p => !isPendingPost(p));
  const group = (label, arr) => {
    if (!arr.length) return;
    list.append(el("div", { class: "hgroup", text: label }));
    for (const p of arr) list.append(historyItem(p));
  };
  if (hfilter === "all") { group("⏳ scheduled", pending); group("posted", posted); }
  else { for (const p of hposts) list.append(historyItem(p)); }
  if (!hdone) list.append(el("button", { class: "ghost sm hmore", type: "button", text: "load more",
    onclick: () => loadHistory(false) }));
}

// ---------------------------------------------------------------------------
// List item
// ---------------------------------------------------------------------------

function isPendingPost(p) { return p.status === "scheduled" || p.status === "sending"; }

// confirmHide soft-deletes a terminal post (kept in the archive, removed from the list).
function confirmHide(p) {
  confirmModal({
    title: "Hide this post?",
    body: "It stays in the archive — it just leaves this list.",
    confirmText: "Hide",
    onConfirm: async () => {
      try {
        const r = await fetch(`/api/posts/${encodeURIComponent(p.id)}/hide`, { method: "POST", credentials: "same-origin" });
        if (!r.ok) throw new Error("HTTP " + r.status);
        // Optimistic: drop just this row (no full refetch → instant, no stale cache).
        const node = $(`.hitem[data-id="${p.id}"]`);
        if (node) node.remove();
        const det = $("#hdetail");
        if (det && det.dataset.id === p.id) det.innerHTML = "";
        flash("Post hidden");
        return true;
      } catch (e) { flash("Hide failed: " + e.message); return false; }
    },
  });
}

function schedLine(post) {
  if (post.status === "scheduled" && post.scheduled_at) {
    const when = new Date(post.scheduled_at).toLocaleString("fr-FR", { timeZone: "Europe/Paris" });
    return el("span", { class: "sched-when", text: `⏳ scheduled · fires ${when}` });
  }
  if (post.status === "missed") return el("span", { class: "sched-missed", text: "⏰ missed" });
  return null;
}

function historyItem(p) {
  // Per-platform pills, color-coded by outcome: green ✓ success, red ✗ failed,
  // amber for targeted-but-other, grey for platforms this post didn't target.
  const byPlat = {}; let ok = 0, fail = 0, pend = 0;
  for (const t of (p.targets || [])) {
    byPlat[t.platform] = t.status;
    if (t.status === "success") ok++;
    else if (tgPending(t.status)) pend++;
    else fail++;
  }
  const sel = new Set(p.platforms || []);
  const badges = el("div", { class: "badges" });
  for (const plat of ORDER) {
    let cls = "pill off", mark = "";
    if (sel.has(plat)) {
      const st = byPlat[plat];
      if (st === "success") { cls = "pill s"; mark = " ✓"; }
      else if (st === "failed") { cls = "pill f"; mark = " ✗"; }
      else if (tgPending(st)) { cls = "pill q"; mark = st === "missed" ? " ⏰" : " ⏳"; }
      else { cls = "pill p"; }
    }
    badges.append(el("span", { class: cls, text: META[plat].label + mark }));
  }
  // Show the actual fire time once published (fired_at = latest attempt); fall
  // back to created_at for pending/never-fired posts.
  const ts = p.fired_at || p.created_at;
  const when = el("div", { class: "when" },
    el("span", { title: new Date(ts).toLocaleString(), text: relTime(ts) }),
    el("span", { class: "dot", text: "·" }), el("span", { text: p.source }),
    el("span", { class: "dot", text: "·" }), el("span", { text: "#" + shortID(p.id) }),
  );
  if (ok) when.append(el("span", { class: "dot", text: "·" }), el("span", { class: "ok tot", text: "✓" + ok }));
  if (fail) when.append(el("span", { class: "dot", text: "·" }), el("span", { class: "bad tot", text: "✗" + fail }));
  if (pend) when.append(el("span", { class: "dot", text: "·" }), el("span", { class: "muted tot", text: "⏳" + pend }));
  const sl = schedLine(p);
  const listIb = interactionBadge(p);
  const item = el("div", { class: "hitem", "data-id": p.id, onclick: () => openDetail(p.id) },
    when,
    listIb,
    el("div", { class: "txt", text: (p.master_text || "").slice(0, 160) }),
    sl,
    badges);
  // Past (non-pending) posts can be hidden from the list; pending posts use cancel.
  if (!isPendingPost(p)) {
    item.append(el("button", { class: "hide-x", type: "button", title: "Hide from list", text: "✕",
      onclick: e => { e.stopPropagation(); confirmHide(p); } }));
  }
  return item;
}

// ---------------------------------------------------------------------------
// Detail pane
// ---------------------------------------------------------------------------

function targetStatus(post, plat) {
  for (const t of (post.targets || [])) if (t.platform === plat) return t.status;
  return "?";
}

function schedControls(post, refresh) {
  if (post.status !== "scheduled") return null;
  const cancel = el("button", { class: "ghost sm", type: "button", text: "cancel",
    onclick: async () => {
      const r = await fetch(`/api/posts/${encodeURIComponent(post.id)}/cancel`, { method: "POST", credentials: "same-origin" });
      if (!r.ok) { flash("cancel failed"); return; }
      flash("cancelled"); refresh();
    } });
  const change = el("button", { class: "ghost sm", type: "button", text: "change time",
    onclick: async () => {
      const v = prompt("New time (YYYY-MM-DDTHH:MM, Paris):");
      if (!v) return;
      const d = new Date(v);
      if (isNaN(d.getTime())) { flash("Invalid date — use YYYY-MM-DDTHH:MM"); return; }
      const r = await fetch(`/api/posts/${encodeURIComponent(post.id)}/reschedule`, {
        method: "POST", credentials: "same-origin",
        headers: { "Content-Type": "application/json" }, body: JSON.stringify({ scheduled_at: d.toISOString() }) });
      if (!r.ok) { flash("reschedule failed"); return; }
      flash("rescheduled"); refresh();
    } });
  return el("div", { class: "sched-ctrls" }, cancel, change);
}

async function relayRetry(postID, relayURL, btn) {
  btn.disabled = true; btn.textContent = "…";
  try {
    const r = await fetch(`/api/posts/${encodeURIComponent(postID)}/relay-retry`, {
      method: "POST", credentials: "same-origin",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ platform: "nostr", relay: relayURL }),
    });
    const post = await r.json();
    if (!r.ok) throw new Error(post.error || ("HTTP " + r.status));
    renderDetail(post);
    document.querySelectorAll(".hitem").forEach(e => e.classList.toggle("active", e.dataset.id === postID));
    const host = relayURL.replace(/^wss?:\/\//, "");
    const nt = (post.targets || []).find(t => t.platform === "nostr");
    const rs = nt && (nt.relays || []).find(r => r.url === relayURL);
    flash(`${host}: ${rs ? rs.status : targetStatus(post, "nostr")}`);
  } catch (e) { btn.disabled = false; btn.textContent = "↻"; flash("Relay retry error: " + e.message); }
}

// translateSelect builds the small inline dropdown next to the "text" header in
// the detail view. Picking a target fires POST /api/translate and hands the
// result off to compose.loadDraft (which confirms before replacing any in-
// progress draft). Reset to the placeholder on completion so the same row can
// trigger another translation.
function translateSelect(sourceText) {
  const sel = el("select", { class: "translate-sel", "aria-label": "Translate post" });
  sel.append(el("option", { value: "", text: "translate to…" }));
  for (const code of state.translateTargets) {
    sel.append(el("option", { value: code, text: "→ " + code }));
  }
  sel.addEventListener("change", async (e) => {
    const target = e.target.value;
    if (!target) return;
    sel.disabled = true;
    try {
      const data = await api("/api/translate", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ text: sourceText, target_lang: target }),
      });
      loadDraft({ text: data.text, lang: target });
    } catch (err) {
      flash("Translation failed: " + err.message);
    } finally {
      sel.value = "";
      sel.disabled = false;
    }
  });
  return sel;
}

function renderDetail(post) {
  const d = $("#hdetail"); d.innerHTML = ""; d.dataset.id = post.id;
  const tg = post.targets || [];
  let ok = 0, fail = 0, pend = 0; for (const t of tg) { if (t.status === "success") ok++; else if (tgPending(t.status)) pend++; else fail++; }
  const scls = post.status === "success" ? "ok" : post.status === "failed" ? "bad" : "warn";

  // ← back button: dismisses detail slide-over on mobile (desktop ignores via CSS)
  const backBtn = el("button", { class: "ghost sm detail-back", type: "button", text: "← back",
    onclick: () => { const h = $("#history"); if (h) h.classList.remove("detail-open"); } });

  d.append(el("div", { class: "sec-head" },
    backBtn,
    el("span", { class: "lbl " + scls, text: "result · " + post.status }),
    el("span", { class: "metaline", text: `✓${ok} ✗${fail}${pend ? " ⏳" + pend : ""} · ${tg.length} target${tg.length === 1 ? "" : "s"} · #${shortID(post.id)}` }),
  ));
  const sl = schedLine(post);
  if (sl) d.append(sl);
  const sc = schedControls(post, () => loadHistory(true));
  if (sc) d.append(sc);
  const ib = interactionBadge(post);
  if (ib) d.append(ib);

  if (post.master_text) {
    const head = el("div", { class: "sec-head" }, el("span", { class: "lbl", text: "text" }));
    if (state.translateTargets && state.translateTargets.length) {
      head.append(translateSelect(post.master_text));
    }
    d.append(head);
    d.append(el("div", { class: "dtext", text: post.master_text }));
  }

  if (post.media && post.media.length) {
    const m = el("div", { class: "dmedia" });
    for (const im of post.media) {
      m.append(el("figure", {},
        el("img", { src: im.blossom_url || "", alt: im.alt || "", loading: "lazy" }),
        im.alt ? el("figcaption", { text: im.alt }) : null));
    }
    d.append(m);
  }

  for (const t of tg) {
    const last = (t.attempts && t.attempts.length) ? t.attempts[t.attempts.length - 1] : null;
    const tries = t.attempt_count || (t.attempts ? t.attempts.length : 1);
    const lat = t.latency_ms || (last && last.latency_ms) || 0;
    let mid;
    if (t.status === "success") {
      if (t.remote_url) mid = el("a", { class: "lnk grow", href: t.remote_url, target: "_blank", rel: "noopener",
        text: (t.remote_id ? shortRef(t.remote_id) : "view") + " ↗" });
      else mid = el("span", { class: "mono-ref grow", text: t.remote_id ? "id " + shortRef(t.remote_id) : "ok" });
    } else if (t.status === "scheduled" || t.status === "sending") {
      mid = el("span", { class: "muted grow", text: "⏳ pending" });
    } else if (t.status === "missed") {
      mid = el("span", { class: "muted grow", text: "⏰ not sent (missed)" });
    } else {
      mid = el("span", { class: "bad grow", text: (last && last.error) || "failed" });
    }
    const meta = el("span", { class: "meta",
      text: (tries ? `${tries}×` : "") + (lat ? ` · ${lat}ms` : "") + (last && last.attempted_at ? ` · ${new Date(last.attempted_at).toLocaleTimeString()}` : "") });
    const b = tgBadge(t.status);
    const row = el("div", { class: "res" },
      el("span", { class: "bdg " + b.cls, text: `${t.platform} ${b.mark}` }),
      mid, meta);
    if (t.status === "failed" || t.status === "missed") {
      const plat = t.platform;
      row.append(el("button", {
        class: "retry", type: "button", text: "Retry",
        onclick: async ev => {
          ev.stopPropagation();
          const btn = ev.target; btn.disabled = true; btn.textContent = "Retrying…";
          try {
            const rr = await fetch(`/api/posts/${encodeURIComponent(post.id)}/retry`, {
              method: "POST", credentials: "same-origin",
              headers: { "Content-Type": "application/json" }, body: JSON.stringify({ platforms: [plat] }),
            });
            const np = await rr.json(); if (!rr.ok) throw new Error(np.error || ("HTTP " + rr.status));
            const st = targetStatus(np, plat);
            renderDetail(np);
            flash(`${plat}: retry ${st === "success" ? "succeeded ✓" : "still failing ✗"}`);
          } catch (e) { btn.disabled = false; btn.textContent = "Retry"; flash("Retry error: " + e.message); }
        },
      }));
    }
    d.append(row);
    const chain = segmentChain(post, t);
    if (chain) d.append(chain);
    if (t.final_text && t.final_text !== post.master_text) {
      d.append(el("div", { class: "dtext alt", text: t.final_text }));
    }
    const relays = relayBlock(t, (relayURL, btn) => relayRetry(post.id, relayURL, btn));
    if (relays) d.append(relays);
    if (last && (last.request_json || last.response_json)) {
      const v = el("div", { class: "verbose", hidden: true, text: `→ ${last.request_json || ""}\n← ${last.response_json || ""}` });
      const vb = el("button", { class: "reset", type: "button", text: "verbose ▸",
        onclick: () => { v.hidden = !v.hidden; vb.textContent = v.hidden ? "verbose ▸" : "verbose ▾"; } });
      d.append(vb, v);
    }
  }
}

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

export async function openDetail(id) {
  $("#history").classList.add("detail-open"); // mobile slide-over
  const d = $("#hdetail"); d.innerHTML = "Loading…";
  try {
    const post = await api("/api/posts/" + encodeURIComponent(id));
    renderDetail(post);
    document.querySelectorAll(".hitem").forEach(e => e.classList.toggle("active", e.dataset.id === id));
  } catch (e) {
    d.innerHTML = "";
    d.append(el("div", { class: "err", text: "Error: " + e.message }));
    $("#history").classList.remove("detail-open");
    flash("Error: " + e.message);
  }
}

export function historyInit() {
  // segment buttons (rendered in index.html as #hseg button[data-status])
  document.querySelectorAll("#hseg button").forEach(b =>
    b.addEventListener("click", () => {
      hfilter = b.dataset.status;
      document.querySelectorAll("#hseg button").forEach(x => x.classList.toggle("on", x === b));
      loadHistory(true);
    }));
  const search = $("#hsearch");
  let t;
  search.addEventListener("input", () => { clearTimeout(t); t = setTimeout(() => { hquery = search.value.trim(); loadHistory(true); }, 250); });
  $("#refresh").addEventListener("click", () => loadHistory(true));
}
