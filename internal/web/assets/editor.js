"use strict";
// editor.js is the thread editor: one tab for the shared text and one per
// selected platform.
//
// The shared tab writes the text as a stack of cards, one per hand-made
// break: Ctrl/Cmd+Enter splits a card at the cursor, Backspace at a card's
// start merges it into the one above, and a line of "---" becomes a break
// as soon as it is followed by a new line.
//
// A platform tab shows that platform's thread as the server plans it (the
// plan, see plan.js): solid lines for hand-made breaks, dashed ones for the
// splits made to fit the limit. Changing a break there gives the platform
// its own breaks (the wording still follows the shared text); "Edit
// wording" gives it its own text, edited as its own stack of cards.
//
// Positions are UTF-16 offsets (see textpos.js) and move with the text as
// it is edited.
import { el, META, ORDER, flash, confirmModal } from "./common.js";
import { state, layoutBreaks, effectiveText } from "./state.js";
import { planFor, lastPlanFor } from "./plan.js";
import { brandTile } from "./brands.js";
import { cpIndex, diff, rebase, cleanBreaks, splitPoint, wordStarts, sentenceStarts, nextMarker } from "./textpos.js";

// The API takes at most 99 breaks (100 posts).
const MAX_BREAKS = 99;

let hooks = { changed() {}, tab() {} };
let root = null;
let sharedPane = null, platformPane = null, tabsRow = null;
// The word picked on a platform tab, for "Break here": {p, at}.
let picked = null;

// editorInit draws the editor into #editor. hooks.changed runs after any
// change to the text or its layout, hooks.tab after the tab changes.
export function editorInit(h) {
  hooks = { ...hooks, ...h };
  root = document.getElementById("editor");
  if (!root) return;
  tabsRow = el("div", { class: "ed-tabs", role: "tablist" });
  sharedPane = el("div", { class: "ed-pane ed-shared" });
  platformPane = el("div", { class: "ed-pane ed-platform", hidden: true });
  root.replaceChildren(tabsRow, sharedPane, platformPane);
  document.addEventListener("plan", onPlan);
  document.addEventListener("keydown", onKey);
  renderEditor();
}

// tabKeys are the editor's tabs in order, for Alt+1…Alt+5.
function tabKeys() { return ["shared", ...selected()]; }

// onKey takes the editor's shortcuts while the composer shows: Alt+1…5 pick
// a tab, Alt+C opens the comparison; on a platform tab Enter breaks at the
// picked word and Escape lets it go. (Ctrl/Cmd+Enter and Backspace work in
// the cards themselves.)
function onKey(e) {
  if (!root || root.offsetParent === null || document.querySelector(".modal-bk, .sheet-bk")) {
    if (e.key === "Escape" && cmpOpen) closeCompare();
    return;
  }
  if (e.altKey && !e.ctrlKey && !e.metaKey) {
    const n = /^Digit([1-9])$/.exec(e.code);
    if (n) {
      const t = tabKeys()[+n[1] - 1];
      if (t) { e.preventDefault(); setTab(t); }
      return;
    }
    if (e.code === "KeyC" && wide()) { e.preventDefault(); openCompare(); return; }
  }
  const inField = e.target.closest && e.target.closest("input, textarea, select, [contenteditable]");
  if (inField || !picked || state.tab !== picked.p) return;
  if (e.key === "Escape") { picked = null; renderPlatform(state.tab); }
  if (e.key === "Enter") platformPane.querySelector(".pp-here")?.click();
}

const wide = () => window.matchMedia("(min-width: 1100px)").matches;

// ---------------------------------------------------------------------------
// Documents: the shared text, or one platform's own text
// ---------------------------------------------------------------------------

// A doc is {p: null} for the shared text, {p} for platform p's own text.
const SHARED = { p: null };

function docText(d) { return d.p ? state.ov[d.p].text || "" : state.master; }
function docBreaks(d) { return d.p ? state.ov[d.p].breaks || [] : state.breaks; }
function docMedia(d) {
  if (!d.p) return state.anchors;
  const ov = state.ov[d.p];
  return ov.mediaAt || (ov.mediaAt = {});
}

// A break at the very end of the text opens an empty card to write the next
// post in; it isn't sent (there is nothing after it yet).
function setDocBreaks(d, breaks) {
  const t = docText(d);
  const clean = cleanBreaks(t, breaks.filter(b => b < t.length), MAX_BREAKS);
  if (t.length && breaks.includes(t.length) && clean.length < MAX_BREAKS) clean.push(t.length);
  if (d.p) state.ov[d.p].breaks = clean; else state.breaks = clean;
}

// tidy drops positions an edit left outside the text or doubled; it doesn't
// move them (cleanBreaks does that, on layout changes and on send).
function tidy(breaks, len) {
  return [...new Set(breaks)].filter(b => b > 0 && b <= len).sort((a, b) => a - b);
}

// applyEdit replaces removed units at `at` in d's text with inserted, and
// moves every position in that text with it: the breaks, the media, and the
// own breaks of platforms that post the shared text.
export function applyEdit(d, at, removed, inserted) {
  const t = docText(d);
  const next = t.slice(0, at) + inserted + t.slice(at + removed);
  const mv = x => rebase(x, at, removed, inserted.length);
  const media = docMedia(d);
  for (const k of Object.keys(media)) media[k] = Math.min(mv(media[k]), next.length);
  if (d.p) {
    const ov = state.ov[d.p];
    ov.text = next;
    if (ov.breaks) ov.breaks = tidy(ov.breaks.map(mv), next.length);
    return;
  }
  state.master = next;
  state.breaks = tidy(state.breaks.map(mv), next.length);
  for (const p of ORDER) {
    const ov = state.ov[p];
    if (ov.text == null && ov.breaks) ov.breaks = tidy(ov.breaks.map(mv), next.length);
  }
}

// setSharedText replaces the whole shared text (a translation, a reset):
// positions that no longer fit are dropped.
export function setSharedText(text) {
  applyEdit(SHARED, 0, state.master.length, text);
  state.breaks = [];
}

// cards are d's text cut at its breaks: [start, end) is what a card shows
// (the whitespace before the next break stays out of it), next is where the
// next card starts.
function cards(d) {
  const t = docText(d);
  const bounds = [0, ...docBreaks(d).filter(b => b > 0 && b <= t.length), t.length];
  const out = [];
  for (let i = 0; i + 1 < bounds.length; i++) {
    const start = bounds[i], next = bounds[i + 1];
    const last = i + 2 === bounds.length;
    const end = last ? next : start + t.slice(start, next).replace(/\s+$/, "").length;
    out.push({ start, end, next });
  }
  return out;
}

// ---------------------------------------------------------------------------
// Tabs
// ---------------------------------------------------------------------------

function selected() { return ORDER.filter(p => state.platforms.has(p)); }

// mode is how platform p is laid out: "shared", "breaks" (own breaks) or
// "text" (own text).
function mode(p) {
  const ov = state.ov[p];
  if (ov.text != null) return "text";
  return ov.breaks ? "breaks" : "shared";
}

function postsOf(p) {
  const pl = planFor(p) || lastPlanFor(p);
  return pl ? pl.items.length : 0;
}

function renderTabs() {
  tabsRow.replaceChildren();
  const keys = tabKeys();
  const tab = (key, label, extra) => {
    const k = keys.indexOf(key) + 1;
    const b = el("button", {
      title: k > 0 && k < 10 ? `Alt+${k}` : null,
      class: "ed-tab" + (state.tab === key ? " on" : "") + (key === "shared" ? "" : " p-" + key),
      type: "button", role: "tab", "aria-selected": state.tab === key ? "true" : "false",
      "data-tab": key, onclick: () => setTab(key),
    }, label);
    if (extra) b.append(extra);
    tabsRow.append(b);
  };
  tab("shared", "Shared");
  for (const p of selected()) {
    const n = postsOf(p);
    const m = mode(p);
    const extra = el("span", { class: "ed-tab-meta" });
    if (n > 1) extra.append(el("span", { class: "ed-tab-n", text: String(n) }));
    if (m !== "shared") extra.append(el("span", { class: "ed-tab-mode", title: m === "text" ? "own text" : "own breaks", text: m === "text" ? "✎" : "✂" }));
    tab(p, META[p].label, extra);
  }
  if (selected().length > 1) {
    tabsRow.append(el("button", { class: "ed-compare", type: "button", title: "Every platform's thread side by side (Alt+C)",
      onclick: openCompare }, "⇆ Compare"));
  }
}

// setTab shows the shared tab ("shared") or platform p's.
export function setTab(t) {
  state.tab = t;
  picked = null;
  renderEditor();
  hooks.tab(t);
}

// renderEditor redraws the editor from state (after a load, a reset, a
// platform switched on or off, a layout change).
export function renderEditor() {
  if (!root) return;
  if (state.tab !== "shared" && !state.platforms.has(state.tab)) state.tab = "shared";
  renderTabs();
  const shared = state.tab === "shared";
  sharedPane.hidden = !shared;
  platformPane.hidden = shared;
  renderStack(SHARED, sharedPane);
  if (shared) platformPane.replaceChildren();
  else renderPlatform(state.tab);
}

// onPlan takes a new plan: post counts on tabs and cards, and a platform
// tab's thread (unless it is being typed in).
function onPlan() {
  if (!root) return;
  renderTabs();
  if (cmpOpen) renderCompare();
  refreshChips(SHARED, sharedPane);
  if (state.tab === "shared") return;
  if (mode(state.tab) === "text") refreshChips({ p: state.tab }, platformPane);
  else renderPlatform(state.tab);
}

// changed tells the composer, and the drafts autosave (which listens for
// change events in the composer), that the post changed.
function changed() {
  hooks.changed();
  root.dispatchEvent(new Event("change", { bubbles: true }));
}

// ---------------------------------------------------------------------------
// Card stack (the shared text, or a platform's own text)
// ---------------------------------------------------------------------------

function autoGrow(ta) {
  if (!ta || ta.offsetParent === null) return;
  ta.style.height = "auto";
  ta.style.height = ta.scrollHeight + "px";
}

function renderStack(d, host, keep) {
  // Redrawn under the cursor (an upload finished, accounts arrived): the
  // cursor stays where it was.
  const act = document.activeElement;
  if (!keep && act && act.classList && act.classList.contains("ec-text") && host.contains(act)) {
    keep = { card: act._card, caret: act.selectionStart };
  }
  const cs = cards(d);
  const t = docText(d);
  const list = el("div", { class: "ec-stack" });
  cs.forEach((c, i) => {
    if (i > 0) list.append(breakDivider(d, i));
    const ta = el("textarea", {
      class: "master ec-text", rows: i === 0 ? 4 : 2,
      placeholder: i === 0 ? (d.p ? META[d.p].label + "'s own wording…" : "write once — mirror to every platform…") : "",
      "aria-label": cs.length > 1 ? `post ${i + 1} of ${cs.length}` : (d.p ? META[d.p].label + " text" : "post text"),
    });
    if (i === 0 && !d.p) ta.id = "master";
    ta.value = t.slice(c.start, c.end);
    ta._val = ta.value;
    ta._card = i;
    ta.addEventListener("input", () => onCardInput(d, ta, host));
    ta.addEventListener("keydown", e => onCardKey(d, ta, e, host));
    const card = el("div", { class: "ecard", "data-card": String(i) }, ta, cardFoot(d, cs, i, host));
    card.addEventListener("dragover", e => { if (dragging(e)) { e.preventDefault(); card.classList.add("drop"); } });
    card.addEventListener("dragleave", () => card.classList.remove("drop"));
    card.addEventListener("drop", e => {
      card.classList.remove("drop");
      const id = e.dataTransfer && e.dataTransfer.getData("text/x-publisher-media");
      if (!id) return;
      e.preventDefault();
      moveMedia(d, id, i, host);
    });
    list.append(card);
  });
  host.replaceChildren(...(d.p ? [platformHead(d.p)] : []), list);
  requestAnimationFrame(() => host.querySelectorAll("textarea.ec-text").forEach(autoGrow));
  if (keep) {
    const ta = host.querySelectorAll("textarea.ec-text")[Math.min(keep.card, cs.length - 1)];
    if (ta) {
      ta.focus();
      const at = Math.min(keep.caret, ta.value.length);
      ta.setSelectionRange(at, at);
    }
  }
  refreshChips(d, host);
}

function dragging(e) {
  return e.dataTransfer && [...e.dataTransfer.types].includes("text/x-publisher-media");
}

function breakDivider(d, i) {
  return el("div", { class: "ec-break" },
    el("span", { class: "ec-break-line" }),
    el("span", { class: "ec-break-lbl", text: "new post" }),
    el("button", { class: "ec-break-x", type: "button", title: "Remove this break (join the posts)", "aria-label": "remove break",
      onclick: () => merge(d, i, d.p ? platformPane : sharedPane) }, "×"),
    el("span", { class: "ec-break-line" }));
}

function cardFoot(d, cs, i, host) {
  const foot = el("div", { class: "ec-foot" });
  foot.append(el("span", { class: "ec-chips" }));
  foot.append(el("span", { class: "grow" }));
  foot.append(el("button", { class: "ec-split ghost xs", type: "button", title: "Split the post at the cursor (Ctrl/⌘+Enter)",
    onmousedown: e => e.preventDefault(), // keep the textarea's caret
    onclick: () => {
      const ta = host.querySelectorAll("textarea.ec-text")[i];
      if (ta) split(d, i, ta.selectionStart ?? ta.value.length, host);
    } }, "✂ split"));
  if (cs.length > 1) {
    const media = mediaOn(d, cs, i);
    if (media.length) foot.append(mediaStrip(d, cs, media, host));
  }
  return foot;
}

function onCardInput(d, ta, host) {
  const before = cards(d);
  const c = before[ta._card];
  if (!c) return;
  const e = diff(ta._val, ta.value);
  applyEdit(d, c.start + e.at, e.removed, e.inserted);
  ta._val = ta.value;
  // A finished line of "---" is a break.
  let made = null;
  for (let m = nextMarker(docText(d)); m; m = nextMarker(docText(d))) {
    applyEdit(d, m.at, m.removed, "");
    setDocBreaks(d, [...docBreaks(d), m.at]);
    made = m.at;
  }
  const after = cards(d);
  if (made != null) {
    const k = after.findIndex(x => x.start === made);
    renderStack(d, host, { card: k >= 0 ? k : after.length - 1, caret: 0 });
  } else if (after.length !== before.length) {
    renderStack(d, host, { card: ta._card, caret: ta.selectionStart });
  } else {
    autoGrow(ta);
  }
  changed();
}

function onCardKey(d, ta, e, host) {
  const i = ta._card;
  const collapsed = ta.selectionStart === ta.selectionEnd;
  if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) {
    e.preventDefault();
    split(d, i, ta.selectionStart, host);
  } else if (e.key === "Backspace" && collapsed && ta.selectionStart === 0 && i > 0) {
    e.preventDefault();
    merge(d, i, host);
  } else if (e.key === "Delete" && collapsed && ta.selectionStart === ta.value.length && i < cards(d).length - 1) {
    e.preventDefault();
    merge(d, i + 1, host, { card: i, caret: ta.selectionStart });
  }
}

// split adds a break in card i at caret.
function split(d, i, caret, host) {
  const cs = cards(d);
  const t = docText(d);
  const at = splitPoint(t, cs[i].start + caret);
  if (at <= cs[i].start || at > t.length || docBreaks(d).includes(at)) {
    flash("Put the cursor where the next post should start");
    return;
  }
  if (docBreaks(d).length >= MAX_BREAKS) { flash("A thread has at most 100 posts"); return; }
  setDocBreaks(d, [...docBreaks(d), at]);
  renderStack(d, host, { card: i + 1, caret: 0 });
  renderTabs();
  changed();
}

// merge removes the break that starts card i, joining it to the card above.
function merge(d, i, host, keep) {
  const cs = cards(d);
  if (i <= 0 || i >= cs.length) return;
  const b = cs[i].start;
  setDocBreaks(d, docBreaks(d).filter(x => x !== b));
  renderStack(d, host, keep || { card: i - 1, caret: b - cs[i - 1].start });
  renderTabs();
  changed();
}

// ---------------------------------------------------------------------------
// Media on cards
// ---------------------------------------------------------------------------

function mediaOn(d, cs, i) {
  const media = docMedia(d);
  return state.images.filter(img => {
    const at = media[img.id] || 0;
    return at >= cs[i].start && (i === cs.length - 1 || at < cs[i + 1].start);
  });
}

function mediaStrip(d, cs, imgs, host) {
  const strip = el("span", { class: "ec-media" });
  for (const img of imgs) {
    const n = state.images.indexOf(img) + 1;
    const thumb = img.video
      ? el("span", { class: "ec-thumb vid", text: "▶" })
      : el("img", { class: "ec-thumb", src: img.poster_url || img.url, alt: img.alt || "attachment " + n });
    const sel = el("select", { class: "ec-move", "aria-label": "Move attachment " + n + " to…",
      onchange: e => { moveMedia(d, img.id, parseInt(e.target.value, 10), host); } });
    sel.append(el("option", { value: "", text: "Move to…", disabled: "disabled" }));
    cs.forEach((_, j) => sel.append(el("option", { value: String(j), text: "post " + (j + 1) })));
    sel.value = "";
    const item = el("span", { class: "ec-mitem", draggable: "true", title: "Drag to another post, or use Move to…" }, thumb, sel);
    item.addEventListener("dragstart", e => {
      e.dataTransfer.setData("text/x-publisher-media", img.id);
      e.dataTransfer.effectAllowed = "move";
    });
    strip.append(item);
  }
  return strip;
}

// moveMedia puts attachment id on card j of d.
function moveMedia(d, id, j, host) {
  const cs = cards(d);
  if (!(j >= 0 && j < cs.length)) return;
  docMedia(d)[id] = cs[j].start;
  renderStack(d, host);
  changed();
}

// ---------------------------------------------------------------------------
// Post counts on cards: how each platform's plan splits a card
// ---------------------------------------------------------------------------

// followers are the platforms whose posts d's breaks shape.
function followers(d) {
  if (d.p) return [d.p];
  return selected().filter(p => mode(p) === "shared");
}

// planStarts is where platform p's planned posts start, as UTF-16 offsets
// into the text they come from.
function planStarts(p) {
  const pl = planFor(p) || lastPlanFor(p);
  if (!pl || !pl.items || !pl.items.length) return null;
  const ix = cpIndex(pl.text || "");
  return pl.items.map(it => ix[Math.min(it.range ? it.range[0] : 0, ix.length - 1)]);
}

function refreshChips(d, host) {
  const cs = cards(d);
  const spots = host.querySelectorAll(".ecard .ec-chips");
  const plats = followers(d);
  const starts = Object.fromEntries(plats.map(p => [p, planStarts(p)]));
  spots.forEach((spot, i) => {
    spot.replaceChildren();
    if (!cs[i]) return;
    for (const p of plats) {
      const s = starts[p];
      if (!s) continue;
      const n = s.filter(x => x >= cs[i].start && (i === cs.length - 1 || x < cs[i + 1].start)).length;
      if (!n) continue;
      spot.append(el("span", { class: "ec-chip p-" + p + (n > 1 ? " split" : ""),
        title: n > 1 ? `${META[p].label} splits this into ${n} posts to fit` : `${META[p].label}: one post`,
        text: `${META[p].label} ${n}` }));
    }
  });
}

// ---------------------------------------------------------------------------
// Platform tab
// ---------------------------------------------------------------------------

function platformHead(p) {
  const m = mode(p);
  const it = state.interaction;
  const head = el("div", { class: "pp-head p-" + p }, brandTile(p, { size: 22, r: 6 }));
  const badge = { shared: "Shared breaks", breaks: "Own breaks", text: "Own text" }[m];
  head.append(el("span", { class: "pp-mode" + (m === "shared" ? "" : " own"), text: badge }));
  head.append(el("span", { class: "grow" }));
  if (it) {
    head.append(el("span", { class: "muted pp-note", text: "In a reply or quote, breaks come from the Shared tab" }));
    return head;
  }
  if (m === "breaks") {
    head.append(el("button", { class: "ghost xs", type: "button", text: "Reset to shared",
      onclick: () => { state.ov[p].breaks = null; renderEditor(); changed(); } }));
  }
  if (m === "text") {
    head.append(el("button", { class: "ghost xs", type: "button", text: "Back to shared text",
      onclick: () => confirmModal({
        title: `Use the shared text on ${META[p].label}?`,
        body: `${META[p].label}'s own wording is thrown away.`,
        confirmText: "Use shared text",
        onConfirm: async () => {
          Object.assign(state.ov[p], { text: null, breaks: null, mediaAt: null });
          renderEditor(); changed();
          return true;
        },
      }) }));
  } else {
    head.append(el("button", { class: "ghost xs", type: "button", text: `Edit wording for ${META[p].label}`,
      onclick: () => {
        const ov = state.ov[p];
        const breaks = [...layoutBreaks(p)];
        Object.assign(ov, { text: state.master, breaks, mediaAt: { ...state.anchors } });
        renderEditor(); changed();
      } }));
  }
  return head;
}

function renderPlatform(p) {
  picked = picked && picked.p === p ? picked : null;
  if (mode(p) === "text" && !state.interaction) { renderStack({ p }, platformPane); return; }
  const fresh = planFor(p);
  const pl = fresh || lastPlanFor(p);
  const head = platformHead(p);
  if (!pl) {
    platformPane.replaceChildren(head, el("p", { class: "muted pp-wait", text: "Planning…" }));
    return;
  }
  platformPane.replaceChildren(head, thread(p, pl, !!fresh));
}

// thread draws platform p's planned posts. Break controls work on a fresh
// plan only: a stale one's positions may not match the text any more.
function thread(p, pl, fresh) {
  const text = pl.text || "";
  const ix = cpIndex(text);
  const u = cp => ix[Math.min(cp, ix.length - 1)];
  const items = pl.items || [];
  const editable = fresh && !state.interaction;
  const box = el("div", { class: "pp-thread p-" + p + (fresh ? "" : " stale") });
  const media = postableImages();
  items.forEach((item, i) => {
    const from = u(item.range[0]), to = u(item.range[1]);
    if (i > 0) box.append(breakLine(p, items, i, u, text, editable));
    const body = el("div", { class: "pp-text" });
    for (const w of words(text, from, to)) {
      if (w.space) { body.append(document.createTextNode(w.s)); continue; }
      const span = el("span", { class: "pp-w" + (/^(https?:\/\/|#|@)/.test(w.s) ? " pv-tok" : "") + (picked && picked.at === w.at ? " picked" : ""),
        "data-at": String(w.at), text: w.s });
      if (editable) span.addEventListener("click", () => pick(p, w.at, box));
      body.append(span);
    }
    const suffix = item.text && item.text.trim() !== text.slice(from, to).trim() ? counterOf(item.text) : "";
    if (suffix) body.append(el("span", { class: "pp-num", text: suffix }));
    const over = pl.limit && item.length > pl.limit;
    const meta = el("div", { class: "pp-meta" },
      el("span", { class: "pp-count" + (over ? " over" : ""), text: pl.limit ? `${item.length}/${pl.limit}` : `${item.length}` }));
    if (pl.link_card && pl.link_card.item === i) meta.append(el("span", { class: "pp-card", text: "🔗 link card" }));
    const its = (item.media || []).map(k => media[k]).filter(Boolean);
    if (its.length) meta.append(el("span", { class: "pp-media", text: "🖼 " + its.length }));
    meta.append(el("span", { class: "grow" }));
    const post = el("div", { class: "pp-post", "data-post": String(i) },
      el("div", { class: "pp-rail" }, el("span", { class: "pp-n", text: String(i + 1) })),
      el("div", { class: "pp-main" }, body, meta,
        ...(item.problems || []).map(pr => el("div", { class: "pp-problem", text: "⚠ " + pr }))));
    box.append(post);
  });
  if (editable && picked) pick(p, picked.at, box, true);
  for (const w of pl.warnings || []) box.append(el("div", { class: "pv-warn", text: w }));
  if (editable) box.append(el("p", { class: "muted pp-hint", text: "Tap a word, then ✂ Break here. ↑ ↓ move a break a sentence (Shift: a word); drag a break onto a word to move it there." }));
  return box;
}

function counterOf(text) {
  const m = /\s(\d+\/\d+)$/.exec(text);
  return m ? " " + m[1] : "";
}

function postableImages() {
  return state.images.filter(i => i.media_id && !(i.video && i.phase !== "ready"));
}

// words cuts text[from, to) into words and the whitespace between them.
function words(text, from, to) {
  const out = [];
  const re = /\s+|\S+/g;
  const s = text.slice(from, to);
  let m;
  while ((m = re.exec(s)) !== null) out.push({ s: m[0], at: from + m.index, space: /^\s/.test(m[0]) });
  return out;
}

// pick marks the word at `at` (or unmarks it, tapped again) and offers
// "Break here" on its post, unless it already starts the post.
function pick(p, at, box, again = false) {
  picked = !again && picked && picked.at === at ? null : { p, at };
  box.querySelectorAll(".pp-w.picked").forEach(n => n.classList.remove("picked"));
  box.querySelectorAll(".pp-here").forEach(b => b.remove());
  if (!picked) return;
  const w = box.querySelector(`.pp-w[data-at="${at}"]`);
  if (!w) { picked = null; return; }
  w.classList.add("picked");
  const post = w.closest(".pp-post");
  if (post.querySelector(".pp-w") === w) return;
  post.querySelector(".pp-meta").append(el("button", { class: "ghost xs pp-here", type: "button",
    title: "Start a new post at the word you picked", text: "✂ Break here",
    onclick: () => { picked = null; setOwn(p, [...layoutBreaks(p), at]); } }));
}

// setOwn gives platform p its own breaks (on the text it posts).
function setOwn(p, breaks) {
  const text = effectiveText(p);
  state.ov[p].breaks = cleanBreaks(text, breaks, MAX_BREAKS);
  picked = null;
  renderEditor();
  changed();
}

// handMade is the hand-made break that starts post i, if one does: the
// last of the platform's breaks after post i-1's start and at or before
// post i's.
function handMade(p, items, i, u) {
  const lo = u(items[i - 1].range[0]), hi = u(items[i].range[0]);
  const bs = layoutBreaks(p).filter(b => b > lo && b <= hi);
  return bs.length ? bs[bs.length - 1] : null;
}

function breakLine(p, items, i, u, text, editable) {
  const kind = items[i].break === "manual" ? "manual" : "auto";
  const line = el("div", { class: "pp-break " + kind, "data-break": String(i),
    title: kind === "manual" ? "A break you made" : "Split to fit the limit" });
  line.append(el("span", { class: "pp-break-line" }),
    el("span", { class: "pp-break-lbl", text: kind === "manual" ? "break" : "auto split" }));
  if (editable) {
    const move = (dir, byWord) => moveBreak(p, items, i, u, text, dir, byWord);
    line.append(
      el("button", { class: "pp-bb", type: "button", title: "Earlier (a sentence; Shift: a word)", "aria-label": "move break earlier",
        onclick: e => move(-1, e.shiftKey) }, "↑"),
      el("button", { class: "pp-bb", type: "button", title: "Later (a sentence; Shift: a word)", "aria-label": "move break later",
        onclick: e => move(1, e.shiftKey) }, "↓"));
    if (kind === "manual") {
      line.append(el("button", { class: "pp-bb", type: "button", title: "Remove this break", "aria-label": "remove break",
        onclick: () => { const m = handMade(p, items, i, u); setOwn(p, layoutBreaks(p).filter(b => b !== m)); } }, "×"));
    }
    line.addEventListener("pointerdown", e => {
      if (e.target.closest("button")) return;
      e.preventDefault();
      line.classList.add("dragging");
      const up = ev => {
        document.removeEventListener("pointerup", up);
        line.classList.remove("dragging");
        const w = document.elementFromPoint(ev.clientX, ev.clientY);
        const at = w && w.closest && w.closest(".pp-w") ? +w.closest(".pp-w").dataset.at : NaN;
        if (Number.isInteger(at)) placeBreak(p, items, i, u, at);
      };
      document.addEventListener("pointerup", up);
    });
  }
  line.append(el("span", { class: "pp-break-line" }));
  return line;
}

// moveBreak moves the break before post i one sentence (or word) earlier
// (dir -1) or later (+1), keeping a word in each of the two posts.
function moveBreak(p, items, i, u, text, dir, byWord) {
  const lo = u(items[i - 1].range[0]), at = u(items[i].range[0]), hi = u(items[i].range[1]);
  const spots = (byWord ? wordStarts : sentenceStarts)(text, 0, text.length).filter(x => x > lo && x < hi);
  const to = dir < 0 ? spots.filter(x => x < at).pop() : spots.find(x => x > at);
  if (to == null) { flash(dir < 0 ? "Nothing earlier to move it to" : "Nothing later to move it to"); return; }
  placeBreak(p, items, i, u, to);
}

// placeBreak puts the break before post i at `at`: a hand-made one moves, a
// split made to fit becomes a hand-made one there.
function placeBreak(p, items, i, u, at) {
  const m = handMade(p, items, i, u);
  const rest = layoutBreaks(p).filter(b => b !== m);
  setOwn(p, [...rest, at]);
}

export { SHARED };

// ---------------------------------------------------------------------------
// Compare: every platform's thread side by side (wide screens)
// ---------------------------------------------------------------------------

let cmpOpen = null; // the open comparison's backdrop

function openCompare() {
  if (cmpOpen) return;
  cmpOpen = el("div", { class: "modal-bk cmp-bk" });
  cmpOpen.addEventListener("click", e => { if (e.target === cmpOpen) closeCompare(); });
  document.body.append(cmpOpen);
  renderCompare();
}

function closeCompare() {
  if (cmpOpen) cmpOpen.remove();
  cmpOpen = null;
}

// renderCompare draws one column per selected platform, post k of each on
// row k, so a post that ends mid-sentence or a link left alone stands out.
// A column's header or post opens that platform's tab.
function renderCompare() {
  const plats = selected();
  const plans = plats.map(p => ({ p, pl: planFor(p) || lastPlanFor(p) }));
  const rows = Math.max(1, ...plans.map(x => (x.pl ? x.pl.items.length : 0)));
  const grid = el("div", { class: "cmp-grid", role: "table" });
  grid.style.gridTemplateColumns = `repeat(${plats.length}, minmax(220px, 1fr))`;
  const open = p => { closeCompare(); setTab(p); document.getElementById("editor")?.scrollIntoView({ block: "start" }); };
  for (const { p, pl } of plans) {
    const n = pl ? pl.items.length : 0;
    grid.append(el("button", { class: "cmp-head p-" + p, type: "button", role: "columnheader", onclick: () => open(p) },
      brandTile(p, { size: 20, r: 5 }), el("span", { class: "cmp-name", text: META[p].label }),
      el("span", { class: "muted", text: n === 1 ? "1 post" : `${n} posts` }),
      mode(p) !== "shared" ? el("span", { class: "pp-mode own", text: mode(p) === "text" ? "own text" : "own breaks" }) : null));
  }
  for (let k = 0; k < rows; k++) {
    for (const { p, pl } of plans) {
      const item = pl && pl.items[k];
      if (!item) { grid.append(el("div", { class: "cmp-cell empty" })); continue; }
      const over = pl.limit && item.length > pl.limit;
      grid.append(el("div", { class: "cmp-cell p-" + p + (k > 0 ? " brk-" + (item.break === "manual" ? "manual" : "auto") : ""),
        role: "cell", tabindex: "0", title: `Open ${META[p].label}'s tab`,
        onclick: () => open(p), onkeydown: e => { if (e.key === "Enter") open(p); } },
        el("div", { class: "cmp-text", text: item.text }),
        el("div", { class: "cmp-meta" },
          el("span", { class: "pp-count" + (over ? " over" : ""), text: pl.limit ? `${item.length}/${pl.limit}` : String(item.length) }),
          (item.media || []).length ? el("span", { text: "🖼 " + item.media.length }) : null,
          (item.problems || []).length ? el("span", { class: "pp-problem", text: "⚠ " + item.problems.join("; ") }) : null)));
    }
  }
  const card = el("div", { class: "modal cmp" },
    el("div", { class: "cmp-top" },
      el("p", { class: "modal-title", text: "Compare threads" }),
      el("span", { class: "muted", text: "Solid line: your break · dashed: split to fit" }),
      el("span", { class: "grow" }),
      el("button", { class: "ghost sm", type: "button", text: "Close", onclick: closeCompare })),
    grid);
  cmpOpen.replaceChildren(card);
}
