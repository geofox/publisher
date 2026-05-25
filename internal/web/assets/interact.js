"use strict";
import { el, $, api, flash, confirmModal } from "./common.js";

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

// renderSource builds the preview card + action panel.
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
  card.append(actionPanel(s));
  return card;
}

function actionPanel(s) {
  const wrap = el("div", { class: "act-panel" });
  const compose = el("div", { class: "act-compose", hidden: true });
  const text = el("textarea", { class: "act-text", placeholder: "add your comment…" });
  const fanout = el("div", { class: "act-fanout", hidden: true });
  const status = el("div", { class: "act-status muted" });
  const fanBoxes = {};
  for (const p of ["bluesky", "mastodon", "nostr", "threads"]) {
    if (p === s.platform) continue;
    const cb = el("input", { type: "checkbox" });
    fanBoxes[p] = cb;
    fanout.append(el("label", { class: "act-fan" }, cb, el("span", { text: " " + p })));
  }

  async function send(action, force) {
    status.textContent = "Working…";
    const fan = action === "quote" ? Object.keys(fanBoxes).filter((p) => fanBoxes[p].checked) : [];
    try {
      const post = await api("/api/interact", {
        method: "POST", headers: { "content-type": "application/json" },
        body: JSON.stringify({
          action, platform: s.platform, ref: s.ref,
          source_url: s.preview.web_url, source_author: s.preview.author_handle,
          text: text.value, fanout: fan, force: !!force,
        }),
      });
      const parts = (post.targets || []).map((t) => t.platform + ":" + t.status).join(", ");
      status.textContent = post.status + " — " + parts;
      flash(action + " " + post.status);
    } catch (e) {
      status.textContent = "✗ " + e.message;
    }
  }

  let pending = null; // the action whose Send button will fire
  for (const [action, cap] of [["reply", s.caps.reply], ["repost", s.caps.repost], ["quote", s.caps.quote]]) {
    const btn = el("button", { class: "act-btn", type: "button", text: action[0].toUpperCase() + action.slice(1) });
    if (!cap.allowed) { btn.classList.add("blocked"); btn.title = cap.reason || "not allowed"; }
    btn.addEventListener("click", () => {
      const proceed = (force) => {
        if (action === "repost") { send("repost", force); return; }
        pending = action;
        compose.hidden = false;
        fanout.hidden = action !== "quote";
        text.focus();
        wrap._force = force;
      };
      if (!cap.allowed) {
        let body = (cap.reason || "not allowed") + " — try anyway?";
        if (s.platform === "bluesky") {
          body += " (Bluesky may silently drop it without an error.)";
        }
        confirmModal({
          title: action[0].toUpperCase() + action.slice(1) + " blocked",
          body,
          confirmText: "Try anyway",
          onConfirm: () => { proceed(true); return true; },
        });
      } else {
        proceed(false);
      }
    });
    wrap.append(btn);
  }
  const go = el("button", { class: "act-go", type: "button", text: "Send",
    onclick: () => { if (pending) send(pending, wrap._force); } });
  compose.append(text, fanout, go);
  wrap.append(compose, status);
  return wrap;
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
