"use strict";
import { el, $, api, flash } from "./common.js";
import { startInteraction } from "./compose.js";

const PLAT_LABEL = { bluesky: "Bluesky", mastodon: "Mastodon", nostr: "Nostr", threads: "Threads" };

let _seq = 0;

// resolveInput fetches the source post for the pasted URL/id and renders it.
async function resolveInput(input) {
  const card = $("#srccard"), status = $("#srcstatus");
  card.innerHTML = "";
  if (!input.trim()) { status.textContent = ""; return; }
  status.textContent = "Resolving…";
  const seq = ++_seq;
  let data;
  try {
    data = await api("/api/resolve", {
      method: "POST", headers: { "content-type": "application/json" },
      body: JSON.stringify({ input }),
    });
  } catch (e) {
    if (seq !== _seq) return;
    status.textContent = "✗ " + e.message;
    return;
  }
  if (seq !== _seq) return; // a newer input superseded this response
  status.textContent = "";
  card.append(renderSource(data));
}

// renderSource builds the preview card + action row.
function renderSource(s) {
  const card = el("div", { class: "src-card p-" + s.platform });
  const p = s.preview;
  card.append(el("div", { class: "src-head" },
    el("span", { class: "src-plat", text: PLAT_LABEL[s.platform] || s.platform }),
    el("span", { class: "src-author", text: p.author_name || "" }),
    el("span", { class: "src-handle muted", text: p.author_handle || "" }),
  ));
  card.append(el("div", { class: "src-text", text: p.text || "" }));
  if (p.media && p.media.length) {
    const g = el("div", { class: "src-media" });
    for (const m of p.media) g.append(el("img", { src: m.url, alt: m.alt || "" }));
    card.append(g);
  }
  if (p.web_url) card.append(el("a", { class: "src-link", href: p.web_url, target: "_blank", rel: "noopener", text: "open original ↗" }));
  card.append(actionRow(s));
  return card;
}

function actionRow(s) {
  const row = el("div", { class: "act-panel" });
  for (const [action, cap] of [["reply", s.caps.reply], ["repost", s.caps.repost], ["quote", s.caps.quote]]) {
    const btn = el("button", { class: "act-btn", type: "button", text: action[0].toUpperCase() + action.slice(1) });
    if (!cap.allowed) { btn.classList.add("blocked"); btn.title = cap.reason || "not allowed"; }
    btn.addEventListener("click", () => {
      if (action === "repost") { doRepost(s, cap); return; }
      startInteraction(s, action); // reply/quote → Compose interaction mode (override handled in the banner)
    });
    row.append(btn);
  }
  return row;
}

// doRepost posts a one-click repost via /api/interact (multipart spec, no media).
async function doRepost(s, cap) {
  const send = async (force) => {
    const fd = new FormData();
    fd.append("spec", JSON.stringify({
      action: "repost", platform: s.platform, ref: s.ref,
      source_url: s.preview.web_url, source_author: s.preview.author_handle, force: !!force,
    }));
    try {
      const r = await fetch("/api/interact", { method: "POST", body: fd, credentials: "same-origin" });
      const data = await r.json();
      if (!r.ok) throw new Error(data.error || ("HTTP " + r.status));
      flash("repost " + data.status);
    } catch (e) { flash("Error: " + e.message); }
  };
  if (!cap.allowed) {
    if (window.confirm("repost: " + (cap.reason || "blocked") + " — try anyway?")) send(true); // eslint-disable-line no-alert
    return;
  }
  send(false);
}

// interactInit wires the smart input (debounced).
export function interactInit() {
  const inp = $("#srcinput");
  if (!inp) return;
  let t = null;
  inp.addEventListener("input", e => {
    clearTimeout(t);
    const v = e.target.value;
    t = setTimeout(() => resolveInput(v), 350);
  });
}
