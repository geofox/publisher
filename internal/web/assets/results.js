"use strict";
import { el, shortRef } from "./common.js";
import { brandTile } from "./brands.js";

// Result rows for the composer's result modal, in the /api/post answer's
// shape. The composer moves to /v1 next, and these with it.

export function tgPending(s) { return s === "scheduled" || s === "sending" || s === "missed"; }
export function tgBadge(s) {
  if (s === "success") return { cls: "s", mark: "✓" };
  if (s === "partial") return { cls: "p", mark: "⚠" };
  if (s === "missed")  return { cls: "q", mark: "⏰" };
  if (s === "scheduled" || s === "sending") return { cls: "q", mark: "⏳" };
  return { cls: "f", mark: "✗" };
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

// retryStatusLine renders the auto-retry state for a target in the detail pane.
function retryStatusLine(t, maxAttempts = 6) {
  if (t.gave_up_at) {
    return el("div", { class: "retry-status gaveup",
      text: `⚠ gave up after ${t.attempt_count || maxAttempts} attempts` });
  }
  if ((t.status === "failed" || t.status === "partial") && (t.attempt_count || 0) < maxAttempts) {
    return el("div", { class: "retry-status pending",
      text: `↻ retrying (${t.attempt_count || 0}/${maxAttempts})` });
  }
  return null;
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
    brandTile(t.platform, { size: 22, r: 6 }),
    el("span", { class: "bdg " + b.cls, text: `${t.platform} ${b.mark}` }),
    mid,
    el("span", { class: "meta", text: t.latency_ms ? t.latency_ms + "ms" : "" }),
  );
  const relays = relayBlock(t, null); // read-only in modal
  const rs = retryStatusLine(t);
  const children = [row];
  if (rs) children.push(rs);
  if (relays) children.push(relays);
  return children.length > 1 ? el("div", { class: "res-wrap" }, ...children) : row;
}

