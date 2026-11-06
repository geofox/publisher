"use strict";
import { el, $, v1, flash, relTime, shortID, shortRef, META, ORDER, confirmModal } from "./common.js";
import { state } from "./state.js";
import { loadDraft } from "./compose.js";
import { brandTile } from "./brands.js";
import { cpIndex } from "./textpos.js";
import { followPost as followPostEvents } from "./follow.js";

// The History tab, on /v1/posts. A post has one target per account; a
// target has its items (one, or one per thread part), and a Nostr item its
// relay deliveries.

// ---------------------------------------------------------------------------
// Words
// ---------------------------------------------------------------------------

// pendingStatus: still to go out (missed counts: it never went).
function pendingStatus(s) { return s === "scheduled" || s === "publishing" || s === "missed"; }
function isPendingPost(p) { return p.status === "scheduled" || p.status === "publishing"; }

function badge(s) {
  if (s === "published") return { cls: "s", mark: "✓" };
  if (s === "partial" || s === "needs_attention") return { cls: "p", mark: "⚠" };
  if (s === "missed") return { cls: "q", mark: "⏰" };
  if (s === "scheduled" || s === "publishing" || s === "planned" || s === "sending") return { cls: "q", mark: "⏳" };
  if (s === "canceled") return { cls: "p", mark: "⊘" };
  if (s === "unknown") return { cls: "p", mark: "?" };
  return { cls: "f", mark: "✗" };
}

// classWords says what an error class means for the owner.
const classWords = {
  transient: "a temporary problem; it is retried",
  rate_limited: "rate limited; it is retried later",
  auth: "the account's sign-in was refused; check its credentials",
  invalid: "the network refused this content; it won't be retried",
  rejected: "the network rejected it; it won't be retried",
  unknown_outcome: "it may have been posted: the server stopped while sending",
};
function classLine(c) { return c && classWords[c] ? classWords[c] : ""; }

const typeName = t => (META[t] && META[t].label) || t;

// ---------------------------------------------------------------------------
// Live updates after a retry or a resolve
// ---------------------------------------------------------------------------

// followPost keeps the detail pane (and the post's list row) current while
// the post sends (see follow.js). until(post) says when the wait is over; by
// default when nothing is publishing.
let follow = null, followID = "";
function stopFollow() {
  const stop = follow;
  follow = null;
  if (stop) stop();
}

function followPost(id, until) {
  stopFollow();
  const stop = followPostEvents(id, {
    until,
    onPost: p => {
      if (follow !== stop) return;
      if ($("#hdetail").dataset.id === id) renderDetail(p);
      patchRow(p);
    },
    // Ended by itself (settled, or the wait ran out), not replaced.
    onStop: () => { if (follow === stop) { follow = null; refreshAttentionBadge(); } },
  });
  follow = stop;
  followID = id;
}

// patchRow redraws a post's list row, if it is shown.
function patchRow(p) {
  const i = hposts.findIndex(x => x.id === p.id);
  if (i >= 0) hposts[i] = p;
  const node = $(`.hitem[data-id="${p.id}"]`);
  if (node) node.replaceWith(historyItem(p, node.classList.contains("active")));
}

// act runs a post action from a button and shows the post it answers with.
// opts.reload re-reads the list (the post may have left the filter);
// opts.until follows the post until it holds (else while it publishes).
async function act(btn, busyText, run, done, opts = {}) {
  const prev = btn.textContent;
  btn.disabled = true; btn.textContent = busyText;
  try {
    const post = await run();
    renderDetail(post);
    if (opts.reload) loadHistory(true);
    else { refreshAttentionBadge(); patchRow(post); }
    if (done) flash(done);
    if (opts.until || post.status === "publishing") followPost(post.id, opts.until);
  } catch (e) {
    btn.disabled = false; btn.textContent = prev;
    flash(e.message);
  }
}

const postPath = id => "/v1/posts/" + encodeURIComponent(id);

// ---------------------------------------------------------------------------
// List
// ---------------------------------------------------------------------------

// Each filter button's /v1 statuses. "failed" includes the posts that need
// the owner; "attention" is only those.
const FILTERS = {
  all: "",
  scheduled: "scheduled,publishing",
  failed: "failed,partial,missed,needs_attention",
  attention: "needs_attention",
  sent: "published",
};

let hfilter = "all", hquery = "", hcursor = null, hdone = false, hposts = [];
let hloading = false, hgen = 0;
const hlimit = 50;

async function refreshAttentionBadge() {
  const b = document.querySelector("#hseg .attn-badge");
  if (!b) return;
  try {
    const page = await v1("/v1/posts?status=needs_attention&limit=100");
    const n = page.data.length;
    if (n > 0) { b.textContent = page.next_cursor ? "100+" : String(n); b.hidden = false; }
    else b.hidden = true;
  } catch { b.hidden = true; }
}

export async function loadHistory(reset = true) {
  if (hloading && !reset) return;
  const gen = ++hgen; // a newer load (filter or search changed) wins
  hloading = true;
  try {
    const qs = new URLSearchParams({ limit: String(hlimit) });
    if (FILTERS[hfilter]) qs.set("status", FILTERS[hfilter]);
    if (hquery) qs.set("q", hquery);
    if (!reset && hcursor) qs.set("cursor", hcursor);
    let page;
    try { page = await v1("/v1/posts?" + qs); }
    catch (e) { if (gen === hgen) flash("History: " + e.message); return; }
    if (gen !== hgen) return;
    hposts = reset ? page.data : hposts.concat(page.data);
    hcursor = page.next_cursor || null;
    hdone = !hcursor;
    renderList();
    refreshAttentionBadge();
  } finally {
    if (gen === hgen) hloading = false;
  }
}

function renderList() {
  const list = $("#hlist"); list.innerHTML = "";
  if (!hposts.length) { list.append(el("p", { class: "muted", text: "No posts." })); return; }
  const active = $("#hdetail").dataset.id;
  const group = (label, arr) => {
    if (!arr.length) return;
    list.append(el("div", { class: "hgroup", text: label }));
    for (const p of arr) list.append(historyItem(p, p.id === active));
  };
  if (hfilter === "all") {
    const pending = hposts.filter(isPendingPost);
    group(pending.some(p => p.status === "publishing") ? "⏳ scheduled & sending" : "⏳ scheduled", pending);
    group("posted", hposts.filter(p => !isPendingPost(p)));
  } else {
    for (const p of hposts) list.append(historyItem(p, p.id === active));
  }
  if (!hdone) list.append(el("button", { class: "ghost sm hmore", type: "button", text: "load more",
    onclick: () => loadHistory(false) }));
}

// confirmHide hides a settled post: it stays in the archive, off this list.
function confirmHide(p) {
  confirmModal({
    title: "Hide this post?",
    body: "It stays in the archive — it just leaves this list.",
    confirmText: "Hide",
    onConfirm: async () => {
      try {
        await v1(postPath(p.id), { method: "PATCH", body: { hidden: true } });
        const node = $(`.hitem[data-id="${p.id}"]`);
        if (node) node.remove();
        hposts = hposts.filter(x => x.id !== p.id);
        const det = $("#hdetail");
        if (det && det.dataset.id === p.id) { det.innerHTML = ""; delete det.dataset.id; stopFollow(); }
        refreshAttentionBadge();
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

// interactionBadge: "↩ replied to / ❝ quoted / 🔁 reposted <author>", linking
// the original.
function interactionBadge(post) {
  const i = post.interaction;
  if (!i) return null;
  const verb = i.action === "reply" ? "↩ replied to" : i.action === "quote" ? "❝ quoted" : "🔁 reposted";
  const who = i.author || i.platform || "source";
  const link = i.url
    ? el("a", { href: i.url, target: "_blank", rel: "noopener", text: who })
    : el("span", { text: who });
  return el("div", { class: "hist-interaction" }, el("span", { text: verb + " " }), link);
}

function historyItem(p, active) {
  // One pill per type, colored by its targets' outcome; grey for the types
  // this post didn't go to.
  const byType = {};
  let ok = 0, fail = 0, pend = 0;
  for (const t of p.targets) {
    byType[t.type] = t.status;
    if (t.status === "published") ok++;
    else if (pendingStatus(t.status)) pend++;
    else fail++;
  }
  const badges = el("div", { class: "badges" });
  const types = [...ORDER, ...Object.keys(byType).filter(t => !ORDER.includes(t))];
  for (const type of types) {
    let cls = "pill off", mark = "";
    if (type in byType) {
      const st = byType[type];
      if (st === "published") { cls = "pill s"; mark = " ✓"; }
      else if (st === "failed") { cls = "pill f"; mark = " ✗"; }
      else if (pendingStatus(st)) { cls = "pill q"; mark = st === "missed" ? " ⏰" : " ⏳"; }
      else { cls = "pill p"; }
    }
    badges.append(el("span", { class: cls, text: typeName(type) + mark }));
  }
  // A scheduled post that has fired is dated by its time; others by when
  // they were made.
  const ts = (p.status !== "scheduled" && p.scheduled_at) || p.created_at;
  const when = el("div", { class: "when" },
    el("span", { title: new Date(ts).toLocaleString(), text: relTime(ts) }),
    el("span", { class: "dot", text: "·" }), el("span", { text: p.source }),
    el("span", { class: "dot", text: "·" }), el("span", { text: "#" + shortID(p.id.replace(/^pst_/, "")) }),
  );
  if (ok) when.append(el("span", { class: "dot", text: "·" }), el("span", { class: "ok tot", text: "✓" + ok }));
  if (fail) when.append(el("span", { class: "dot", text: "·" }), el("span", { class: "bad tot", text: "✗" + fail }));
  if (pend) when.append(el("span", { class: "dot", text: "·" }), el("span", { class: "muted tot", text: "⏳" + pend }));
  if (p.status === "needs_attention") {
    when.append(el("span", { class: "dot", text: "·" }), el("span", { class: "warn tot", text: "needs you" }));
  }
  const item = el("div", { class: "hitem" + (active ? " active" : ""), "data-id": p.id, onclick: () => openDetail(p.id) },
    when,
    interactionBadge(p),
    el("div", { class: "txt", text: (p.content.text || "").slice(0, 160) }),
    schedLine(p),
    badges);
  if (!isPendingPost(p)) {
    item.append(el("button", { class: "hide-x", type: "button", title: "Hide from list", text: "✕",
      onclick: e => { e.stopPropagation(); confirmHide(p); } }));
  }
  return item;
}

// ---------------------------------------------------------------------------
// Detail
// ---------------------------------------------------------------------------

function schedControls(post) {
  if (post.status === "publishing") {
    const stop = el("button", { class: "ghost sm", type: "button", text: "stop sending",
      onclick: () => act(stop, "…", () => v1(postPath(post.id) + ":cancel", { method: "POST" }), "stopped", { reload: true }) });
    return el("div", { class: "sched-ctrls" }, stop);
  }
  if (post.status !== "scheduled") return null;
  const cancel = el("button", { class: "ghost sm", type: "button", text: "cancel",
    onclick: () => act(cancel, "…", () => v1(postPath(post.id) + ":cancel", { method: "POST" }), "canceled", { reload: true }) });
  const change = el("button", { class: "ghost sm", type: "button", text: "change time",
    onclick: () => {
      const v = prompt("New time (YYYY-MM-DDTHH:MM, Paris):");
      if (!v) return;
      const d = new Date(v);
      if (isNaN(d.getTime())) { flash("Invalid date — use YYYY-MM-DDTHH:MM"); return; }
      act(change, "…", () => v1(postPath(post.id), { method: "PATCH", body: { schedule: { at: d.toISOString() } } }), "rescheduled", { reload: true });
    } });
  return el("div", { class: "sched-ctrls" }, cancel, change);
}

// translateSelect: pick a language, and the post's text, translated, opens
// in Compose (which asks before replacing work in progress).
function translateSelect(sourceText) {
  const sel = el("select", { class: "translate-sel", "aria-label": "Translate post" });
  sel.append(el("option", { value: "", text: "translate to…" }));
  for (const code of state.translateTargets) sel.append(el("option", { value: code, text: "→ " + code }));
  sel.addEventListener("change", async e => {
    const target = e.target.value;
    if (!target) return;
    sel.disabled = true;
    try {
      const data = await v1("/v1/translate", { method: "POST", body: { text: sourceText, target_lang: target } });
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

// retryable: what :retry would send for this target.
function retryable(t) {
  return t.status === "failed" || t.status === "missed" || t.status === "canceled" ||
    (t.status === "partial" && (t.items || []).length > 1);
}

// resolveControls asks the owner about an item whose outcome is unknown.
function resolveControls(post, t, it) {
  const later = (t.items || []).some(x => x.ordinal > it.ordinal && (x.status === "planned" || x.status === "failed"));
  const yes = el("button", { class: "ghost sm", type: "button", text: "It was posted",
    onclick: ev => {
      ev.stopPropagation();
      const ask = later
        ? "Paste the post's URL. Where the network can look it up (Bluesky, Mastodon), the rest of the thread goes on under it; otherwise the rest is canceled."
        : "Paste the post's URL (optional).";
      const url = prompt(ask, "");
      if (url === null) return;
      const body = { target: t.id, ordinal: it.ordinal, outcome: "published" };
      if (url.trim()) body.url = url.trim();
      act(yes, "…", () => v1(postPath(post.id) + ":resolve", { method: "POST", body }), "recorded as posted");
    } });
  const no = el("button", { class: "ghost sm", type: "button", text: "It wasn't: send it again",
    onclick: ev => {
      ev.stopPropagation();
      act(no, "…", () => v1(postPath(post.id) + ":resolve", { method: "POST",
        body: { target: t.id, ordinal: it.ordinal, outcome: "not_published" } }), "sending again");
    } });
  return el("div", { class: "resolve" },
    el("span", { class: "muted", text: `Check ${typeName(t.type)}: did this part go out?` }), yes, no);
}

// itemChain lists a target's items: each part of a thread, and any item
// whose outcome is unknown even when it stands alone.
function itemChain(post, t) {
  const items = t.items || [];
  if (items.length < 2 && !items.some(it => it.status === "unknown")) return null;
  const wrap = el("div", { class: "seg-chain" });
  const ranged = items.length > 1 && fromShared(post.content.text || "", items);
  for (const it of items) {
    const bad = it.status === "failed" || it.status === "unknown";
    const row = el("div", { class: "seg-row" + (bad ? " seg-failed" : "") + (ranged ? " ranged" : "") });
    if (ranged && it.range) {
      // Where the part came from: hovering (or tapping) it marks its
      // stretch of the text above.
      const show = () => markRange(it.range);
      row.title = "Marks where this part is in the text";
      row.addEventListener("mouseenter", show);
      row.addEventListener("mouseleave", () => markRange(null));
      row.addEventListener("click", e => { if (!e.target.closest("a, button")) show(); });
    }
    const b = badge(it.status);
    row.append(el("span", { class: "seg-n", text: `${it.ordinal + 1}.` }),
      el("span", { class: "seg-text", text: it.text }),
      el("span", { class: "bdg " + b.cls, title: it.status, text: b.mark }));
    if (it.url) row.append(el("a", { href: it.url, target: "_blank", rel: "noopener", text: "↗" }));
    if (it.error || it.error_class) {
      row.append(el("span", { class: "seg-err", text: [it.error, classLine(it.error_class)].filter(Boolean).join(" — ") }));
    }
    wrap.append(row);
    if (it.status === "unknown") wrap.append(resolveControls(post, t, it));
  }
  return wrap;
}

// fromShared reports whether a target's items came from the post's text (not
// an account's own wording): each part's range of that text is the part.
function fromShared(text, items) {
  const ix = cpIndex(text);
  return items.every(it => {
    if (!it.range) return false;
    const [a, b] = it.range;
    if (b > ix.length - 1) return false;
    const part = text.slice(ix[a], ix[b]).trim();
    return part === "" || (it.text || "").startsWith(part);
  });
}

// markRange marks code points [from, to) of the detail's text, or nothing.
function markRange(range) {
  const box = $("#hdetail .dtext.main");
  if (!box) return;
  const text = box.dataset.text || "";
  if (!range) { box.textContent = text; return; }
  const ix = cpIndex(text);
  const a = ix[Math.min(range[0], ix.length - 1)], b = ix[Math.min(range[1], ix.length - 1)];
  box.replaceChildren(text.slice(0, a), el("mark", { text: text.slice(a, b) }), text.slice(b));
}

// deliveryBlock: a Nostr item's relays, with a retry for each failed one.
function deliveryBlock(post, t) {
  const it = (t.items || [])[0];
  const ds = (it && it.deliveries) || [];
  if (!ds.length) return null;
  const attempted = ds.filter(d => d.status !== "skipped").length;
  const ok = ds.filter(d => d.status === "ok").length;
  const wrap = el("div", { class: "relays" });
  wrap.append(el("div", { class: "relays-head", text: `${ok}/${attempted} relays` }));
  for (const d of ds) {
    const mark = d.status === "ok" ? "✓" : d.status === "failed" ? "✗" : "·";
    const cls = d.status === "ok" ? "rok" : d.status === "failed" ? "rfail" : "rskip";
    const row = el("div", { class: "relay " + cls },
      el("span", { class: "rmark", text: mark }),
      el("span", { class: "rurl", text: d.endpoint.replace(/^wss?:\/\//, "") }),
      d.status === "skipped" ? el("span", { class: "rnote", text: "skipped · no Tor" }) : null,
      d.status === "failed" && d.message ? el("span", { class: "rnote", text: d.message }) : null,
    );
    if (d.status === "failed") {
      const btn = el("button", { class: "retry sm", type: "button", text: "↻",
        onclick: ev => {
          ev.stopPropagation();
          // The relay is retried in the background: wait for its row to change.
          const was = d.attempted_at;
          const until = p => {
            const tt = p.targets.find(x => x.id === t.id);
            const dd = tt && tt.items && tt.items[0] && tt.items[0].deliveries.find(x => x.endpoint === d.endpoint);
            return !dd || dd.status !== "failed" || dd.attempted_at !== was;
          };
          act(btn, "…", () => v1(postPath(post.id) + ":retry", { method: "POST",
            body: { deliveries: [{ target: t.id, endpoint: d.endpoint }] } }), d.endpoint.replace(/^wss?:\/\//, "") + ": retrying", { until });
        } });
      row.append(btn);
    }
    wrap.append(row);
  }
  return wrap;
}

function targetRow(post, t) {
  const items = t.items || [];
  const attempts = t.attempts || [];
  const last = attempts.length ? attempts[attempts.length - 1] : null;
  const failedItem = items.find(it => it.status === "failed" || it.status === "unknown");
  let mid;
  if (t.status === "published" || t.status === "partial") {
    const url = items[0] && items[0].url;
    const rid = items[0] && items[0].remote_id;
    mid = url
      ? el("a", { class: "lnk grow", href: url, target: "_blank", rel: "noopener", text: (rid ? shortRef(rid) : "view") + " ↗" })
      : el("span", { class: "mono-ref grow", text: rid ? "id " + shortRef(rid) : "ok" });
  } else if (t.status === "scheduled" || t.status === "publishing") {
    mid = el("span", { class: "muted grow", text: "⏳ pending" });
  } else if (t.status === "missed") {
    mid = el("span", { class: "muted grow", text: "⏰ not sent (missed)" });
  } else if (t.status === "canceled") {
    mid = el("span", { class: "muted grow", text: "⊘ canceled" });
  } else {
    const err = (failedItem && failedItem.error) || (last && last.error) || "failed";
    mid = el("span", { class: "bad grow", text: err });
  }
  const meta = el("span", { class: "meta",
    text: (attempts.length ? `${attempts.length}×` : "") + (last && last.latency_ms ? ` · ${last.latency_ms}ms` : "") +
      (last ? ` · ${new Date(last.attempted_at).toLocaleTimeString()}` : "") });
  const b = badge(t.status);
  const row = el("div", { class: "res" },
    brandTile(t.type, { size: 22, r: 6 }),
    el("span", { class: "bdg " + b.cls, text: `${typeName(t.type)} ${b.mark}` }),
    mid, meta);
  if (retryable(t) && !items.some(it => it.status === "unknown")) {
    const btn = el("button", { class: "retry", type: "button", text: t.status === "partial" ? "Resume" : "Retry",
      onclick: ev => {
        ev.stopPropagation();
        act(btn, "Retrying…", () => v1(postPath(post.id) + ":retry", { method: "POST", body: { targets: [t.id] } }),
          typeName(t.type) + ": retrying");
      } });
    row.append(btn);
  }
  const out = [row];
  const why = classLine(t.error_class || (failedItem && failedItem.error_class));
  if (why && t.status !== "published") out.push(el("div", { class: "retry-status" + (t.gave_up_at ? " gaveup" : " pending"), text: why }));
  if (t.gave_up_at && t.status !== "published" && t.status !== "canceled") {
    out.push(el("div", { class: "retry-status gaveup", text: `⚠ automatic retries stopped after ${attempts.length} attempts` }));
  }
  const chain = itemChain(post, t);
  if (chain) out.push(chain);
  if (items.length === 1 && items[0].text && items[0].text !== post.content.text) {
    out.push(el("div", { class: "dtext alt", text: items[0].text }));
  }
  const relays = deliveryBlock(post, t);
  if (relays) out.push(relays);
  return out;
}

function renderDetail(post) {
  const d = $("#hdetail"); d.innerHTML = ""; d.dataset.id = post.id;
  const tg = post.targets;
  let ok = 0, fail = 0, pend = 0;
  for (const t of tg) { if (t.status === "published") ok++; else if (pendingStatus(t.status)) pend++; else fail++; }
  const scls = post.status === "published" ? "ok" : post.status === "failed" ? "bad" : "warn";
  const backBtn = el("button", { class: "ghost sm detail-back", type: "button", text: "← back",
    onclick: () => { const h = $("#history"); if (h) h.classList.remove("detail-open"); } });
  d.append(el("div", { class: "sec-head" },
    backBtn,
    el("span", { class: "lbl " + scls, text: "result · " + post.status.replace("_", " ") }),
    el("span", { class: "metaline", text: `✓${ok} ✗${fail}${pend ? " ⏳" + pend : ""} · ${tg.length} target${tg.length === 1 ? "" : "s"} · #${shortID(post.id.replace(/^pst_/, ""))}` }),
  ));
  const sl = schedLine(post);
  if (sl) d.append(sl);
  const sc = schedControls(post);
  if (sc) d.append(sc);
  const ib = interactionBadge(post);
  if (ib) d.append(ib);

  const text = post.content.text;
  if (text) {
    const head = el("div", { class: "sec-head" }, el("span", { class: "lbl", text: "text" }));
    if (state.translateTargets && state.translateTargets.length) head.append(translateSelect(text));
    d.append(head, el("div", { class: "dtext main", "data-text": text, text }));
  }
  if (post.content.media.length) {
    const m = el("div", { class: "dmedia" });
    for (const im of post.content.media) {
      const video = (im.mime || "").startsWith("video/");
      m.append(el("figure", {},
        el("img", { src: (video ? im.poster_url : im.url) || "", alt: im.alt || "", loading: "lazy" }),
        im.alt ? el("figcaption", { text: im.alt }) : null));
    }
    d.append(m);
  }
  for (const t of tg) d.append(...targetRow(post, t));
}

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

let detailGen = 0;
export async function openDetail(id) {
  if (!id) return;
  if (!id.startsWith("pst_")) id = "pst_" + id; // a store id (an in-flight post from before an upgrade)
  const gen = ++detailGen; // a later click wins
  if (follow && followID !== id) stopFollow();
  $("#history").classList.add("detail-open"); // mobile slide-over
  const d = $("#hdetail"); d.innerHTML = "Loading…";
  try {
    const post = await v1(postPath(id));
    if (gen !== detailGen) return;
    renderDetail(post);
    document.querySelectorAll(".hitem").forEach(e => e.classList.toggle("active", e.dataset.id === id));
    if (post.status === "publishing") followPost(id);
  } catch (e) {
    if (gen !== detailGen) return;
    d.innerHTML = "";
    d.append(el("div", { class: "err", text: "Error: " + e.message }));
    $("#history").classList.remove("detail-open");
    flash("Error: " + e.message);
  }
}

export function historyInit() {
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
