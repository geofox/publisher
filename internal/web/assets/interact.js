"use strict";
import { el, $, v1, flash } from "./common.js";
import { startInteraction, sendPost, openProgressModal } from "./compose.js";
import { setInflight } from "./state.js";
import { accountFor } from "./accounts.js";
import { brandTile } from "./brands.js";

const PLAT_LABEL = { bluesky: "Bluesky", mastodon: "Mastodon", nostr: "Nostr", threads: "Threads" };

let _seq = 0;

// fromV1 reads a POST /v1/resolve answer into the shape the composer keeps
// for a source post.
export function fromV1(r) {
  const caps = {};
  for (const a of ["reply", "repost", "quote"]) caps[a] = (r.actions && r.actions[a]) || { allowed: false };
  return {
    platform: r.platform,
    preview: {
      author_name: (r.author && r.author.name) || "",
      author_handle: (r.author && r.author.handle) || "",
      text: r.text || "",
      media: (r.media || []).filter(m => m.type !== "video").map(m => ({ url: m.url, alt: m.alt || "" })),
      web_url: r.url,
    },
    caps,
  };
}

// resolveInput looks up the pasted URL (or Nostr id) and renders the post.
async function resolveInput(input) {
  const card = $("#srccard"), status = $("#srcstatus");
  card.innerHTML = "";
  const seq = ++_seq; // before the empty case too: a lookup in flight is now stale
  if (!input.trim()) { status.textContent = ""; return; }
  status.textContent = "Resolving…";
  let data;
  try {
    data = fromV1(await v1("/v1/resolve", { method: "POST", body: { url: input.trim() } }));
    // What was typed names the post for the server again (a Nostr id, a
    // Mastodon URL); the answer's url is for showing (njump, the origin).
    data.input = input.trim();
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
    brandTile(s.platform, { size: 24, r: 7 }),
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

// actionRow offers what the post allows; a refused action shows why (the
// server refuses it too).
function actionRow(s) {
  const row = el("div", { class: "act-panel" });
  for (const [action, cap] of [["reply", s.caps.reply], ["repost", s.caps.repost], ["quote", s.caps.quote]]) {
    const btn = el("button", { class: "act-btn", type: "button", text: action[0].toUpperCase() + action.slice(1) });
    if (!cap.allowed) {
      btn.classList.add("blocked"); btn.disabled = true; btn.title = cap.reason || "not allowed";
    } else if (cap.reason) btn.title = cap.reason;
    btn.addEventListener("click", () => {
      if (!cap.allowed) return;
      if (action === "repost") { doRepost(s, btn); return; }
      startInteraction(s, action); // reply/quote → Compose interaction mode
    });
    row.append(btn);
  }
  const why = [["reply", s.caps.reply], ["repost", s.caps.repost], ["quote", s.caps.quote]]
    .filter(([, c]) => !c.allowed && c.reason).map(([a, c]) => a + ": " + c.reason);
  if (why.length) row.append(el("div", { class: "act-why muted", text: why.join(" · ") }));
  return row;
}

// doRepost reposts at once: POST /v1/posts with repost and the source
// network's account, then the progress modal.
async function doRepost(s, btn) {
  const acct = accountFor(s.platform);
  if (!acct) { flash("No " + (PLAT_LABEL[s.platform] || s.platform) + " account is configured"); return; }
  btn.disabled = true;
  try {
    const post = await sendPost({ repost: { url: s.input }, targets: [{ account: acct.id }] }, "interact");
    setInflight(post.id);
    openProgressModal(post.id, post);
  } catch (e) {
    flash("Error: " + e.message);
  } finally {
    btn.disabled = false;
  }
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
