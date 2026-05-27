"use strict";
import { state } from "./state.js";
import { loadDraft } from "./compose.js";
import { clearRecovery } from "./drafts_recovery.js";

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
    clearRecovery();
    renderList();
  } catch (e) {
    alert("Failed to load draft: " + e.message);
  }
}

function escapeHTML(s) {
  return String(s).replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
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
  loadDraftList();
}
