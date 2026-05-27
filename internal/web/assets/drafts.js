"use strict";
import { state, buildSpec, defaultOv } from "./state.js";
import { loadDraft, markDirty } from "./compose.js";
import { clearRecovery, snapshot } from "./drafts_recovery.js";

const $ = (sel, root = document) => root.querySelector(sel);

let activeFilters = { q: "", tags: new Set() };
let drafts = []; // last loaded list

function relativeTime(iso) {
  const d = new Date(iso);
  const s = Math.floor((Date.now() - d.getTime()) / 1000);
  if (s < 60) return s + "s";
  if (s < 3600) return Math.floor(s / 60) + "m";
  if (s < 86400) return Math.floor(s / 3600) + "h";
  return Math.floor(s / 86400) + "d";
}

export async function loadDraftList() {
  const qs = new URLSearchParams();
  if (activeFilters.q) qs.set("q", activeFilters.q);
  for (const t of activeFilters.tags) qs.append("tag", t);
  qs.set("limit", "50");
  try {
    const r = await fetch("/api/drafts?" + qs.toString(), { credentials: "same-origin" });
    if (!r.ok) throw new Error("HTTP " + r.status);
    drafts = await r.json();
    renderList();
    renderTagFilters();
  } catch (e) {
    console.warn("loadDraftList failed:", e);
  }
}

function renderList() {
  const list = $("#draft-list");
  if (!list) return;
  list.innerHTML = "";
  if (!drafts || drafts.length === 0) {
    const empty = document.createElement("div");
    empty.className = "muted";
    empty.style.padding = "8px";
    empty.textContent = "No drafts yet.";
    list.appendChild(empty);
    refreshActiveControls();
    return;
  }
  for (const d of drafts) {
    const row = document.createElement("div");
    row.className = "draft-row" + (state.activeDraftId === d.id ? " active" : "");
    row.dataset.id = d.id;
    row.innerHTML = `
      <div class="title">${escapeHTML(d.title || "(untitled)")}</div>
      <div class="preview">${escapeHTML(d.preview || "")}</div>
      <div class="meta">
        <span>${(d.tags || []).map(t => "#" + escapeHTML(t)).join(" ")}</span>
        <span class="age">${relativeTime(d.updated_at)}</span>
      </div>
    `;
    row.addEventListener("click", () => openDraft(d.id));
    list.appendChild(row);
  }
  refreshActiveControls();
}

function renderTagFilters() {
  const box = $("#draft-tags");
  if (!box) return;
  const all = new Set();
  for (const d of drafts || []) for (const t of d.tags || []) all.add(t);
  box.innerHTML = "";
  for (const t of all) {
    const chip = document.createElement("span");
    chip.className = "tag-chip" + (activeFilters.tags.has(t) ? " active" : "");
    chip.textContent = "#" + t;
    chip.addEventListener("click", () => {
      if (activeFilters.tags.has(t)) activeFilters.tags.delete(t);
      else activeFilters.tags.add(t);
      loadDraftList();
    });
    box.appendChild(chip);
  }
}

async function openDraft(id) {
  if (state.dirty && !confirm("Discard unsaved changes and load this draft?")) return;
  try {
    const r = await fetch("/api/drafts/" + encodeURIComponent(id), { credentials: "same-origin" });
    if (!r.ok) throw new Error("HTTP " + r.status);
    const d = await r.json();
    // hydrate spec from d.spec (stored JSON) augmented with d.media on the front
    let spec = {};
    try { spec = JSON.parse(d.spec || "{}"); } catch { spec = {}; }
    spec.id = d.id;
    spec.media = d.media || [];
    loadDraft(spec);
    renderTagChips(d.tags || []);
    clearRecovery();
    renderList();
    refreshActiveControls();
  } catch (e) {
    alert("Failed to load draft: " + e.message);
  }
}

function escapeHTML(s) {
  return String(s).replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

function setStatus(kind, text) {
  const el = document.getElementById("draft-status");
  if (!el) return;
  el.className = "draft-status " + (kind || "");
  el.textContent = text || "";
}

export async function saveActiveDraft() {
  const save = document.getElementById("draft-save");
  if (save) save.disabled = true;
  setStatus("saving", "saving…");
  const spec = buildSpec();
  spec.tags = currentTagChips();
  const fd = new FormData();
  fd.append("spec", JSON.stringify(spec));
  state.images.forEach((img, idx) => {
    if (img.file) fd.append("img_" + idx, img.file);
  });
  try {
    let url = "/api/drafts";
    let method = "POST";
    if (state.activeDraftId) {
      url = "/api/drafts/" + encodeURIComponent(state.activeDraftId);
      method = "PUT";
    }
    const r = await fetch(url, { method, body: fd, credentials: "same-origin" });
    if (!r.ok) {
      if (r.status === 404 && state.activeDraftId) {
        // stale id — fall through to a POST
        state.activeDraftId = null;
        return saveActiveDraft();
      }
      throw new Error("HTTP " + r.status);
    }
    const saved = await r.json();
    state.activeDraftId = saved.id;
    state.dirty = false;
    // refresh state.images to use the server-returned media (so subsequent saves don't re-upload)
    state.images = (saved.media || []).map(m => ({
      blossom_url: m.blossom_url, sha256: m.sha256, mime: m.mime,
      dim: m.dim, blurhash: m.blurhash, size_bytes: m.size_bytes,
      alt: m.alt || "", ordinal: m.ordinal, file: null,
      url: m.blossom_url || "",
    }));
    setStatus("saved", "saved just now");
    snapshot(); // clears recovery (since activeDraftId is now set)
    loadDraftList();
    refreshActiveControls();
  } catch (e) {
    setStatus("error", "save failed — retry");
    console.warn("saveActiveDraft:", e);
  } finally {
    if (save) save.disabled = false;
  }
}

export function newDraft() {
  if (state.dirty) {
    const choice = confirm("You have unsaved changes. OK to discard and start a new draft? (Cancel to keep editing.)");
    if (!choice) return;
  }
  // reset state
  state.master = "";
  state.activeDraftId = null;
  state.dirty = false;
  state.images = [];
  state.interaction = null;
  // reset overrides to defaults — iterate state.ov keys for safety
  for (const p of Object.keys(state.ov)) state.ov[p] = defaultOv(p);
  // best-effort UI refresh
  const ta = document.getElementById("master") || document.getElementById("m");
  if (ta) ta.value = "";
  renderTagChips([]);
  setStatus("", "");
  loadDraftList();
  refreshActiveControls();
}

function currentTagChips() {
  return Array.from(document.querySelectorAll("#draft-tags-input .tag-chip"))
    .map(el => el.dataset.tag);
}

function renderTagChips(tags) {
  const host = document.querySelector("#draft-tags-input .tag-chips");
  if (!host) return;
  host.innerHTML = "";
  for (const t of tags) {
    const chip = document.createElement("span");
    chip.className = "tag-chip";
    chip.dataset.tag = t;
    chip.textContent = "#" + t + " ✕";
    chip.addEventListener("click", () => {
      chip.remove();
      markDirty();
    });
    host.appendChild(chip);
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

export function installDraftsSidebar() {
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

  installTagsInput();
  populateTranslateMenu();
  loadDraftList();
}

export async function deleteActiveDraft() {
  if (!state.activeDraftId) return;
  if (!confirm("Delete this draft? This cannot be undone.")) return;
  try {
    const r = await fetch("/api/drafts/" + encodeURIComponent(state.activeDraftId), {
      method: "DELETE", credentials: "same-origin",
    });
    if (!r.ok && r.status !== 204) throw new Error("HTTP " + r.status);
    state.activeDraftId = null;
    state.dirty = false;
    state.master = "";
    state.images = [];
    const ta = document.getElementById("master") || document.getElementById("m");
    if (ta) ta.value = "";
    setStatus("", "");
    loadDraftList();
    refreshActiveControls();
  } catch (e) {
    setStatus("error", "delete failed");
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
  sel.innerHTML = `<option value="">🌐 Translate to…</option>` +
    targets.map(t => `<option value="${t}">${t.toUpperCase()}</option>`).join("");
  // attach the change handler once (replace any prior handler by cloning the node would lose the options;
  // instead, idempotently bind a property-based handler):
  sel.onchange = async () => {
    const target = sel.value;
    sel.value = "";
    if (!target || !state.activeDraftId) return;
    setStatus("saving", "translating…");
    try {
      const r = await fetch(
        "/api/drafts/" + encodeURIComponent(state.activeDraftId) + "/translate",
        { method: "POST", headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ target }), credentials: "same-origin" }
      );
      if (!r.ok) throw new Error("HTTP " + r.status);
      const newD = await r.json();
      // load the new draft into Compose
      let spec = {};
      try { spec = JSON.parse(newD.spec || "{}"); } catch {}
      spec.id = newD.id;
      spec.media = newD.media || [];
      loadDraft(spec);
      renderTagChips(newD.tags || []);
      setStatus("saved", "translated → new draft");
      loadDraftList();
    } catch (e) {
      setStatus("error", "translate failed");
      console.warn("translate:", e);
    }
  };
}
