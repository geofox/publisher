"use strict";
import { ORDER } from "./common.js";

export function defaultOv(p) {
  // Read state.userLanguages at call time so callers (initial module load,
  // post-/api/config init, startInteraction reset) all pick up the operator's
  // first configured language as the default. Falls back to "en" pre-fetch.
  const lang = (state.userLanguages && state.userLanguages[0]) || "en";
  const o = { text: null };
  if (p === "bluesky")  { o.langs = lang; o.reply = ""; o.disable_quotes = false; }
  if (p === "mastodon") { o.spoiler_text = ""; o.sensitive = false; o.visibility = ""; o.language = lang; }
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
  // Operator-configured ISO 639-1 codes (USER_LANGUAGES env). Empty until
  // /api/config resolves on boot; defaultOv falls back to "en" until then.
  userLanguages: [],
  // ISO 639-1 codes the operator can translate INTO via DeepL — userLanguages
  // intersected with DeepL's supported targets, server-side. Empty when the
  // operator has no DEEPL_API_KEY configured; the history Translate button
  // is hidden in that case.
  translateTargets: [],
  // Drafts integration
  activeDraftId: null, // id of the draft currently loaded into Compose, or null
  dirty: false,        // true when in-memory spec differs from last saved state
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

// postedText returns the full text platform p will actually post — the single
// source of truth shared by the live preview AND the per-platform counts so they
// always agree. Normal compose → your commentary (effectiveText). Interaction
// mode → the source platform posts your commentary (native reply/quote), while a
// fan-out platform posts the assembled reproduction. Mirrors Go interactText/Post:
// an empty per-platform override ("") falls back to master (effectiveText treats
// "" as a real override, so we don't reuse it here).
export function postedText(p) {
  const it = state.interaction;
  if (!it) return effectiveText(p);
  const ovText = state.ov[p] && state.ov[p].text;
  const commentary = ovText != null && ovText !== "" ? ovText : state.master;
  if (p === it.platform) return commentary; // source: native reply/quote, commentary only
  return assembleReproduction(commentary, it.sourcePreview, it.sourceURL); // fan-out reproduction
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
  if (p === "bluesky") {
    // langs may be a string (editor input) or an array (a draft rehydrated
    // from a stored spec where buildSpec previously serialized it as []).
    const langsStr = Array.isArray(ov.langs) ? ov.langs.join(",") : (ov.langs || "");
    o.langs = langsStr.split(",").map((s) => s.trim()).filter(Boolean);
    o.bluesky_reply = ov.reply;
    o.bluesky_disable_quotes = ov.disable_quotes;
  }
  if (p === "mastodon") { o.spoiler_text = ov.spoiler_text; o.sensitive = ov.sensitive; o.visibility = ov.visibility; o.language = ov.language; }
  if (p === "threads")  { o.topic_tag = ov.topic_tag; o.threads_reply_control = ov.reply_control; }
  if (p === "nostr")    { o.pow = ov.pow; o.content_warning = ov.content_warning; }
  return o;
}

// imageSpecs serializes state.images for a publish/interact spec. A fresh image
// (has a File) becomes a `ref` entry whose bytes ride as a multipart file; an
// already-uploaded one (a restored/loaded draft) becomes a `blossom_url`
// reference the server re-fetches. Shared by buildSpec and buildInteractSpec so
// both paths carry references identically.
export function imageSpecs() {
  return state.images.filter(i => !(i.video && i.phase !== "ready")).map((i, idx) => {
    if (i.file) {
      return { ordinal: idx, ref: "img_" + idx, alt: i.alt };
    }
    return {
      ordinal: idx, blossom_url: i.blossom_url, sha256: i.sha256,
      mime: i.mime, dim: i.dim, blurhash: i.blurhash, size_bytes: i.size_bytes,
      duration_secs: i.duration_secs || 0,
      alt: i.alt,
    };
  });
}

export function buildSpec() {
  const overrides = {};
  for (const p of state.platforms) overrides[p] = ovFor(p);
  const images = imageSpecs();
  const spec = {
    master_text: state.master,
    platforms: [...state.platforms],
    delay_seconds: 0,
    overrides,
    images,
    // mirror the preview's numbering toggle so the posted thread matches what was shown
    number: document.getElementById("threadnum")?.checked ?? true,
  };
  const sa = document.querySelector("#schedat")?.value;
  if (sa) spec.scheduled_at = new Date(sa).toISOString();
  if (state.activeDraftId) spec.draft_id = state.activeDraftId;
  return spec;
}

// imagesGen increments whenever state.images is cleared or wholesale
// replaced; async attach flows capture it before awaiting and discard their
// result if the composer moved on underneath them.
export let imagesGen = 0;

// clearImages aborts in-flight compresses, revokes preview URLs, empties the
// array and bumps the generation — the one safe way to discard attachments.
export function clearImages() {
  for (const i of state.images) {
    if (i._compressAbort) i._compressAbort.abort();
    if (i._xhr) i._xhr.abort();
    if (i._jobTimer) clearInterval(i._jobTimer);
    if (i.url) URL.revokeObjectURL(i.url);
  }
  state.images = [];
  imagesGen++;
}

export function bumpImagesGen() { imagesGen++; }

// In-flight post id: stashed on submit so a refresh can re-attach to the live
// progress stream; cleared when the post reaches a terminal state or the modal
// is closed.
const INFLIGHT_KEY = "inflight_post_v1";
export function setInflight(id) { try { localStorage.setItem(INFLIGHT_KEY, id); } catch {} }
export function getInflight() { try { return localStorage.getItem(INFLIGHT_KEY) || ""; } catch { return ""; } }
export function clearInflight() { try { localStorage.removeItem(INFLIGHT_KEY); } catch {} }

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
    images: imageSpecs(),
  };
}
