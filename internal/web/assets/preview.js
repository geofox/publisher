"use strict";
import { el, $, gcount, graphemes, META } from "./common.js";
import { state, effectiveText, focusedPlatform, assembleReproduction } from "./state.js";

// previewText returns the text the preview should render/count for platform p:
// normal compose or the source platform's native action → your commentary;
// a fan-out platform in interaction mode → the assembled reproduction (what is
// actually posted there).
function previewText(p) {
  const it = state.interaction;
  if (!it) return effectiveText(p);
  // Match Go's interactText/Post: an empty per-platform override ("") falls back
  // to the master commentary (effectiveText treats "" as a real override).
  const ovText = state.ov[p] && state.ov[p].text;
  const commentary = ovText != null && ovText !== "" ? ovText : state.master;
  if (p === it.platform) return commentary; // source: native reply/quote, commentary only
  return assembleReproduction(commentary, it.sourcePreview, it.sourceURL); // fan-out reproduction
}

// mediaMax mirrors Go dispatch.mediaMax (per-platform attachment cap; 0 = none).
function mediaMax(p) {
  return p === "bluesky" || p === "mastodon" || p === "threads" ? 4 : 0;
}

// previewMedia returns the {url,alt} images the preview should show for platform p:
// your attached images, plus — on a fan-out platform — the original's media to be
// re-hosted, capped per platform (matching dispatch.capMedia: user first).
function previewMedia(p) {
  const user = state.images.map((i) => ({ url: i.url, alt: i.alt }));
  const it = state.interaction;
  if (!it || p === it.platform) return user;
  const src = ((it.sourcePreview && it.sourcePreview.media) || []).map((m) => ({ url: m.url, alt: m.alt || "" }));
  const max = mediaMax(p);
  const out = user.slice();
  for (const m of src) {
    if (max > 0 && out.length >= max) break;
    out.push(m);
  }
  return out;
}

// quotedCard renders the source post being quoted/replied-to (shown under the
// source-platform preview; on fan-out platforms the original is already inline in
// previewText so no card is needed).
function quotedCard() {
  const it = state.interaction;
  const sp = (it && it.sourcePreview) || {};
  const card = el("div", { class: "pv-quoted" });
  card.append(el("div", { class: "pv-quoted-author", text: it.sourceAuthor || it.platform }));
  if (sp.text) card.append(el("div", { class: "pv-quoted-text", text: sp.text }));
  if (sp.media && sp.media.length) {
    const g = el("div", { class: "pv-quoted-media" });
    for (const m of sp.media) g.append(el("img", { src: m.url, alt: m.alt || "" }));
    card.append(g);
  }
  const verb = it && it.action === "reply" ? "Replied-to" : "Quoted";
  let hint = verb + " post is attached — only your text counts toward the limit.";
  if (it && it.platform === "nostr") hint += " A nostr: mention links it.";
  card.append(el("div", { class: "pv-quoted-hint muted", text: hint }));
  return card;
}

// maybeAppendQuotedCard adds the source card after the platform preview when the
// focused platform is the interaction's source (native reply/quote embeds it).
function maybeAppendQuotedCard(host, p) {
  const it = state.interaction;
  if (it && p === it.platform) host.append(quotedCard());
}

// threadPreview fetches the per-platform split for the focused platform and
// renders the segment chain into `container`. Returns true if it rendered a
// multi-segment thread, false if the draft fits in one post (caller renders the
// normal single-post preview).
export async function threadPreview(container, text, platform, number) {
  if (!text.trim()) return false;
  let data;
  try {
    const resp = await fetch("/api/thread-preview", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ text, platforms: [platform], number }),
    });
    data = await resp.json();
  } catch {
    return false; // network hiccup → fall back to normal preview
  }
  const pv = (data.previews || []).find((p) => p.platform === platform);
  if (!pv || pv.count < 2) return false;

  container.innerHTML = "";
  const head = document.createElement("div");
  head.className = "pv-thread-head";
  head.textContent = `${pv.count} posts`;
  container.appendChild(head);
  pv.segments.forEach((seg, i) => {
    const card = document.createElement("div");
    card.className = "pv-seg";
    const n = document.createElement("span");
    n.className = "pv-seg-n";
    n.textContent = `${i + 1}/${pv.count}`;
    const body = document.createElement("div");
    body.className = "pv-seg-body";
    body.textContent = seg; // textContent => no HTML injection
    card.appendChild(n);
    card.appendChild(body);
    container.appendChild(card);
  });
  (pv.warnings || []).forEach((wmsg) => {
    const wd = document.createElement("div");
    wd.className = "pv-warn";
    wd.textContent = wmsg;
    container.appendChild(wd);
  });
  return true;
}

// _threadDebounce holds the pending debounce timer for thread-preview fetches.
let _threadDebounce = null;
// _threadSeq is incremented on every renderPreview() call so in-flight async
// responses that resolve after a newer render can detect they are stale.
let _threadSeq = 0;

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
  const segs = graphemes(text);
  if (segs.length <= limit) return { head: text, tail: "", over: 0 };
  return { head: segs.slice(0, limit).join(""), tail: segs.slice(limit).join(""), over: segs.length - limit };
}

// _renderSinglePost renders the existing single-post preview card into host.
function _renderSinglePost(host, p) {
  const meta = META[p], ov = state.ov[p];
  const text = previewText(p), n = gcount(text);
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

  // media grid — your attached images, plus (on a fan-out platform) the original's
  // re-hosted media, capped per platform exactly like dispatch does on send.
  const media = previewMedia(p);
  if (media.length) {
    const g = el("div", { class: "pv-media" });
    for (const im of media) {
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
  maybeAppendQuotedCard(host, p);
}

// renderPreview paints the focused platform's preview into #preview.
// For multi-segment threads it shows the thread chain (debounced network call);
// for single posts it renders the existing single-post card immediately.
export function renderPreview() {
  const host = $("#preview");
  if (!host) return;
  host.innerHTML = "";
  const p = focusedPlatform();
  // Increment the sequence counter on every call so any in-flight async response
  // that resolves after this render can detect it is stale and discard its write.
  const seq = ++_threadSeq;
  if (!p) { host.append(el("p", { class: "muted", text: "no targets selected" })); return; }

  // Render the single-post preview immediately (synchronous, no flicker).
  _renderSinglePost(host, p);

  // Debounce the thread-preview network call (~250 ms) so we don't fire a
  // request per keystroke. If the server says it's a multi-segment thread, we
  // swap the host content for the threaded view; otherwise the single-post
  // preview already rendered above stays in place.
  const text = previewText(p);
  const number = document.getElementById("threadnum")?.checked ?? true;
  // A draft can only become a thread if it has a manual `---` marker line or
  // exceeds the platform's limit. Otherwise the synchronous single-post card
  // above is already correct — skip the network round-trip so typing stays snappy
  // (the seq bump above discards any thread fetch still in flight from a longer
  // earlier state).
  const limit = META[p].limit;
  if (!/^[ \t]*---[ \t]*$/m.test(text) && (!limit || gcount(text) <= limit)) {
    clearTimeout(_threadDebounce);
    return;
  }
  clearTimeout(_threadDebounce);
  _threadDebounce = setTimeout(() => {
    _threadDebounce = null;
    threadPreview(host, text, p, number).then(rendered => {
      // Discard the response if a newer renderPreview() call owns the view now.
      if (seq !== _threadSeq) return;
      // If threadPreview returned false the single-post preview is still showing
      // — nothing to do. If it returned true it already replaced host's content.
      if (!rendered) {
        // Defensive: the synchronous single-post render at the top of this
        // renderPreview() call already shows the single post, so re-assert it to
        // ensure a prior thread view can never linger when the draft now fits.
        // _renderSinglePost re-appends the quoted card itself — no double-append.
        host.innerHTML = "";
        _renderSinglePost(host, p);
      } else {
        // threadPreview cleared host and rendered the thread chain, wiping the
        // quoted card the synchronous single-post render appended — re-append it
        // so the source card persists under the thread view.
        maybeAppendQuotedCard(host, p);
      }
    }).catch(() => { /* network error — single-post preview remains */ });
  }, 250);
}
