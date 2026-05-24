"use strict";
import { el, $, gcount, META } from "./common.js";
import { state, effectiveText, focusedPlatform } from "./state.js";

// escapeHTML-free: we build text nodes via el(); highlight() returns an array of
// nodes (plain text + accent spans) for hashtags, @mentions, and URLs.
const TOKEN = /(https?:\/\/[^\s]+|#[\w]+|@[\w.]+)/g;
function highlight(text) {
  const out = [];
  let last = 0, m;
  TOKEN.lastIndex = 0;
  while ((m = TOKEN.exec(text)) !== null) {
    if (m.index > last) out.push(document.createTextNode(text.slice(last, m.index)));
    out.push(el("span", { class: "pv-tok", text: m[0] }));
    last = m.index + m[0].length;
  }
  if (last < text.length) out.push(document.createTextNode(text.slice(last)));
  return out;
}

// truncate returns {head, tail, over}: head = graphemes within limit, tail =
// the over-limit remainder (rendered struck/dimmed), over = count over limit.
// limit 0 (Nostr) → no truncation.
function truncate(text, limit) {
  if (!limit) return { head: text, tail: "", over: 0 };
  let segs;
  try { segs = [...new Intl.Segmenter(undefined, { granularity: "grapheme" }).segment(text)].map(s => s.segment); }
  catch (_) { segs = [...text]; } // codepoint fallback, mirrors gcount()
  if (segs.length <= limit) return { head: text, tail: "", over: 0 };
  return { head: segs.slice(0, limit).join(""), tail: segs.slice(limit).join(""), over: segs.length - limit };
}

// renderPreview paints the focused platform's preview into #preview.
export function renderPreview() {
  const host = $("#preview");
  if (!host) return;
  host.innerHTML = "";
  const p = focusedPlatform();
  if (!p) { host.append(el("p", { class: "muted", text: "no targets selected" })); return; }

  const meta = META[p], ov = state.ov[p];
  const text = effectiveText(p), n = gcount(text);
  const cwText = p === "mastodon" ? ov.spoiler_text : p === "nostr" ? ov.content_warning : "";

  const card = el("div", { class: "pv-card p-" + p });

  // header: platform + selector + count
  const sel = el("select", { class: "pv-switch", onchange: e => { state.focus = e.target.value; renderPreview(); } });
  for (const q of [...state.platforms]) sel.append(el("option", { value: q, text: META[q].label }));
  sel.value = p;
  const overCls = !meta.limit ? "ok" : n > meta.limit ? "bad" : n > meta.limit * 0.9 ? "warn" : "ok";
  card.append(el("div", { class: "pv-head" },
    sel,
    el("span", { class: "pv-count " + overCls, text: meta.limit ? `${n}/${meta.limit}${n > meta.limit ? ` · +${n - meta.limit} over` : " ✓"}` : "∞" }),
  ));

  // optional content-warning fold
  if (cwText) {
    const body = el("div", { class: "pv-text", hidden: true });
    const reveal = el("button", { class: "pv-cw", type: "button", text: `⚠ CW: ${cwText} — show`,
      onclick: () => { body.hidden = !body.hidden; reveal.textContent = (body.hidden ? "⚠ CW: " + cwText + " — show" : "⚠ CW: " + cwText + " — hide"); } });
    const t = truncate(text, meta.limit);
    body.append(...highlight(t.head));
    if (t.tail) body.append(el("span", { class: "pv-over", text: t.tail }));
    card.append(reveal, body);
  } else {
    const t = truncate(text, meta.limit);
    const body = el("div", { class: "pv-text" });
    body.append(...(text ? highlight(t.head) : [el("span", { class: "muted", text: "—" })]));
    if (t.tail) body.append(el("span", { class: "pv-over", text: t.tail }));
    card.append(body);
  }

  // media grid
  if (state.images.length) {
    const g = el("div", { class: "pv-media" });
    for (const im of state.images) {
      g.append(el("figure", {},
        el("img", { src: im.url, alt: im.alt || "" }),
        el("figcaption", { class: im.alt ? "" : "muted", text: im.alt ? "alt ✓" : "no alt" })));
    }
    card.append(g);
  }

  // settings line (platform-specific)
  const bits = [];
  if (p === "bluesky")  { bits.push(`replies: ${ov.reply || "anyone"}`, `quotes: ${ov.disable_quotes ? "off" : "on"}`); }
  if (p === "mastodon") { bits.push(`visibility: ${ov.visibility || "default"}`, ov.sensitive ? "sensitive" : null); }
  if (p === "threads")  { bits.push(`replies: ${ov.reply_control || "anyone"}`, ov.topic_tag ? `#${ov.topic_tag}` : null); }
  if (p === "nostr")    { bits.push(`PoW: ${ov.pow || 0}`); }
  const line = bits.filter(Boolean).join(" · ");
  if (line) card.append(el("div", { class: "pv-settings", text: line }));

  host.append(card);
}
