"use strict";
import { $, ORDER } from "./common.js";
import { state, defaultOv } from "./state.js";
import { composeInit, applyIdentity } from "./compose.js";
import { historyInit, loadHistory } from "./history.js";
import { toolsInit, loadTools } from "./tools.js";
import { verifyInit } from "./verify.js";
import { interactInit } from "./interact.js";
import { installRecoveryAutosave } from "./drafts_recovery.js";
import { installDraftsSidebar, populateTranslateMenu } from "./drafts.js";
import { tokensInit, tokensShow, loadUserChip } from "./tokens.js";

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

// loadIdentity fetches the operator's real cross-platform profile and applies it
// to the composer (avatar, name, per-platform handles). Fire-and-forget after
// boot: the live profile calls can take a few seconds, and any failure simply
// leaves the built-in placeholders in place.
async function loadIdentity() {
  try {
    const r = await fetch("/api/identity", { credentials: "same-origin" });
    if (!r.ok) return;
    applyIdentity(await r.json());
  } catch (_) { /* keep placeholders */ }
}

// ---------------------------------------------------------------------------
// Tab routing
// ---------------------------------------------------------------------------

function switchTab(view) {
  document.querySelectorAll(".tab").forEach(b => b.classList.toggle("on", b.dataset.view === view));
  document.querySelectorAll(".view").forEach(s => { s.hidden = s.id !== view; });
  if (view === "history") loadHistory(true);
  if (view === "tools")   loadTools();
  if (view === "tokens")  tokensShow();
}

// ---------------------------------------------------------------------------
// Boot
// ---------------------------------------------------------------------------

async function init() {
  await loadConfig();
  composeInit();
  loadIdentity(); // fire-and-forget: swap placeholders for the real profile when it resolves
  historyInit();
  toolsInit();
  verifyInit();
  interactInit();
  tokensInit();
  // User chip (identity + sign-out) disabled: sign-out POST is CSRF-blocked
  // behind the proxy and the chip only surfaced the raw OIDC subject (UUID),
  // not a name/email. Re-enable once the cross-origin logout is resolved.
  // loadUserChip();
  installRecoveryAutosave();
  installDraftsSidebar();
  populateTranslateMenu(); // re-populate after /api/config resolves (idempotent)
  document.querySelectorAll(".tab").forEach(b =>
    b.addEventListener("click", () => switchTab(b.dataset.view)));
}

document.addEventListener("DOMContentLoaded", init);
