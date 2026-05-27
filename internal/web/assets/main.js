"use strict";
import { $, ORDER } from "./common.js";
import { state, defaultOv } from "./state.js";
import { composeInit } from "./compose.js";
import { historyInit, loadHistory } from "./history.js";
import { toolsInit, loadTools } from "./tools.js";
import { verifyInit } from "./verify.js";
import { interactInit } from "./interact.js";
import { installRecoveryAutosave } from "./drafts_recovery.js";
import { installDraftsSidebar } from "./drafts.js";

// loadConfig fetches operator preferences (currently just USER_LANGUAGES) and
// rebuilds the per-platform overrides so the Bluesky/Mastodon language fields
// default to the operator's first configured language. Failures fall back to
// the "en" default already in defaultOv.
async function loadConfig() {
  try {
    const r = await fetch("/api/config", { credentials: "same-origin" });
    if (!r.ok) return;
    const data = await r.json();
    if (Array.isArray(data.user_languages) && data.user_languages.length) {
      state.userLanguages = data.user_languages;
      ORDER.forEach(p => { state.ov[p] = defaultOv(p); });
    }
    if (Array.isArray(data.translate_targets)) {
      state.translateTargets = data.translate_targets;
    }
  } catch (_) { /* keep the "en" defaults; translate button stays hidden */ }
}

// ---------------------------------------------------------------------------
// Tab routing
// ---------------------------------------------------------------------------

function switchTab(view) {
  document.querySelectorAll(".tab").forEach(b => b.classList.toggle("on", b.dataset.view === view));
  document.querySelectorAll(".view").forEach(s => { s.hidden = s.id !== view; });
  if (view === "history") loadHistory(true);
  if (view === "tools")   loadTools();
}

// ---------------------------------------------------------------------------
// Boot
// ---------------------------------------------------------------------------

async function init() {
  await loadConfig();
  composeInit();
  historyInit();
  toolsInit();
  verifyInit();
  interactInit();
  installRecoveryAutosave();
  installDraftsSidebar();
  document.querySelectorAll(".tab").forEach(b =>
    b.addEventListener("click", () => switchTab(b.dataset.view)));
}

document.addEventListener("DOMContentLoaded", init);
