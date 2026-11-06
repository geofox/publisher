"use strict";
import { el, $, gcount, graphemes, META } from "./common.js";
import { state, focusedPlatform, postedText } from "./state.js";
import { brandTile, icon } from "./brands.js";
import { requestPlan, planFor, lastPlanFor } from "./plan.js";

// threadBadgeStatic → the indigo "{N}-post thread" pill shown in a threaded
// preview header (non-interactive; the row/banner badges open the review sheet).
function threadBadgeStatic(count) {
  const b = el("span", { class: "thread-badge" });
  const ic = icon("thread", { size: 12, sw: 1.9 }); if (ic) b.append(ic);
  b.append(el("span", { class: "tb-txt", text: `${count}-post thread` }));
  return b;
}

// mediaMax mirrors Go thread.MaxImagesFor (per-platform attachment cap; 0 = none).
export function mediaMax(p) {
  if (p === "mastodon") return 4;
  return p === "bluesky" || p === "threads" ? 10 : 0;
}

// previewMedia returns the {url,alt} images the preview should show for platform p:
// your attached images, plus — on a fan-out platform — the original's media to be
// re-hosted, capped per platform (matching dispatch.capMedia: user first).
export function previewMedia(p) {
  const user = state.images.map((i) => ({ url: i.url, alt: i.alt, video: !!i.video, poster_url: i.poster_url }));
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

// mediaGridFrom builds the <div.pv-media> thumbnail grid for an explicit media
// list. >4 images add a count line — Bluesky renders those as a swipeable
// carousel, so the grid here is a contact sheet, not the final layout.
function mediaGridFrom(media) {
  if (!media.length) return null;
  const g = el("div", { class: "pv-media" });
  for (const im of media) {
    // Video: controls+playsinline are load-bearing — iOS paints no frame for
    // a bare non-playing element (without them the tile is an invisible box).
    // Set poster only when provided so no broken-image placeholder appears.
    let mediaEl;
    if (im.video) {
      const vAttrs = { src: im.url, preload: "metadata", muted: "muted", controls: "controls", playsinline: "playsinline" };
      if (im.poster_url) vAttrs.poster = im.poster_url;
      mediaEl = el("video", vAttrs);
    } else {
      mediaEl = el("img", { src: im.url, alt: im.alt || "" });
    }
    g.append(el("figure", {},
      mediaEl,
      el("figcaption", { class: im.alt ? "" : "muted", text: im.alt ? "alt ✓" : "no alt" })));
  }
  if (media.length > 4) {
    const wrapEl = el("div", {});
    wrapEl.append(el("div", { class: "muted", text: `${media.length} images` }), g);
    return wrapEl;
  }
  return g;
}

// mediaGrid keeps the per-platform signature used by the single-post card.
function mediaGrid(p) {
  return mediaGridFrom(previewMedia(p));
}

// linkCardEl renders the bluesky external-embed mock the server planned for
// this draft (thumb left, title/description/host right). The thumb loads
// straight from the external site — CSP img-src already allows https:.
function linkCardEl(card) {
  const a = el("a", { class: "pv-linkcard", href: card.uri, target: "_blank", rel: "noopener" });
  if (card.thumb_url) {
    a.append(el("img", { class: "pv-linkcard-thumb", src: card.thumb_url, alt: "",
      onerror: (e) => e.target.remove() }));
  }
  const txt = el("div", { class: "pv-linkcard-txt" });
  txt.append(el("div", { class: "pv-linkcard-title", text: card.title }));
  if (card.description) txt.append(el("div", { class: "pv-linkcard-desc", text: card.description }));
  let host = "";
  try { host = new URL(card.uri).hostname; } catch { /* leave empty */ }
  if (host) txt.append(el("div", { class: "pv-linkcard-host", text: host }));
  a.append(txt);
  return a;
}

// quotedCard renders the source post being quoted/replied-to (shown under the
// source-platform preview; on fan-out platforms the original is already inline in
// postedText so no card is needed).
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
  if (host.querySelector(".pv-quoted")) return;
  const it = state.interaction;
  if (it && p === it.platform) host.append(quotedCard());
}

// platformSwitch builds the <select> that changes which platform the preview
// shows. Shared by the single-post card and the threaded view so the operator
// can always switch the previewed platform — including when the draft renders
// as a multi-segment thread.
function platformSwitch(p) {
  const sel = el("select", { class: "pv-switch", onchange: e => { state.focus = e.target.value; renderPreview(); } });
  for (const q of [...state.platforms]) sel.append(el("option", { value: q, text: META[q].label }));
  sel.value = p;
  return sel;
}

// appendFitNotes renders the planned platform-fit conversions ("image 2
// → JPEG (over 1.9 MB)") under a preview card. Server-planned (PlanMediaFit),
// so the badge and the dispatch-time re-encode share one predicate.
// media (optional) is the previewMedia list for the platform, used to label
// video entries as "video" instead of "image N".
function appendFitNotes(host, platform, notes, media) {
  for (const n of notes || []) {
    const label = (media && media[n.ordinal] && media[n.ordinal].video)
      ? "video"
      : "image " + (n.ordinal + 1);
    host.appendChild(el("div", { class: "pv-fitnote",
      text: `${label} ${n.note} for ${META[platform].label}` }));
  }
}

// threadPreview renders platform's thread, as the server's plan has it
// (plan.js), into container. Returns false when the post is one post with
// nothing the plan adds (the single-post card stands). stale marks a plan
// for an earlier state of the composer, shown until the new one comes.
function threadPreview(container, platform, vt, stale) {
  if (!vt || !vt.items || !vt.items.length) return false;
  // Plan media indices are the attachments the body sent (state.js
  // postMedia); the preview's are all attachments (previewMedia).
  const sent = [];
  state.images.forEach((img, k) => {
    if (img.media_id && !(img.video && img.phase !== "ready")) sent.push(k);
  });
  // In the shape the renderer below reads: segments, per-segment media (as
  // previewMedia indices), the link card, fit notes.
  const pv = {
    count: vt.items.length,
    segments: vt.items.map(it => it.text),
    imgs: vt.items.map(it => (it.media || []).map(i => sent[i]).filter(i => i !== undefined)),
    card: vt.link_card && !state.interaction ? { segment: vt.link_card.item, uri: vt.link_card.url, title: vt.link_card.title,
      description: vt.link_card.description, thumb_url: vt.link_card.thumb_url || "" } : null,
    fit_notes: (vt.media_notes || []).map(n => ({ ordinal: sent[n.media], note: n.note })).filter(n => n.ordinal !== undefined),
    warnings: vt.warnings || [],
  };
  if (state.interaction && platform !== state.interaction.platform && pv.imgs.length) {
    // The re-hosted originals go after yours, on the first post.
    const n = state.images.length, extra = previewMedia(platform).length - n;
    for (let i = 0; i < extra; i++) pv.imgs[0].push(n + i);
  }
  const fitNotes = pv.fit_notes;
  const pvMedia = previewMedia(platform);
  if (pv.count < 2) {
    // A stale one-post plan has an old text: the composer's card stands.
    if (stale || (!pv.card && !fitNotes.length)) return false;
    // Single post with a link card or fit notes: re-render with the authoritative
    // (possibly URL-stripped) text and the planned card attached.
    container.innerHTML = "";
    _renderSinglePost(container, platform, { text: pv.segments[0], card: pv.card });
    // Append fit notes inside the platform card that _renderSinglePost created.
    appendFitNotes(container.querySelector(".pv-card") || container, platform, fitNotes, pvMedia);
    return true;
  }

  container.innerHTML = "";
  // The whole thread view is wrapped in a p-{platform} card so .pv-tok token
  // highlighting picks up the platform tint, and the left border is tinted too.
  const wrap = el("div", { class: "pv-card pv-thread p-" + platform + (stale ? " stale" : "") });
  const limit = vt.limit;
  // Header mirrors the single-post card: brand tile + platform switcher (so the
  // operator can change the previewed platform even while viewing a thread) +
  // the "{N}-post thread" badge.
  wrap.appendChild(el("div", { class: "pv-head" },
    brandTile(platform, { size: 22, r: 6 }),
    platformSwitch(platform),
    el("span", { class: "grow" }),
    threadBadgeStatic(pv.count),
  ));
  // Per-segment media from the server's plan: pv.imgs[i] lists the
  // attachments (previewMedia indices) on post i.
  const media = pvMedia;
  pv.segments.forEach((seg, i) => {
    const rail = el("div", { class: "pv-seg-rail" }, el("div", { class: "pv-seg-num", text: String(i + 1) }));
    if (i < pv.count - 1) rail.append(el("div", { class: "pv-seg-line" }));
    const body = el("div", { class: "pv-seg-body" }, ...highlight(seg));
    const segLen = vt.items[i].length;
    const main = el("div", { class: "pv-seg-main" }, body,
      el("div", { class: "pv-seg-n", style: "margin-top:4px", text: `${i + 1}/${pv.count}` }),
      el("div", { class: "pv-seg-count" + (limit && segLen > limit ? " over" : ""), text: limit ? `${segLen}/${limit}` : `${segLen} chars` }),
    );
    const segMedia = (pv.imgs[i] || []).map(ix => media[ix]).filter(Boolean);
    if (segMedia.length) {
      const g = mediaGridFrom(segMedia);
      if (g) main.append(g);
    }
    if (pv.card && pv.card.segment === i) main.append(linkCardEl(pv.card));
    wrap.appendChild(el("div", { class: "pv-seg" }, rail, main));
  });
  (pv.warnings || []).forEach((wmsg) => {
    wrap.appendChild(el("div", { class: "pv-warn", text: wmsg }));
  });
  appendFitNotes(wrap, platform, fitNotes, media);
  container.appendChild(wrap);
  return true;
}

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
// opts.text overrides the local draft text (server-stripped trailing URL) and
// opts.card appends the planned bluesky link card.
function _renderSinglePost(host, p, opts = {}) {
  const meta = META[p], ov = state.ov[p];
  const text = opts.text != null ? opts.text : postedText(p), n = gcount(text);
  const cwText = p === "mastodon" ? ov.spoiler_text : p === "nostr" ? ov.content_warning : "";

  const card = el("div", { class: "pv-card p-" + p });

  // header: brand tile + platform selector + state-colored count
  const sel = platformSwitch(p);
  const overCls = !meta.limit ? "inf" : n > meta.limit ? "over" : n > meta.limit * 0.9 ? "near" : "ok";
  card.append(el("div", { class: "pv-head" },
    brandTile(p, { size: 22, r: 6 }),
    sel,
    el("span", { class: "grow" }),
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
  const g = mediaGrid(p);
  if (g) card.append(g);

  if (opts.card) card.append(linkCardEl(opts.card));

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
// The single-post card renders at once from the composer; a thread (or a
// link card, or media notes) replaces it from the server's plan.
export function renderPreview() {
  // Every change to the composer comes through here: ask for a fresh plan.
  requestPlan();
  const host = $("#preview");
  if (!host) return;
  host.innerHTML = "";
  const p = focusedPlatform();
  if (!p) { host.append(el("p", { class: "muted", text: "no targets selected" })); return; }

  // Render the single-post preview immediately (synchronous, no flicker).
  _renderSinglePost(host, p);

  // The thread, link card and media notes come from the server's plan
  // (plan.js; a new one fires a "plan" event, which renders again). Until
  // the plan for the composer as it is arrives, the last one shows, dimmed,
  // rather than flicking back to a single post.
  const fresh = planFor(p);
  const vt = fresh || lastPlanFor(p);
  if (!vt || !threadPreview(host, p, vt, !fresh)) return;
  maybeAppendQuotedCard(host, p);
}
