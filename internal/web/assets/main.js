"use strict";
import { $ } from "./common.js";
import { state, byKey, selected } from "./state.js";
import { composeInit, applyIdentity, rerenderComposer, setAccountsReloader, setAccountsPhase } from "./compose.js";
import { loadAccounts, reloadAccounts, accounts, accountsLoaded, byId, identity, loadInstance, defaultTargets } from "./accounts.js";
import { historyInit, loadHistory } from "./history.js";
import { toolsInit, loadTools, relaySyncOff } from "./tools.js";
import { verifyInit } from "./verify.js";
import { interactInit } from "./interact.js";
import { installRecoveryAutosave } from "./drafts_recovery.js";
import { installDraftsSidebar, populateTranslateMenu } from "./drafts.js";
import { tokensInit, tokensShow, setTokensMode } from "./tokens.js";
import { setReauthHandler } from "./common.js";
import { reauth, setReauthMode } from "./reauth.js";
import { accessShow, setAccessMode } from "./access.js";

// loadConfig reads the instance (GET /v1/instance): the operator's languages
// (the Bluesky/Mastodon language fields default to the first) and the
// languages DeepL can translate into. A failure keeps the "en" default and
// hides translation.
async function loadConfig() {
  try {
    const data = await loadInstance();
    if (Array.isArray(data.user_languages) && data.user_languages.length) {
      state.userLanguages = data.user_languages;
      state.ov = byKey(); // each account's settings start from these languages
    }
    if (Array.isArray(data.translate_targets)) {
      state.translateTargets = data.translate_targets;
    }
    if (data.features && data.features.media === false) noMediaStore();
    if (data.features && data.features.relay_sync === false) relaySyncOff();
    setReauthMode(data.auth);
    setAccessMode(data.auth);
    setTokensMode(data.auth);
    if (data.sign_in_recommended) signInBanner();
  } catch (_) { /* keep the "en" defaults; translate button stays hidden */ }
}

// signInBanner suggests setting up sign-in on a server with none of its
// own (proxy mode), until "Not now" (for 30 days, in this browser).
const BANNER_KEY = "publisher.signinBannerSnoozed";
function signInBanner() {
  const b = $("#signin-banner");
  if (!b) return;
  let until = 0;
  try { until = Number(localStorage.getItem(BANNER_KEY)) || 0; } catch (_) { /* storage off */ }
  if (Date.now() < until) return;
  b.hidden = false;
  $("#signin-banner-x").addEventListener("click", () => {
    b.hidden = true;
    try { localStorage.setItem(BANNER_KEY, String(Date.now() + 30 * 86400e3)); } catch (_) { /* storage off */ }
  });
}

// noMediaStore hides the attach controls on a server without a media
// store, saying why (an upload would be refused).
function noMediaStore() {
  state.mediaOff = true; // the composer keeps #vidpreset hidden too
  for (const id of ["#addimg", "#vidpreset"]) { const e = $(id); if (e) e.hidden = true; }
  const meta = $("#media-meta");
  if (meta) meta.textContent = "no media store on this server (set BLOSSOM_URL)";
}

// applyAccounts runs after each successful read of the accounts (the
// "accounts" event): the selection keeps only accounts that still exist, a
// new post gets the default selection, and the composer redraws with each
// account's limits, handle and status, under the owner's profile.
function applyAccounts() {
  for (const id of [...state.targets]) if (!byId(id)) state.targets.delete(id);
  selectDefaults();
  setAccountsPhase("ready");
  if (!composed) return; // composeInit draws with them
  applyIdentity(identity());
  rerenderComposer();
}
let composed = false;

// selectDefaults selects, for a post whose accounts nobody has chosen yet,
// the connected accounts that new posts select. Once there are accounts it
// runs once: a later reload never changes a selection.
function selectDefaults() {
  if (state.chosen) return;
  state.targets = new Set(defaultTargets());
  if (accounts().length) state.chosen = true;
  if (!state.targets.has(state.focus)) state.focus = selected()[0] || "";
}

// readAccounts reads the accounts; while none could ever be read it says so
// in the composer and tries again, later each time (up to a minute).
let retryDelay = 0, retryTimer = null;
function readAccounts(read = loadAccounts) {
  return read().then(list => { retryDelay = 0; clearTimeout(retryTimer); return list; }, e => {
    if (!accountsLoaded()) {
      setAccountsPhase("error", e.message || "the server didn't answer");
      retryDelay = Math.min(retryDelay ? retryDelay * 2 : 2000, 60000);
      clearTimeout(retryTimer);
      retryTimer = setTimeout(() => readAccounts().catch(() => {}), retryDelay);
    }
    throw e;
  });
}

// ---------------------------------------------------------------------------
// Tab routing
// ---------------------------------------------------------------------------

function switchTab(view) {
  document.querySelectorAll(".tab").forEach(b => b.classList.toggle("on", b.dataset.view === view));
  document.querySelectorAll(".view").forEach(s => { s.hidden = s.id !== view; });
  if (view === "history") loadHistory(true);
  if (view === "tools")   loadTools();
  if (view === "tokens")  { accessShow(); tokensShow(); }
}

// ---------------------------------------------------------------------------
// Boot
// ---------------------------------------------------------------------------

async function init() {
  // An operation that needs the owner to confirm it's them asks here.
  setReauthHandler(reauth);
  await loadConfig();
  // Every successful read of the accounts (here, a Post, a draft, a
  // restore) is applied as it arrives.
  document.addEventListener("accounts", applyAccounts);
  // The accounts carry the limits, statuses and profiles. Wait briefly so
  // the composer starts with them; if they are slower, it takes them on
  // arrival.
  const first = readAccounts().catch(() => { /* shown in the composer; tried again */ });
  await Promise.race([first, new Promise(r => setTimeout(r, 1500))]);
  composeInit();
  composed = true;
  if (accountsLoaded()) { applyIdentity(identity()); rerenderComposer(); }
  // A Post that finds no usable account asks again first.
  setAccountsReloader(() => readAccounts(reloadAccounts));
  historyInit();
  toolsInit();
  verifyInit();
  interactInit();
  tokensInit();
  // Signing out is in Access (this browser's session row).
  installRecoveryAutosave();
  installDraftsSidebar();
  populateTranslateMenu(); // re-populate after the instance resolves (idempotent)
  document.querySelectorAll(".tab").forEach(b =>
    b.addEventListener("click", () => switchTab(b.dataset.view)));
}

document.addEventListener("DOMContentLoaded", init);
