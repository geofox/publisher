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
};
ORDER.forEach(p => { state.ov[p] = defaultOv(p); });

export function effectiveText(p) { return state.ov[p].text != null ? state.ov[p].text : state.master; }

// focusedPlatform returns state.focus if still selected, else the first selected
// platform (in ORDER), else null.
export function focusedPlatform() {
  if (state.platforms.has(state.focus)) return state.focus;
  for (const p of ORDER) if (state.platforms.has(p)) return p;
  return null;
}

export function buildSpec() {
  const overrides = {};
  for (const p of state.platforms) {
    const ov = state.ov[p], o = {};
    if (ov.text != null) o.text = ov.text;
    if (p === "bluesky")  { o.langs = ov.langs.split(",").map(s => s.trim()).filter(Boolean); o.bluesky_reply = ov.reply; o.bluesky_disable_quotes = ov.disable_quotes; }
    if (p === "mastodon") { o.spoiler_text = ov.spoiler_text; o.sensitive = ov.sensitive; o.visibility = ov.visibility; o.language = ov.language; }
    if (p === "threads")  { o.topic_tag = ov.topic_tag; o.threads_reply_control = ov.reply_control; }
    if (p === "nostr")    { o.pow = ov.pow; o.content_warning = ov.content_warning; }
    overrides[p] = o;
  }
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
