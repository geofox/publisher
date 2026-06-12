"use strict";

export const META = {
  bluesky:  { label: "Bluesky",  limit: 300 },
  mastodon: { label: "Mastodon", limit: 500 },
  threads:  { label: "Threads",  limit: 500 },
  nostr:    { label: "Nostr",    limit: 0 },
};
export const ORDER = ["bluesky", "mastodon", "threads", "nostr"];

export const $ = sel => document.querySelector(sel);

// safeURL neutralizes dangerous URL schemes on href/src attributes. Most values
// are first-party, but remote_url comes back from external platforms (e.g. a
// Mastodon instance), so a "javascript:"/"data:" link could otherwise run script
// in the owner's authenticated session when clicked. Browsers ignore leading
// whitespace and control chars when resolving a scheme, so strip those first.
function safeURL(v) {
  const scheme = String(v).replace(/[\s\x00-\x1f]+/g, "").toLowerCase();
  return /^(javascript|data|vbscript):/.test(scheme) ? "#" : v;
}

export function el(tag, attrs = {}, ...kids) {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "class") n.className = v;
    else if (k === "text") n.textContent = v;
    else if (k.startsWith("on")) n.addEventListener(k.slice(2), v);
    else if ((k === "href" || k === "src" || k === "poster") && v !== false && v != null) n.setAttribute(k, safeURL(v));
    else if (v !== false && v != null) n.setAttribute(k, v);
  }
  for (const kid of kids) if (kid != null) n.append(kid);
  return n;
}

// Intl.Segmenter construction is costly and grapheme counting runs several times
// per keystroke, so build one segmenter lazily and reuse it. `false` means the
// engine lacks Intl.Segmenter → fall back to code points.
let _seg;
function segmenter() {
  if (_seg === undefined) {
    try { _seg = new Intl.Segmenter(undefined, { granularity: "grapheme" }); }
    catch (_) { _seg = false; }
  }
  return _seg;
}

// gcount counts graphemes without materializing an array.
export function gcount(s) {
  const seg = segmenter();
  if (!seg) return [...s].length;
  let n = 0;
  for (const _ of seg.segment(s)) n++;
  return n;
}

// graphemes returns the grapheme clusters of s (for truncation/slicing).
export function graphemes(s) {
  const seg = segmenter();
  if (!seg) return [...s];
  const out = [];
  for (const g of seg.segment(s)) out.push(g.segment);
  return out;
}
export function wcount(s) { const t = s.trim(); return t ? t.split(/\s+/).length : 0; }
export function shortID(id) { return (id || "").slice(0, 8); }
export function shortRef(s) { s = String(s); return s.length > 22 ? s.slice(0, 12) + "…" + s.slice(-6) : s; }
export function relTime(iso) {
  const d = new Date(iso), s = (Date.now() - d.getTime()) / 1000;
  if (s < 60) return "just now";
  if (s < 3600) return Math.floor(s / 60) + "m ago";
  if (s < 86400) return Math.floor(s / 3600) + "h ago";
  if (s < 604800) return Math.floor(s / 86400) + "d ago";
  return d.toLocaleDateString();
}

export function flash(msg) {
  const t = el("div", { class: "toast", text: msg });
  document.body.append(t);
  setTimeout(() => t.remove(), 2600);
}

// api wraps fetch with same-origin creds + JSON parse; throws Error(data.error||HTTP n) on !ok.
export async function api(path, opts = {}) {
  const r = await fetch(path, { credentials: "same-origin", ...opts });
  const data = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error((data && data.error) || ("HTTP " + r.status));
  return data;
}

// confirmModal shows a small yes/no dialog. onConfirm is async and returns true
// to dismiss (false keeps it open, e.g. on failure).
export function confirmModal({ title, body, confirmText, onConfirm }) {
  const bk = el("div", { class: "modal-bk" });
  const close = () => bk.remove();
  bk.addEventListener("click", e => { if (e.target === bk) close(); });
  const card = el("div", { class: "modal confirm" });
  card.append(el("p", { class: "modal-title", text: title }));
  if (body) card.append(el("p", { class: "confirm-body", text: body }));
  card.append(el("div", { class: "confirm-actions" },
    el("button", { class: "ghost sm", type: "button", text: "Cancel", onclick: close }),
    el("button", {
      class: "danger sm", type: "button", text: confirmText || "Confirm",
      onclick: async e => {
        const b = e.target; b.disabled = true; b.textContent = "…";
        try {
          const done = await onConfirm();
          if (done) close(); else { b.disabled = false; b.textContent = confirmText || "Confirm"; }
        } catch (_) { b.disabled = false; b.textContent = confirmText || "Confirm"; }
      },
    }),
  ));
  bk.append(card);
  document.body.append(bk);
}
