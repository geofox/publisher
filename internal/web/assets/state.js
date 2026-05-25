"use strict";
import { ORDER } from "./common.js";

export function defaultOv(p) {
  const o = { text: null };
  if (p === "bluesky")  { o.langs = "en"; o.reply = ""; o.disable_quotes = false; }
  if (p === "mastodon") { o.spoiler_text = ""; o.sensitive = false; o.visibility = ""; o.language = "en"; }
  if (p === "threads")  { o.topic_tag = ""; o.reply_control = ""; }
  if (p === "nostr")    { o.pow = 20; o.content_warning = ""; }
  return o;
}

export const state = {
  master: "",
  platforms: new Set(ORDER),
  ov: {},
  images: [],
  focus: "bluesky", // platform shown in the live preview
  interaction: null, // null = normal compose; else {action, platform, ref, sourcePreview, sourceURL, sourceAuthor, caps, force}
};
ORDER.forEach(p => { state.ov[p] = defaultOv(p); });

export function effectiveText(p) { return state.ov[p].text != null ? state.ov[p].text : state.master; }

// assembleReproduction mirrors Go dispatch.assembleReproduction so a fan-out
// target's live preview matches what gets posted: commentary, an attributed copy
// of the original's text, then the source URL (blank-line separated). `sp` is the
// interaction sourcePreview; author attribution uses its author_handle.
export function assembleReproduction(commentary, sp, sourceURL) {
  const parts = [];
  const c = (commentary || "").trim();
  if (c) parts.push(c);
  sp = sp || {};
  if ((sp.text || "").trim()) parts.push("— " + (sp.author_handle || "") + ":\n" + sp.text);
  if (sourceURL) parts.push(sourceURL);
  return parts.join("\n\n");
}

// focusedPlatform returns state.focus if still selected, else the first selected
// platform (in ORDER), else null.
export function focusedPlatform() {
  if (state.platforms.has(state.focus)) return state.focus;
  for (const p of ORDER) if (state.platforms.has(p)) return p;
  return null;
}

// ovFor returns the API override object for one platform (shared by buildSpec
// and buildInteractSpec).
function ovFor(p) {
  const ov = state.ov[p], o = {};
  if (ov.text != null) o.text = ov.text;
  if (p === "bluesky")  { o.langs = ov.langs.split(",").map((s) => s.trim()).filter(Boolean); o.bluesky_reply = ov.reply; o.bluesky_disable_quotes = ov.disable_quotes; }
  if (p === "mastodon") { o.spoiler_text = ov.spoiler_text; o.sensitive = ov.sensitive; o.visibility = ov.visibility; o.language = ov.language; }
  if (p === "threads")  { o.topic_tag = ov.topic_tag; o.threads_reply_control = ov.reply_control; }
  if (p === "nostr")    { o.pow = ov.pow; o.content_warning = ov.content_warning; }
  return o;
}

export function buildSpec() {
  const overrides = {};
  for (const p of state.platforms) overrides[p] = ovFor(p);
  const spec = {
    master_text: state.master,
    platforms: [...state.platforms],
    delay_seconds: 0,
    overrides,
    images: state.images.map(i => ({ alt: i.alt })),
    // mirror the preview's numbering toggle so the posted thread matches what was shown
    number: document.getElementById("threadnum")?.checked ?? true,
  };
  const sa = document.querySelector("#schedat")?.value;
  if (sa) spec.scheduled_at = new Date(sa).toISOString();
  return spec;
}

// buildInteractSpec assembles the /api/interact spec (reply/quote) from the
// interaction state: the source platform is the locked native target; the other
// selected platforms are fan-out reproductions.
export function buildInteractSpec() {
  const it = state.interaction;
  const fanout = [...state.platforms].filter((p) => p !== it.platform);
  const overrides = {};
  for (const p of state.platforms) overrides[p] = ovFor(p);
  return {
    action: it.action,
    platform: it.platform,
    ref: it.ref,
    source_url: it.sourceURL,
    source_author: it.sourceAuthor,
    source_preview: {
      author: it.sourceAuthor,
      text: (it.sourcePreview && it.sourcePreview.text) || "",
      media: ((it.sourcePreview && it.sourcePreview.media) || []).map((m) => ({ url: m.url, alt: m.alt || "" })),
    },
    text: state.master,
    overrides,
    fanout,
    number: document.getElementById("threadnum")?.checked ?? true,
    force: !!it.force,
    images: state.images.map((i) => ({ alt: i.alt })),
  };
}
