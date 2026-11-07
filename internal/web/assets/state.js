"use strict";
import { typeOf, inOrder, byId } from "./accounts.js";
import { VERSION } from "./snapshot.js";
import { u16ToCp, cleanBreaks } from "./textpos.js";

// defaultOv is a new target's settings for an account of network type p:
// the network's, then the account's own defaults (Settings, Composing)
// when acct is given.
export function defaultOv(p, acct = null) {
  // Read state.userLanguages at call time so callers (initial module load,
  // post-/v1/instance init, startInteraction reset) all pick up the operator's
  // first configured language as the default. Falls back to "en" pre-fetch.
  const lang = (state.userLanguages && state.userLanguages[0]) || "en";
  // text: the account's own wording (null: the shared text). breaks: its
  // own breaks in the text it posts (null: the shared ones, or none with its
  // own text). mediaAt: with its own text, each attachment's position in it
  // (by attachment id). Positions are UTF-16 offsets.
  const o = { text: null, breaks: null, mediaAt: null };
  if (p === "bluesky")  { o.langs = lang; o.reply = ""; o.disable_quotes = false; }
  if (p === "mastodon") { o.spoiler_text = ""; o.sensitive = false; o.visibility = ""; o.language = lang; }
  if (p === "threads")  { o.topic_tag = ""; o.reply_control = ""; }
  if (p === "nostr")    { o.pow = 20; o.content_warning = ""; }
  if (acct && acct.defaults) withAccountDefaults(o, p, acct.defaults);
  return o;
}

// withAccountDefaults puts an account's defaults (content warning,
// languages, options) into its settings, as its card shows them. The
// composer sends what its cards hold, so a default applies by being there.
function withAccountDefaults(o, p, d) {
  const opt = d.options || {};
  const has = k => Object.prototype.hasOwnProperty.call(opt, k);
  const langs = Array.isArray(d.languages) ? d.languages.filter(Boolean) : [];
  const cw = typeof d.content_warning === "string" ? d.content_warning : "";
  if (p === "bluesky") {
    if (langs.length) o.langs = langs.join(",");
    if (has("reply_gate")) o.reply = String(opt.reply_gate || "");
    if (has("disable_quotes")) o.disable_quotes = !!opt.disable_quotes;
  }
  if (p === "mastodon") {
    if (cw) o.spoiler_text = cw;
    if (langs.length) o.language = langs[0];
    if (has("visibility")) o.visibility = String(opt.visibility || "");
  }
  if (p === "threads") {
    if (has("topic_tag")) o.topic_tag = String(opt.topic_tag || "");
    if (has("reply_control")) o.reply_control = String(opt.reply_control || "");
  }
  if (p === "nostr") {
    if (cw) o.content_warning = cw;
    if (has("pow") && Number.isFinite(Number(opt.pow))) o.pow = Number(opt.pow);
  }
}

export const state = {
  master: "",
  breaks: [],          // shared breaks: UTF-16 offsets into master, ascending
  tab: "shared",       // the thread editor's tab: "shared" or an account id
  targets: new Set(),  // the selected accounts (ids); setTargets changes it
  // Whether the selection was set (by hand, a draft, a reply…): once it
  // is, the accounts' defaults (main.js selectDefaults) no longer apply.
  chosen: false,
  ov: Object.create(null), // account id → its settings (ovOf; see byKey)
  images: [],
  focus: "",           // the account shown in the live preview
  // null = normal compose; else {action, account (the one that replies or
  // quotes), platform (its type), ref, sourcePreview, sourceURL,
  // sourceAuthor, sourceInput, caps, prevTargets}
  interaction: null,
  // Operator-configured ISO 639-1 codes (USER_LANGUAGES env). Empty until
  // /v1/instance resolves on boot; defaultOv falls back to "en" until then.
  userLanguages: [],
  // ISO 639-1 codes the operator can translate INTO via DeepL — userLanguages
  // intersected with DeepL's supported targets, server-side. Empty when the
  // operator has no DEEPL_API_KEY configured; the history Translate button
  // is hidden in that case.
  translateTargets: [],
  // The server has no media store (features.media false): no attaching.
  mediaOff: false,
  // The reverse proxy is the only gate (auth "proxy"): accounts come from
  // the server's environment only.
  proxyAuth: false,
  // Drafts integration
  activeDraftId: null, // id of the draft currently loaded into Compose, or null
  draftETag: "",       // its version as last read or saved, for If-Match
  dirty: false,        // true when in-memory spec differs from last saved state
  edits: 0,            // counts edits (markDirty), so a save knows if more came meanwhile
  anchors: {},         // image id → its position in master (UTF-16); it goes with the post holding it
};

// byKey is an empty object for values keyed by account id: no prototype,
// so no id ("__proto__", "constructor") can read or replace Object's.
export function byKey() { return Object.create(null); }

// ovOf is account id's settings, made from its network's defaults the first
// time they are asked for. Asked before the accounts were read (a reply
// started after a failed read), the network isn't known yet: its defaults
// (languages, proof of work…) are added once it is.
const untyped = new WeakSet();
export function ovOf(id) {
  const type = typeOf(id);
  let ov = state.ov[id];
  if (!ov) {
    ov = state.ov[id] = defaultOv(type, byId(id));
    if (!type) untyped.add(ov);
  } else if (type && untyped.has(ov)) {
    for (const [k, v] of Object.entries(defaultOv(type, byId(id)))) if (!(k in ov)) ov[k] = v;
    untyped.delete(ov);
  }
  return ov;
}

// selected is the selected accounts in the app's order.
export function selected() { return inOrder(state.targets); }

// setTargets selects exactly ids (account ids).
export function setTargets(ids) {
  state.targets = new Set(ids);
  state.chosen = true;
}

// layoutBreaks is the breaks account id's text is cut at: its own, else the
// shared ones when it posts the shared text, else none.
export function layoutBreaks(id) {
  const ov = ovOf(id);
  if (ov.breaks) return ov.breaks;
  return ov.text != null ? [] : state.breaks;
}

// mediaPosFor is attachment img's position in account id's text.
export function mediaPosFor(id, img) {
  const ov = ovOf(id);
  if (ov.text != null) return (ov.mediaAt && ov.mediaAt[img.id]) || 0;
  return state.anchors[img.id] || 0;
}

export function effectiveText(id) { const ov = ovOf(id); return ov.text != null ? ov.text : state.master; }

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

// postedText returns the full text account id will actually post — the single
// source of truth shared by the live preview AND the per-account counts so they
// always agree. Normal compose → your commentary (effectiveText). Interaction
// mode → the acting account posts your commentary (native reply/quote), while
// an account on another network posts the assembled reproduction. Mirrors Go
// interactText/Post: an empty own text ("") falls back to master
// (effectiveText treats "" as a real override, so we don't reuse it here).
export function postedText(id) {
  const it = state.interaction;
  if (!it) return effectiveText(id);
  const ovText = ovOf(id).text;
  const commentary = ovText != null && ovText !== "" ? ovText : state.master;
  if (id === it.account) return commentary; // the acting account: native reply/quote, commentary only
  return assembleReproduction(commentary, it.sourcePreview, it.sourceURL); // fan-out reproduction
}

// focusedTarget returns state.focus if still selected, else the first
// selected account (in order), else null.
export function focusedTarget() {
  if (state.targets.has(state.focus)) return state.focus;
  return selected()[0] || null;
}

// ovFor returns a snapshot's settings for one account (buildSpec).
function ovFor(id) {
  const p = typeOf(id);
  const ov = ovOf(id), o = {};
  if (ov.text != null) o.text = ov.text;
  if (ov.breaks) o.breaks = [...ov.breaks];
  if (ov.mediaAt) o.mediaAt = { ...ov.mediaAt };
  if (p === "bluesky") {
    // langs may be a string (editor input) or an array (a draft rehydrated
    // from a stored spec where buildSpec previously serialized it as []).
    const langsStr = Array.isArray(ov.langs) ? ov.langs.join(",") : (ov.langs || "");
    o.langs = langsStr.split(",").map((s) => s.trim()).filter(Boolean);
    // The keys of state.ov, so that loadDraft restores them as they are.
    o.reply = ov.reply;
    o.disable_quotes = ov.disable_quotes;
  }
  if (p === "mastodon") { o.spoiler_text = ov.spoiler_text; o.sensitive = ov.sensitive; o.visibility = ov.visibility; o.language = ov.language; }
  if (p === "threads")  { o.topic_tag = ov.topic_tag; o.reply_control = ov.reply_control; }
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
  const ids = selected();
  const overrides = {};
  for (const id of ids) overrides[id] = ovFor(id);
  const spec = {
    // 3: targets and settings by account id (snapshot.js reads older ones).
    v: VERSION,
    master_text: state.master,
    breaks: [...state.breaks],
    targets: ids,
    delay_seconds: 0,
    overrides,
    images: imageSpecs(),
    anchors: state.anchors,   // media positions, keyed by attachment id
    // mirror the preview's numbering toggle so the posted thread matches what was shown
    number: document.getElementById("threadnum")?.checked ?? true,
  };
  const sa = document.querySelector("#schedat")?.value;
  if (sa) spec.scheduled_at = new Date(sa).toISOString();
  if (state.activeDraftId) { spec.draft_id = state.activeDraftId; spec.etag = state.draftETag || ""; }
  // A reply or quote in progress: what the restore resolves again.
  const it = state.interaction;
  if (it) spec.interaction = { action: it.action, account: it.account, platform: it.platform, sourceInput: it.sourceInput, prevTargets: [...(it.prevTargets || [])] };
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

// v1Target is account id's target: the account, the options its card sets
// and the content it overrides (text, content warning, sensitivity,
// languages). A repost carries nothing but the account.
export function v1Target(id, { repost = false } = {}) {
  const t = { account: id };
  if (repost) return t;
  const p = typeOf(id);
  const ov = ovOf(id), options = {}, content = {};
  // The server refuses a blank override; the composer treats one as "use the
  // shared text".
  // In a reply or a quote only the shared breaks apply (to the native
  // target; copies are split automatically).
  const own = ov.text != null && ov.text.trim() !== "";
  if (own) content.text = ov.text;
  if (!state.interaction) {
    const text = own ? ov.text : state.master;
    const th = {};
    if (ov.breaks && (own || ov.text == null)) th.breaks = cpBreaks(text, ov.breaks);
    else if (own) th.breaks = [];
    if (own) {
      const at = postMedia().map(i => u16ToCp(text, Math.min((ov.mediaAt && ov.mediaAt[i.id]) || 0, text.length)));
      if (at.some(x => x > 0)) th.media_at = at;
    }
    // Absent breaks already mean the shared ones (or none with own text).
    if (th.media_at || (th.breaks && (th.breaks.length || !own))) t.thread = th;
  }
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

// cpBreaks is breaks in text as the API takes them: valid, in code points.
function cpBreaks(text, breaks) {
  return cleanBreaks(text, breaks).map(b => u16ToCp(text, b));
}

// v1PostBody is the composer as a POST /v1/posts body: shared text and
// media, one target per selected account, numbering, and the schedule or
// the reply/quote it makes.
export function v1PostBody() {
  const it = state.interaction;
  const text = state.master;
  const media = postMedia().map(i => {
    const m = { id: i.media_id, alt: i.alt || "" };
    const at = it ? 0 : u16ToCp(text, Math.min(state.anchors[i.id] || 0, text.length));
    if (at > 0) m.at = at;
    return m;
  });
  const content = { text, media };
  const breaks = cpBreaks(text, state.breaks);
  if (breaks.length) content.breaks = breaks;
  const body = {
    content,
    targets: selected().map(id => v1Target(id)),
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
