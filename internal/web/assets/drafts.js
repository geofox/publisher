"use strict";
import { state, v1PostBody, clearImages, setTargets } from "./state.js";
import { cpIndex } from "./textpos.js";
import { loadDraft, markDirty, mediaReady, newKey, resetComposer, rerenderComposer } from "./compose.js";
import { loadRecovery, clearRecovery, snapshot, setRecoveryPending } from "./drafts_recovery.js";
import { flash, el, v1, problemError, confirmModal, pressable } from "./common.js";
import { byId, typeOf, loadAccounts, defaultTargets, networkName } from "./accounts.js";
import { fromV1 } from "./interact.js";
import { upgrade, VERSION } from "./snapshot.js";

// Drafts live on the server (/v1/drafts). A draft holds a POST /v1/posts
// body; the composer turns itself into one to save (v1PostBody) and reads
// one back (specFromDraft). Each save sends the version it last read
// (If-Match), so a draft changed in another tab is not overwritten
// unknowingly.

const $ = (sel, root = document) => root.querySelector(sel);

function closeMobileSidebar() {
  const sb = document.getElementById("drafts-sidebar");
  const toggle = document.getElementById("drafts-toggle");
  if (sb && sb.classList.contains("open")) {
    sb.classList.remove("open");
    if (toggle) toggle.setAttribute("aria-expanded", "false");
  }
}

let activeFilters = { q: "", tags: new Set() };
let drafts = []; // last loaded list
let listSeq = 0;

function relativeTime(iso) {
  const d = new Date(iso);
  const s = Math.floor((Date.now() - d.getTime()) / 1000);
  if (s < 60) return s + "s";
  if (s < 3600) return Math.floor(s / 60) + "m";
  if (s < 86400) return Math.floor(s / 3600) + "h";
  return Math.floor(s / 86400) + "d";
}

// draftCall is a /v1/drafts request that also returns the ETag.
async function draftCall(path, { method = "GET", body, ifMatch } = {}) {
  const headers = {};
  if (body !== undefined) headers["Content-Type"] = "application/json";
  if (ifMatch) headers["If-Match"] = ifMatch;
  const r = await fetch(path, {
    method, credentials: "same-origin", headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const data = r.status === 204 ? null : await r.json().catch(() => null);
  if (!r.ok) {
    const err = problemError(r.status, data);
    err.etag = r.headers.get("ETag") || "";
    throw err;
  }
  return { data, etag: r.headers.get("ETag") || "" };
}

const draftPath = id => "/v1/drafts/" + encodeURIComponent(id);

export async function loadDraftList() {
  const qs = new URLSearchParams();
  if (activeFilters.q) qs.set("q", activeFilters.q);
  for (const t of activeFilters.tags) qs.append("tag", t);
  qs.set("limit", "50");
  const seq = ++listSeq;
  try {
    const page = await v1("/v1/drafts?" + qs.toString());
    if (seq !== listSeq) return; // a newer search answered first
    drafts = (page && page.data) || [];
    renderList();
    renderTagFilters();
  } catch (e) {
    console.warn("loadDraftList failed:", e);
  }
}

function renderList() {
  const list = $("#draft-list");
  if (!list) return;
  list.replaceChildren();
  if (!drafts || drafts.length === 0) {
    list.append(el("div", { class: "muted", style: "padding:8px", text: "No drafts yet." }));
    refreshActiveControls();
    return;
  }
  for (const d of drafts) {
    const row = el("div", { class: "draft-row" + (state.activeDraftId === d.id ? " active" : "") },
      el("div", { class: "title", text: d.title || "(untitled)" }),
      el("div", { class: "preview", text: d.preview || "" }),
      el("div", { class: "meta" },
        el("span", { text: (d.tags || []).map(t => "#" + t).join(" ") }),
        el("span", { class: "age", text: relativeTime(d.updated_at) })));
    row.dataset.id = d.id;
    row.addEventListener("click", () => openDraft(d.id));
    pressable(row);
    list.append(row);
  }
  refreshActiveControls();
}

function renderTagFilters() {
  const box = $("#draft-tags");
  if (!box) return;
  const all = new Set(activeFilters.tags);
  for (const d of drafts || []) for (const t of d.tags || []) all.add(t);
  box.replaceChildren();
  for (const t of all) {
    const chip = pressable(el("span", { class: "tag-chip" + (activeFilters.tags.has(t) ? " active" : ""), text: "#" + t,
      "aria-pressed": activeFilters.tags.has(t) ? "true" : "false" }));
    chip.addEventListener("click", () => {
      if (activeFilters.tags.has(t)) activeFilters.tags.delete(t);
      else activeFilters.tags.add(t);
      loadDraftList();
    });
    box.append(chip);
  }
}

// ovFromTarget reads a target back into its account's settings (p: the
// account's network): the reverse of state.js v1Target.
// u16 turns the API's code point positions in text into the composer's
// UTF-16 offsets.
function u16(text, positions) {
  const ix = cpIndex(text);
  return positions.filter(Number.isInteger).map(cp => ix[Math.max(0, Math.min(cp, ix.length - 1))]);
}

function ovFromTarget(p, t, shared) {
  const o = {}, opts = t.options || {}, c = t.content || {};
  const langs = Array.isArray(c.languages) ? c.languages : [];
  o.text = typeof c.text === "string" ? c.text : null;
  const th = t.thread || {};
  const text = o.text != null ? o.text : shared;
  if (Array.isArray(th.breaks)) o.breaks = u16(text, th.breaks);
  if (p === "bluesky") {
    o.reply = opts.reply_gate || "";
    o.disable_quotes = !!opts.disable_quotes;
    o.langs = langs.join(",");
  }
  if (p === "mastodon") {
    o.visibility = opts.visibility || "";
    o.spoiler_text = c.content_warning || "";
    o.sensitive = !!c.sensitive;
    o.language = langs[0] || "";
  }
  if (p === "threads") {
    o.topic_tag = opts.topic_tag || "";
    o.reply_control = opts.reply_control || "";
  }
  if (p === "nostr") {
    if (opts.pow != null) o.pow = opts.pow;
    o.content_warning = c.content_warning || "";
  }
  return o;
}

// sourceInteraction resolves the post a draft replies to or quotes, for the
// composer's interaction mode; null (with a note) when it can't be read.
// The draft's account on the source's network acts.
async function sourceInteraction(post, targets) {
  const action = post.quote ? "quote" : post.reply_to ? "reply" : "";
  if (!action) return { interaction: null };
  return resolveInteraction(action, (post.quote || post.reply_to).url, [], "", targets);
}

// resolve asks POST /v1/resolve about url, looked up by account (one on
// the post's network) or, without one, by the server's pick.
function resolve(url, account) {
  return v1("/v1/resolve", { method: "POST", body: account ? { url, account } : { url } });
}

// resolveInteraction reads the post at url again for a reply or quote.
// actor is the account that acted before (a snapshot's), else the first of
// candidates on the post's network acts, else the server's pick. prev is
// the selection to return to afterwards.
async function resolveInteraction(action, url, prev, actor = "", candidates = []) {
  try {
    let r;
    try {
      r = await resolve(url, actor);
    } catch (e) {
      // That account can't look it up any more (turned off, say): the
      // server picks, or another of the candidates below.
      if (!actor || !(e.status >= 400 && e.status < 500)) throw e;
      candidates = candidates.filter(id => id !== actor);
      actor = "";
      r = await resolve(url, "");
    }
    if (!actor) {
      const mine = candidates.find(id => typeOf(id) === r.platform);
      if (mine && mine !== r.account) r = await resolve(url, mine);
    }
    const src = fromV1(r);
    if (!src.account) throw new Error("no " + networkName(src.platform) + " account is connected");
    return {
      interaction: {
        action, account: src.account, platform: src.platform,
        sourcePreview: src.preview, sourceURL: src.preview.web_url, sourceAuthor: src.preview.author_handle,
        sourceInput: url, caps: src.caps,
        prevTargets: new Set(prev && prev.length ? prev : defaultTargets()),
      },
    };
  } catch (e) {
    return { interaction: null, error: e.message, lost: "The post this draft " + (action === "quote" ? "quotes" : "replies to") +
      " couldn't be loaded (" + e.message + "): it opens as a new post" };
  }
}

// specFromDraft turns a /v1 draft into what the composer's loadDraft reads
// (a version-3 snapshot, snapshot.js): every account it names that is still
// here, each with its own settings.
async function specFromDraft(d, etag) {
  const accountsRead = await loadAccounts().then(() => true, () => false);
  const post = d.post || {}, c = post.content || {};
  const targets = [], overrides = {}, ownMedia = {};
  let skipped = 0;
  for (const t of post.targets || []) {
    const a = byId(t.account);
    if (!a || targets.includes(a.id)) { skipped++; continue; }
    targets.push(a.id);
    overrides[a.id] = ovFromTarget(a.type, t, typeof c.text === "string" ? c.text : "");
    if (Array.isArray((t.thread || {}).media_at)) ownMedia[a.id] = t.thread.media_at;
  }
  const objects = new Map((d.media || []).map(m => [m.id, m]));
  const media = [], anchors = {};
  let missing = 0;
  const sharedText = typeof c.text === "string" ? c.text : "";
  (c.media || []).forEach((cm, k) => {
    const m = objects.get(cm.id);
    if (!m) { missing++; return; }
    const id = newKey();
    media.push({
      client_id: id, media_id: m.id, blossom_url: m.url, mime: m.mime, dim: m.dim || "",
      blurhash: m.blurhash || "", size_bytes: m.size_bytes || 0,
      alt: typeof cm.alt === "string" ? cm.alt : (m.alt || ""),
      duration_secs: m.duration_secs || 0, poster_url: m.poster_url || "",
    });
    if (Number.isInteger(cm.at) && cm.at > 0) anchors[id] = u16(sharedText, [cm.at])[0];
    for (const [p, at] of Object.entries(ownMedia)) {
      const ov = overrides[p];
      if (ov.text == null || !Number.isInteger(at[k])) continue;
      ov.mediaAt = ov.mediaAt || {};
      ov.mediaAt[id] = u16(ov.text, [at[k]])[0];
    }
  });
  const { interaction, lost } = await sourceInteraction(post, targets);
  const notes = [];
  if (!accountsRead && (post.targets || []).length) {
    notes.push("The accounts couldn't be read: check where this goes before saving or posting");
  } else if (skipped) notes.push(skipped + " of the draft's accounts are gone and were left out");
  if (missing) notes.push(missing + " attachment(s) were deleted and left out");
  if (lost) notes.push(lost);
  return {
    id: d.id, etag,
    v: VERSION,
    master_text: sharedText,
    breaks: Array.isArray(c.breaks) ? u16(sharedText, c.breaks) : [],
    targets: interaction ? [interaction.account, ...targets.filter(id => id !== interaction.account)] : targets,
    overrides, media, anchors, interaction,
    // The accounts it named that are gone (loadDraft then selects none of
    // the defaults in their place).
    dropped: accountsRead ? (post.targets || []).map(t => t.account).filter(id => !byId(id)) : [],
    number: post.threading ? !!post.threading.numbering : true,
    scheduled_at: post.schedule && post.schedule.at,
    notice: notes.length ? notes.join(" · ") : "",
  };
}

// showDraft loads a /v1 draft into the composer. The spec is built first
// (it may resolve a source post); the composer changes only once it is
// ready, and only if still wanted (current: a newer open didn't start).
async function showDraft(d, etag, notice, current = () => true) {
  const spec = await specFromDraft(d, etag);
  if (!current()) return;
  // What was in Compose was confirmed away (or saved): replace it as is.
  state.dirty = false;
  state.master = "";
  clearImages();
  state.interaction = null;
  if (notice && !spec.notice) spec.notice = notice;
  spec.onLoaded = () => {
    renderTagChips(d.tags || []);
    clearRecovery();
    renderList();
    closeMobileSidebar();
    refreshActiveControls();
    setStatus("", "");
  };
  // A note to read must outlast the usual "Draft loaded" flash.
  loadDraft(spec);
}

let openSeq = 0;

async function openDraft(id) {
  if (state.dirty && !confirm("Discard unsaved changes and load this draft?")) return;
  const seq = ++openSeq;
  try {
    const { data, etag } = await draftCall(draftPath(id));
    await showDraft(data, etag, "", () => seq === openSeq);
  } catch (e) {
    if (seq !== openSeq) return;
    if (e.status === 404) { flash("That draft is gone"); loadDraftList(); return; }
    alert("Failed to load draft: " + e.message);
  }
}

function setStatus(kind, text) {
  const node = document.getElementById("draft-status");
  if (!node) return;
  node.className = "draft-status " + (kind || "");
  node.textContent = text || "";
}

// draftBody is the composer as a draft: its post as it stands (targets
// without an account left out), and its tags.
function draftBody() {
  const post = v1PostBody();
  post.targets = post.targets.filter(t => t.account);
  return { tags: currentTagChips(), post };
}

let saving = null;

export function saveActiveDraft(opts) {
  if (!saving) saving = doSave(opts).finally(() => { saving = null; });
  return saving;
}

async function doSave({ overwrite = false } = {}) {
  if (state.images.some(i => i.video && i.phase !== "ready")) {
    flash(state.images.some(i => i.video && i.phase === "failed")
      ? "The video failed — retry or remove it first"
      : "Wait for the video to finish processing before saving");
    return;
  }
  const save = document.getElementById("draft-save");
  if (save) save.disabled = true;
  setStatus("saving", "saving…");
  try {
    if (!(await mediaReady())) { setStatus("error", "not saved: an attachment isn't uploaded"); return; }
    const body = draftBody();
    const edits = state.edits; // what this save holds
    let res;
    if (state.activeDraftId) {
      try {
        res = await draftCall(draftPath(state.activeDraftId), {
          method: "PATCH", body, ifMatch: overwrite ? "*" : (state.draftETag || "*"),
        });
      } catch (e) {
        if (e.status === 404) {
          state.activeDraftId = null; // deleted elsewhere: save it anew
        } else if (e.status === 412) {
          setStatus("error", "changed in another tab");
          conflictModal();
          return;
        } else throw e;
      }
    }
    if (!res) res = await draftCall("/v1/drafts", { method: "POST", body });
    state.activeDraftId = res.data.id;
    state.draftETag = res.etag;
    if (state.edits === edits) {
      state.dirty = false;
      setStatus("saved", "saved just now");
    } else {
      setStatus("dirty", "unsaved changes"); // edited while saving
    }
    snapshot(); // clears recovery, or keeps what was edited while saving
    loadDraftList();
    refreshActiveControls();
  } catch (e) {
    setStatus("error", "save failed — retry");
    flash("Draft not saved: " + e.message);
    console.warn("saveActiveDraft:", e);
  } finally {
    if (save) save.disabled = false;
  }
}

// conflictModal: the draft was saved elsewhere since this tab read it.
function conflictModal() {
  confirmModal({
    title: "This draft changed in another tab",
    body: "It was saved somewhere else after you opened it. Overwrite it with what's here, or cancel and open it from the list to see the other version (your edits here would be lost).",
    confirmText: "Overwrite",
    onConfirm: async () => { await saveActiveDraft({ overwrite: true }); return true; },
  });
}

export function newDraft() {
  if (state.dirty) {
    const choice = confirm("You have unsaved changes. OK to discard and start a new draft? (Cancel to keep editing.)");
    if (!choice) return;
  }
  clearComposer();
}

// clearComposer empties Compose for a new draft: text, media, settings,
// schedule, numbering, targets and tags.
function clearComposer() {
  openSeq++; // a draft still opening must not land afterwards
  clearRecovery(); // what was thrown away (or deleted) must not come back
  state.activeDraftId = null;
  state.draftETag = "";
  state.dirty = false;
  state.anchors = {};
  setTargets(defaultTargets());
  resetComposer();
  const num = document.getElementById("threadnum"); if (num) num.checked = true;
  rerenderComposer();
  renderTagChips([]);
  setStatus("", "");
  loadDraftList();
  refreshActiveControls();
}

function currentTagChips() {
  return Array.from(document.querySelectorAll("#draft-tags-input .tag-chip"))
    .map(node => node.dataset.tag);
}

function renderTagChips(tags) {
  const host = document.querySelector("#draft-tags-input .tag-chips");
  if (!host) return;
  host.replaceChildren();
  for (const t of tags) {
    const chip = pressable(el("span", { class: "tag-chip", text: "#" + t + " ✕", "aria-label": "Remove tag " + t }));
    chip.dataset.tag = t;
    chip.addEventListener("click", () => {
      const kbd = document.activeElement === chip;
      chip.remove();
      markDirty();
      if (kbd) document.querySelector("#draft-tags-input input")?.focus(); // not lost to <body>
    });
    host.append(chip);
  }
}

function installTagsInput() {
  const input = document.querySelector("#draft-tags-input input");
  if (!input) return;
  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter" || e.key === ",") {
      e.preventDefault();
      const raw = input.value.trim();
      if (!raw) return;
      const tags = new Set(currentTagChips());
      tags.add(raw.replace(/^#+/, "").toLowerCase().slice(0, 32));
      renderTagChips(Array.from(tags));
      input.value = "";
      markDirty();
    }
  });
}

function maybeShowRecoveryBanner() {
  const banner = document.getElementById("draft-recovery-banner");
  if (!banner) return;
  if (state.activeDraftId) { banner.hidden = true; return; }
  const recovered = loadRecovery();
  if (!recovered) { banner.hidden = true; return; }
  banner.hidden = false;
  setRecoveryPending(true);
  const r = document.getElementById("draft-recovery-restore");
  const d = document.getElementById("draft-recovery-discard");
  if (r) r.addEventListener("click", async () => {
    if (r.disabled) return;
    r.disabled = true; // resolving a reply's post takes a moment: one restore
    try { await restore(); } finally { r.disabled = false; }
  });
  const restore = async () => {
    // The snapshot names accounts (an older one, networks): it needs them
    // read, which a blip at start may not have done.
    let list;
    try { list = await loadAccounts(); } catch (e) {
      flash("Not restored yet: your accounts couldn't be read (" + e.message + "): try again", 8000);
      return;
    }
    const snap = upgrade(recovered, list);
    const all = snap.images || [];
    const refs = all.filter(i => i.blossom_url);
    // A reply or quote in progress stays one: its post is read again, by
    // the account that was acting. If it can't be, restoring would make it
    // a standalone post: don't.
    let interaction = null;
    const it = snap.interaction;
    if (it && it.sourceInput && (it.action === "reply" || it.action === "quote")) {
      const res = await resolveInteraction(it.action, it.sourceInput, it.prevTargets, it.account, snap.targets);
      if (!res.interaction) {
        flash("Not restored: the post this " + it.action + " answers couldn't be read (" + (res.error || "try again") + ")", 8000);
        return;
      }
      interaction = res.interaction;
    }
    const left = snap.dropped.length;
    // Reference attachments (uploaded images, ready videos) hydrate like a
    // loaded draft; fresh local-File images cannot survive localStorage.
    // Edits to an open draft go back into it (its etag finds it changed).
    loadDraft({ ...snap, interaction, id: snap.draft_id || null, etag: snap.etag || "", media: refs,
      notice: left ? left + (left === 1 ? " of its accounts isn't" : " of its accounts aren't") + " available here and " + (left === 1 ? "was" : "were") + " left out" : "",
      // Once it has loaded (after "Replace?" when Compose wasn't empty):
      // the restored edits are unsaved.
      onLoaded: () => {
        renderTagChips(Array.isArray(snap.tags) ? snap.tags : []);
        setRecoveryPending(false);
        markDirty();
        banner.hidden = true;
        renderList(); refreshActiveControls();
        if (all.length > refs.length) {
          flash((all.length - refs.length) + " attachment(s) couldn't be restored — re-attach them");
        }
      } });
  };
  if (d) d.addEventListener("click", () => {
    setRecoveryPending(false);
    clearRecovery();
    banner.hidden = true;
  });
}

export function installDraftsSidebar() {
  // Compose left the open draft (a reply started, a translation loaded).
  document.addEventListener("draft-detached", () => { setStatus("", ""); renderList(); refreshActiveControls(); });
  const search = $("#draft-search");
  if (search) {
    let t = null;
    search.addEventListener("input", () => {
      activeFilters.q = search.value;
      clearTimeout(t);
      t = setTimeout(loadDraftList, 200);
    });
  }

  const saveBtn = $("#draft-save");
  if (saveBtn) saveBtn.addEventListener("click", saveActiveDraft);
  const newBtn = $("#draft-new");
  if (newBtn) newBtn.addEventListener("click", newDraft);
  const delBtn = $("#draft-delete");
  if (delBtn) delBtn.addEventListener("click", deleteActiveDraft);
  refreshActiveControls(); // initial state

  // Ctrl/Cmd+S only when the Compose view is active
  document.addEventListener("keydown", (e) => {
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "s") {
      const composeSec = document.getElementById("compose");
      if (composeSec && composeSec.offsetParent !== null) {
        e.preventDefault();
        saveActiveDraft();
      }
    }
  });

  const toggle = document.getElementById("drafts-toggle");
  const sidebar = document.getElementById("drafts-sidebar");
  if (toggle && sidebar) {
    toggle.addEventListener("click", () => {
      const open = sidebar.classList.toggle("open");
      toggle.setAttribute("aria-expanded", String(open));
    });
  }
  const closeBtn = document.getElementById("drafts-close");
  if (closeBtn) closeBtn.addEventListener("click", closeMobileSidebar);

  installTagsInput();
  populateTranslateMenu();
  maybeShowRecoveryBanner();
  loadDraftList();
}

export async function deleteActiveDraft() {
  if (!state.activeDraftId) return;
  if (!confirm("Delete this draft? This cannot be undone.")) return;
  const del = async ifMatch => draftCall(draftPath(state.activeDraftId), { method: "DELETE", ifMatch });
  try {
    try {
      await del(state.draftETag || "*");
    } catch (e) {
      if (e.status === 404) { clearComposer(); return; } // already gone
      if (e.status !== 412) throw e;
      if (!confirm("This draft was changed in another tab since you opened it. Delete it anyway?")) return;
      await del("*");
    }
    clearComposer();
  } catch (e) {
    setStatus("error", "delete failed");
    flash("Draft not deleted: " + e.message);
    console.warn("deleteActiveDraft:", e);
  }
}

function refreshActiveControls() {
  const del = document.getElementById("draft-delete");
  if (del) del.hidden = !state.activeDraftId;
}

export function populateTranslateMenu() {
  const sel = document.getElementById("draft-translate");
  if (!sel) return;
  const targets = state.translateTargets || [];
  if (targets.length === 0) { sel.hidden = true; return; }
  sel.hidden = false;
  sel.replaceChildren(el("option", { value: "", text: "🌐 Translate to…" }),
    ...targets.map(t => el("option", { value: t, text: String(t).toUpperCase() })));
  // A property handler, so calling this again doesn't bind twice.
  sel.onchange = async () => {
    const target = sel.value;
    sel.value = "";
    if (!target) return;
    if (!state.activeDraftId) { flash("Save the draft first, then translate it"); return; }
    if (state.dirty) { flash("Save your changes first: the translation is made from the saved draft"); return; }
    setStatus("saving", "translating…");
    try {
      const { data, etag } = await draftCall(draftPath(state.activeDraftId) + ":translate", {
        method: "POST", body: { target_lang: target },
      });
      await showDraft(data, etag, "Translated → new draft");
      loadDraftList();
    } catch (e) {
      setStatus("error", "translate failed");
      flash("Translate failed: " + e.message);
      console.warn("translate:", e);
    }
  };
}
