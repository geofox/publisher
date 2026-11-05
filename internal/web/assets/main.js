"use strict";
import { $, META, ORDER } from "./common.js";
import { state, defaultOv } from "./state.js";
import { composeInit, applyIdentity, rerenderComposer } from "./compose.js";
import { loadAccounts, accountFor, identityFromAccounts, loadInstance } from "./accounts.js";
import { historyInit, loadHistory } from "./history.js";
import { toolsInit, loadTools } from "./tools.js";
import { verifyInit } from "./verify.js";
import { interactInit } from "./interact.js";
import { installRecoveryAutosave } from "./drafts_recovery.js";
import { installDraftsSidebar, populateTranslateMenu } from "./drafts.js";
import { tokensInit, tokensShow, loadUserChip } from "./tokens.js";

// loadConfig reads the instance (GET /v1/instance): the operator's languages
// (the Bluesky/Mastodon language fields default to the first) and the
// languages DeepL can translate into. A failure keeps the "en" default and
// hides translation.
async function loadConfig() {
  try {
    const data = await loadInstance();
    if (Array.isArray(data.user_languages) && data.user_languages.length) {
      state.userLanguages = data.user_languages;
      ORDER.forEach(p => { state.ov[p] = defaultOv(p); });
    }
    if (Array.isArray(data.translate_targets)) {
      state.translateTargets = data.translate_targets;
    }
  } catch (_) { /* keep the "en" defaults; translate button stays hidden */ }
}

// applyAccounts takes each type's name and text limit from its account, so
// the composer counts against what the server splits by (a Mastodon instance
// announces its own limit), and swaps the placeholder author for the
// accounts' profiles. Types with no account keep the built-in META values.
function applyAccounts() {
  for (const p of ORDER) {
    const a = accountFor(p);
    if (!a || !META[p]) continue;
    if (typeof a.label === "string" && a.label) META[p].label = a.label;
    const max = a.capabilities && a.capabilities.text && a.capabilities.text.max_length;
    if (Number.isInteger(max) && max >= 0) META[p].limit = max;
  }
  if (!composed) return; // composeInit draws with them
  applyIdentity(identityFromAccounts(ORDER));
  rerenderComposer();
}
let composed = false;

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
  // The accounts carry the effective limits and live profiles; profiles can
  // take a few seconds on a cold cache. Wait briefly so the composer starts
  // with the right limits; if they are slower, it takes them on arrival.
  const accounts = loadAccounts().then(applyAccounts).catch(() => { /* keep built-ins and placeholders */ });
  await Promise.race([accounts, new Promise(r => setTimeout(r, 1500))]);
  composeInit();
  composed = true;
  accounts.then(applyAccounts); // now with the profiles, drawn into the composer
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
  populateTranslateMenu(); // re-populate after the instance resolves (idempotent)
  document.querySelectorAll(".tab").forEach(b =>
    b.addEventListener("click", () => switchTab(b.dataset.view)));
}

document.addEventListener("DOMContentLoaded", init);
