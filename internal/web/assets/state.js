"use strict";
import { ORDER } from "./common.js";
import { accountFor } from "./accounts.js";

export function defaultOv(p) {
  // Read state.userLanguages at call time so callers (initial module load,
  // post-/v1/instance init, startInteraction reset) all pick up the operator's
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
  // /v1/instance resolves on boot; defaultOv falls back to "en" until then.
  userLanguages: [],
  // ISO 639-1 codes the operator can translate INTO via DeepL — userLanguages
  // intersected with DeepL's supported targets, server-side. Empty when the
  // operator has no DEEPL_API_KEY configured; the history Translate button
  // is hidden in that case.
  translateTargets: [],
  // Drafts integration
  activeDraftId: null, // id of the draft currently loaded into Compose, or null
  draftETag: "",       // its version as last read or saved, for If-Match
  dirty: false,        // true when in-memory spec differs from last saved state
  anchors: {},         // image id → 0-based part index (keyed by stable image id)
};
ORDER.forEach(p => { state.ov[p] = defaultOv(p); });

export function masterParts() {
  // mirrors internal/thread.splitMarkers — split on lines that are solely "---"
  const blocks = []; let cur = [];
  for (const ln of state.master.replace(/\r\n/g, "\n").split("\n")) {
    if (ln.trim() === "---") { blocks.push(cur.join("\n").trim()); cur = []; }
    else cur.push(ln);
  }
  blocks.push(cur.join("\n").trim());
  return blocks.filter(Boolean);
}

export function effectiveText(p) { return state.ov[p].text != null ? state.ov[p].text : state.master; }

// The original's text and author as a link-quote copies them; these mirror Go
// dispatch.quotedText / quotedAuthor: controls and bidi characters dropped, a
// mention-starting @ followed by a word joiner, the text cut at 500
// characters (the author at 100).
const invisible = /[\u0000-\u0008\u000b-\u001f\u007f-\u009f\u202a-\u202e\u2066-\u2069]/gu;
function cut(chars, n) {
  return chars.length > n ? chars.slice(0, n).join("").trim() + "…" : chars.join("").trim();
}
export function quotedText(s) {
  const chars = [...s.trim().replace(invisible, "")];
  return cut(chars, 500).replace(/@(?=[\p{L}\p{N}_])/gu, "@\u2060");
}
function quotedAuthor(s) {
  return cut([...s.trim().replace(invisible, "").replace(/[\n\t]/g, "")], 100);
}

// assembleReproduction mirrors Go dispatch.assembleReproduction so a fan-out
// target's live preview matches what gets posted: commentary, an attributed copy
// of the original's text, then the source URL (blank-line separated). `sp` is the
// interaction sourcePreview; author attribution uses its author_handle.
export function assembleReproduction(commentary, sp, sourceURL) {
  const parts = [];
  const c = (commentary || "").trim();
  if (c) parts.push(c);
  sp = sp || {};
  const t = quotedText(sp.text || "");
  if (t) parts.push("— " + quotedAuthor(sp.author_handle || "") + ":\n" + t);
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

// ovFor returns a draft's override object for one platform (buildSpec).
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

// imageSpecs serializes state.images for the recovery snapshot. A fresh
// image not uploaded yet can't survive a reload (its bytes are only in
// memory); one in media storage becomes a reference with its media id.
export function imageSpecs() {
  return state.images.filter(i => !(i.video && i.phase !== "ready")).map((i, idx) => {
    const url = i.file ? (i.media_id && i.remote_url) : i.blossom_url;
    if (!url) {
      return { ordinal: idx, ref: "img_" + idx, alt: i.alt, id: i.id };
    }
    return {
      ordinal: idx, blossom_url: url, sha256: i.sha256,
      mime: i.mime, dim: i.dim, blurhash: i.blurhash, size_bytes: i.size_bytes,
      duration_secs: i.duration_secs || 0,
      poster_url: i.poster_url || "",
      alt: i.alt,
      id: i.id,
      media_id: i.media_id || "",
    };
  });
}

export function buildSpec() {
  const overrides = {};
  for (const p of state.platforms) overrides[p] = ovFor(p);
  const nParts = masterParts().length;
  const imgs = imageSpecs();
  const img_parts = state.images
    .filter(i => !(i.video && i.phase !== "ready"))
    .map(i => Math.min(state.anchors[i.id] || 0, Math.max(0, nParts - 1)));
  const spec = {
    master_text: state.master,
    platforms: [...state.platforms],
    delay_seconds: 0,
    overrides,
    images: imgs,
    img_parts,
    anchors: state.anchors,   // media placement, keyed by attachment id
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
    if (i._uploadAbort) i._uploadAbort.abort();
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

// ---------------------------------------------------------------------------
// POST /v1/posts bodies
// ---------------------------------------------------------------------------

function langList(v) {
  const str = Array.isArray(v) ? v.join(",") : (v || "");
  return str.split(",").map(s => s.trim()).filter(Boolean);
}

// v1Target is platform p's target: its account, the options its card sets and
// the content it overrides (text, content warning, sensitivity, languages).
// A repost carries nothing but the account.
export function v1Target(p, { repost = false } = {}) {
  const acct = accountFor(p);
  const t = { account: acct ? acct.id : "" };
  if (repost) return t;
  const ov = state.ov[p], options = {}, content = {};
  // The server refuses a blank override; the composer treats one as "use the
  // shared text".
  if (ov.text != null && ov.text.trim() !== "") content.text = ov.text;
  if (p === "bluesky") {
    options.reply_gate = ov.reply || "";
    options.disable_quotes = !!ov.disable_quotes;
    const langs = langList(ov.langs);
    if (langs.length) content.languages = langs;
  }
  if (p === "mastodon") {
    if (ov.visibility) options.visibility = ov.visibility;
    if (ov.spoiler_text) content.content_warning = ov.spoiler_text;
    if (ov.sensitive) content.sensitive = true;
    if (ov.language) content.languages = [ov.language];
  }
  if (p === "threads") {
    if (ov.topic_tag) options.topic_tag = ov.topic_tag;
    if (ov.reply_control) options.reply_control = ov.reply_control;
  }
  if (p === "nostr") {
    const pow = parseInt(ov.pow, 10);
    options.pow = Number.isFinite(pow) && pow > 0 ? pow : 0;
    if (ov.content_warning) content.content_warning = ov.content_warning;
  }
  if (Object.keys(options).length) t.options = options;
  if (Object.keys(content).length) t.content = content;
  return t;
}

// postMedia lists the attachments a post can carry now: in media storage
// (an image uploaded, a video processed), in the composer's order.
export function postMedia() {
  return state.images.filter(i => i.media_id && !(i.video && i.phase !== "ready"));
}

// mediaPart is the text part an attachment goes with (its placement chip),
// clamped to the parts there are.
export function mediaPart(img) {
  const n = masterParts().length;
  return Math.min(state.anchors[img.id] || 0, Math.max(0, n - 1));
}

// v1PostBody is the composer as a POST /v1/posts body: shared text and
// media, one target per selected platform, numbering, and the schedule or
// the reply/quote it makes.
export function v1PostBody() {
  const it = state.interaction;
  const media = postMedia().map(i => {
    const m = { id: i.media_id, alt: i.alt || "" };
    const part = it ? 0 : mediaPart(i);
    if (part > 0) m.part = part;
    return m;
  });
  const body = {
    content: { text: state.master, media },
    targets: ORDER.filter(p => state.platforms.has(p)).map(p => v1Target(p)),
    threading: { numbering: document.getElementById("threadnum")?.checked ?? true },
  };
  if (it) {
    body[it.action === "quote" ? "quote" : "reply_to"] = { url: it.sourceInput || it.sourceURL };
  } else {
    const sa = document.querySelector("#schedat")?.value;
    if (sa) body.schedule = { at: new Date(sa).toISOString() };
  }
  return body;
}
