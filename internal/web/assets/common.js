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
    // Inline style attributes are blocked by the CSP (style-src 'self'); the
    // CSSOM is not, so styles go through n.style instead of setAttribute.
    else if (k === "style") n.style.cssText = v;
    else if ((k === "href" || k === "src" || k === "poster") && v !== false && v != null) n.setAttribute(k, safeURL(v));
    else if (v !== false && v != null) n.setAttribute(k, v);
  }
  for (const kid of kids) if (kid != null) n.append(kid);
  return n;
}

// pressable makes a clickable non-button element usable from the keyboard:
// it takes focus, says what it is, and Enter or Space clicks it.
export function pressable(n, role = "button") {
  n.tabIndex = 0;
  n.setAttribute("role", role);
  n.addEventListener("keydown", e => {
    if (e.target !== n || (e.key !== "Enter" && e.key !== " ")) return;
    e.preventDefault();
    n.click();
  });
  return n;
}

// The dialogs on screen, the newest last: only the top one answers Escape
// and keeps Tab inside it.
const dialogs = [];
const FOCUSABLE = 'button:not([disabled]), [href], input:not([disabled]):not([type="hidden"]):not([hidden]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

function topDialog() {
  while (dialogs.length && !dialogs[dialogs.length - 1].bk.isConnected) dialogs.pop();
  return dialogs[dialogs.length - 1];
}

// restoreFocus puts focus back on the element a dialog was opened from:
// once it is enabled again (a button disabled while its request runs), or
// on its replacement (an element with the same data-focus-key, else its
// data-focus-fallback) when the list it was in was redrawn. It gives up
// when something else takes focus.
export function restoreFocus(before) {
  if (!before) return;
  const key = before.dataset ? before.dataset.focusKey : "";
  const fallback = before.dataset ? before.dataset.focusFallback : "";
  const byKey = k => k ? document.querySelector('[data-focus-key="' + CSS.escape(k) + '"]') : null;
  let tries = 0;
  const go = () => {
    const active = document.activeElement;
    if (active && active !== document.body && active !== before) return;
    let t = before.isConnected ? before : byKey(key) || byKey(fallback);
    if (t && t.disabled && !before.isConnected && byKey(fallback)) t = byKey(fallback);
    if (t && !t.disabled && typeof t.focus === "function") { t.focus(); return; }
    if (tries++ < 60) setTimeout(go, 50);
  };
  go();
}

// asDialog gives a modal its dialog semantics: role and aria-modal on the
// card, a label (its title) and a description (.modal-desc), focus moved
// into it (focusEl, unless something in it already has focus), Escape
// calls close (the top dialog's only), Tab stays inside, and focus goes
// back where it was (see restoreFocus) once it closes. It answers close
// wrapped to do that cleanup. escape: false leaves Escape alone (a dialog
// whose content can't be shown again).
export function asDialog(bk, card, close, focusEl, { escape = true } = {}) {
  const before = document.activeElement;
  card.setAttribute("role", "dialog");
  card.setAttribute("aria-modal", "true");
  const title = card.querySelector(".modal-title, .pm-title");
  if (title) { title.id = title.id || "dlg-" + Math.random().toString(36).slice(2, 9); card.setAttribute("aria-labelledby", title.id); }
  const desc = card.querySelector(".modal-desc");
  if (desc) { desc.id = desc.id || "dlg-" + Math.random().toString(36).slice(2, 9); card.setAttribute("aria-describedby", desc.id); }
  const me = { bk };
  dialogs.push(me);
  const onKey = e => {
    if (!bk.isConnected) { document.removeEventListener("keydown", onKey); return; }
    if (topDialog() !== me) return;
    if (escape && e.key === "Escape") { e.preventDefault(); done(); return; }
    if (e.key !== "Tab") return;
    const fs = [...card.querySelectorAll(FOCUSABLE)].filter(n => n.offsetParent !== null || n === document.activeElement);
    if (!fs.length) { e.preventDefault(); return; }
    const first = fs[0], last = fs[fs.length - 1];
    if (!card.contains(document.activeElement)) { e.preventDefault(); first.focus(); }
    else if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
  };
  let closed = false;
  const done = () => {
    if (closed) return;
    closed = true;
    document.removeEventListener("keydown", onKey);
    const i = dialogs.indexOf(me);
    if (i >= 0) dialogs.splice(i, 1);
    close();
    restoreFocus(before);
  };
  document.addEventListener("keydown", onKey);
  queueMicrotask(() => {
    if (card.contains(document.activeElement)) return;
    const f = focusEl || card.querySelector(FOCUSABLE);
    if (f) f.focus();
  });
  return done;
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

// Times are entered and shown in the device's own time zone, as the
// datetime-local input reads them: TZ names it, whenText formats one.
export const TZ = (() => { try { return Intl.DateTimeFormat().resolvedOptions().timeZone || "local time"; } catch { return "local time"; } })();
export function whenText(d) {
  return d.toLocaleString(undefined, { weekday: "short", day: "2-digit", month: "short", year: "numeric", hour: "2-digit", minute: "2-digit" });
}

// localInput turns an RFC 3339 time into a datetime-local value (local
// wall time, to the minute); "" for none or a bad one.
export function localInput(iso) {
  const d = iso ? new Date(iso) : null;
  if (!d || isNaN(d)) return "";
  const pad = n => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

export function flash(msg, ms = 2600) {
  const t = el("div", { class: "toast", text: msg });
  document.body.append(t);
  setTimeout(() => t.remove(), ms);
}

// api wraps fetch with same-origin creds + JSON parse; throws Error(data.error||HTTP n) on !ok.
export async function api(path, opts = {}) {
  const r = await fetch(path, { credentials: "same-origin", ...opts });
  const data = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error((data && data.error) || ("HTTP " + r.status));
  return data;
}

// v1 calls the /v1 API: a JSON body in (when given), JSON out, same-origin
// credentials. A failure throws an Error whose message is the problem's
// detail, or its field errors' details when it lists some; the Error carries
// .status, .code and .problem (RFC 9457) for callers that branch on them.
//
// An operation that needs the owner to confirm it's them (403
// reauth_required) goes through the re-authentication dialog (see
// setReauthHandler) and is sent once more with its nonce, unless reauth is
// false (the caller passes a nonce it got itself).
export async function v1(path, { method = "GET", body, headers = {}, signal, reauth = true } = {}) {
  const opts = { method, credentials: "same-origin", headers: { ...headers }, signal };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  for (let attempt = 0; ; attempt++) {
    const r = await fetch(path, opts);
    const data = r.status === 204 ? null : await r.json().catch(() => null);
    if (r.ok) return data;
    const err = problemError(r.status, data);
    const ra = Number(r.headers.get("Retry-After"));
    if (ra > 0) err.retryAfter = ra;
    if (reauth && attempt === 0 && err.code === "reauth_required" && reauthHandler && data && data.operation) {
      const nonce = await reauthHandler(data.operation, data.target || "");
      if (nonce) { opts.headers["Publisher-Reauth"] = nonce; continue; }
      err.message = "Cancelled: you didn't confirm it's you.";
      err.cancelled = true;
    }
    throw err;
  }
}

// setReauthHandler sets what v1 asks for a re-authentication nonce: a
// function of the operation (and, for an operation on one account, its
// id) answering the nonce, or null when the owner cancelled.
let reauthHandler = null;
export function setReauthHandler(fn) { reauthHandler = fn; }

// problemError turns a /v1 problem body into an Error.
export function problemError(status, p) {
  const fe = p && Array.isArray(p.errors) ? p.errors : [];
  const msg = fe.length
    ? fe.map(e => e.detail || e.code).join("; ")
    : (p && (p.detail || p.title)) || ("HTTP " + status);
  const err = new Error(msg);
  err.status = status;
  err.code = (p && p.code) || "";
  err.problem = p;
  return err;
}

// v1All reads every page of a /v1 list (bounded, so a server bug can't
// loop forever).
export async function v1All(path) {
  let out = [], cursor = "";
  for (let i = 0; i < 50; i++) {
    const sep = path.includes("?") ? "&" : "?";
    const page = await v1(path + (cursor ? sep + "cursor=" + encodeURIComponent(cursor) : ""));
    out = out.concat((page && page.data) || []);
    if (!page || !page.next_cursor) break;
    cursor = page.next_cursor;
  }
  return out;
}

// confirmModal shows a small yes/no dialog. onConfirm is async and returns true
// to dismiss (false keeps it open, e.g. on failure).
export function confirmModal({ title, body, confirmText, onConfirm }) {
  const bk = el("div", { class: "modal-bk" });
  let close = () => bk.remove();
  bk.addEventListener("click", e => { if (e.target === bk) close(); });
  const card = el("div", { class: "modal confirm" });
  card.append(el("p", { class: "modal-title", text: title }));
  if (body) card.append(el("p", { class: "confirm-body", text: body }));
  const cancel = el("button", { class: "ghost sm", type: "button", text: "Cancel", onclick: () => close() });
  card.append(el("div", { class: "confirm-actions" },
    cancel,
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
  close = asDialog(bk, card, close, cancel); // Cancel first: Enter must not confirm by accident
}

// inputDialog asks for one value (or a few: fields); onSubmit gets the
// values and a function that says something under them, and answers true
// to close.
export function inputDialog({ title, label, value = "", type = "text", maxlength, fields, submitText, onSubmit }) {
  const bk = el("div", { class: "modal-bk" });
  let close = () => bk.remove();
  const err = el("p", { class: "reauth-status", role: "status", "aria-live": "polite" });
  const inputs = (fields || [{ label, value, type, maxlength }]).map((f, i) => {
    const id = "dlg-in-" + i;
    return { label: el("label", { for: id, text: f.label }), input: el("input", { id, type: f.type || "text", value: f.value || "", autocomplete: f.autocomplete || "off", maxlength: f.maxlength || false }) };
  });
  const ok = el("button", { class: "primary sm", type: "submit", text: submitText || "Save" });
  const form = el("form", { class: "reauth" },
    ...inputs.flatMap(x => [x.label, x.input]), err,
    el("div", { class: "confirm-actions" }, el("button", { class: "ghost sm", type: "button", text: "Cancel", onclick: () => close() }), ok));
  form.addEventListener("submit", async ev => {
    ev.preventDefault();
    if (ok.disabled) return; // one at a time
    ok.disabled = true;
    try {
      if (await onSubmit(inputs.map(x => x.input.value), m => { err.textContent = m; })) close();
    } finally {
      ok.disabled = false;
    }
  });
  const card = el("div", { class: "modal reauth" }, el("p", { class: "modal-title", text: title }), form);
  bk.append(card);
  document.body.append(bk);
  close = asDialog(bk, card, close, inputs[0].input);
}
