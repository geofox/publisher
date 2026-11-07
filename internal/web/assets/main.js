"use strict";
import { $ } from "./common.js";
import { state, byKey, selected } from "./state.js";
import { composeInit, applyIdentity, rerenderComposer, setAccountsReloader, setAccountsPhase } from "./compose.js";
import { loadAccounts, reloadAccounts, accounts, accountsLoaded, byId, identity, loadInstance, defaultTargets } from "./accounts.js";
import { historyInit, loadHistory } from "./history.js";
import { toolsInit, loadTools, relaySyncOff } from "./tools.js";
import { verifyInit } from "./verify.js";
import { focusFirstSteps } from "./settings-accounts.js";
import { interactInit } from "./interact.js";
import { installRecoveryAutosave } from "./drafts_recovery.js";
import { installDraftsSidebar, populateTranslateMenu } from "./drafts.js";
import { tokensInit, setTokensMode } from "./tokens.js";
import { setReauthHandler } from "./common.js";
import { reauth, setReauthMode } from "./reauth.js";
import { setAccessMode } from "./access.js";
import { settingsInit, settingsShow, settingsHidden } from "./settings.js";

// loadConfig reads the instance (GET /v1/instance): the operator's languages
// (the Bluesky/Mastodon language fields default to the first) and the
// languages DeepL can translate into. A failure keeps the "en" default and
// hides translation. It answers the instance (null on a failure).
let instance = null;
async function loadConfig() {
  try {
    const data = await loadInstance();
    instance = data;
    if (Array.isArray(data.user_languages) && data.user_languages.length) {
      state.userLanguages = data.user_languages;
      state.ov = byKey(); // each account's settings start from these languages
    }
    if (Array.isArray(data.translate_targets)) {
      state.translateTargets = data.translate_targets;
    }
    if (data.features && data.features.media === false) mediaStore(false);
    if (data.features && data.features.relay_sync === false) relaySyncOff();
    state.proxyAuth = data.auth === "proxy";
    setReauthMode(data.auth);
    setAccessMode(data.auth);
    setTokensMode(data.auth);
    if (data.sign_in_recommended) signInBanner();
  } catch (_) { /* keep the "en" defaults; translate button stays hidden */ }
  return instance;
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

// mediaStore shows the attach controls, or hides them on a server without
// a media store, saying why (an upload would be refused). Settings, Media
// changes it without a reload.
let mediaNote = null;
function mediaStore(on) {
  state.mediaOff = !on; // the composer keeps #vidpreset hidden too
  const add = $("#addimg"); if (add) add.hidden = !on;
  const vp = $("#vidpreset"); if (vp) vp.hidden = !on || state.images.some(i => i.video);
  const meta = $("#media-meta");
  if (!meta) return;
  if (mediaNote === null) mediaNote = meta.textContent;
  meta.textContent = on ? mediaNote : "no media store on this server: choose one in Settings, Media";
}

// settingsSaved follows what Settings saves that the composer shows: the
// media store, the languages and translation.
function settingsSaved(e) {
  const { scope, body } = e.detail || {};
  const leaf = (g, n) => body && body[g] && body[g][n];
  if (scope === "instance" && leaf("media", "store")) mediaStore(leaf("media", "store").value !== "none");
  if (scope === "owner") {
    loadInstance().then(data => {
      if (Array.isArray(data.user_languages) && data.user_languages.length) state.userLanguages = data.user_languages;
      if (Array.isArray(data.translate_targets)) state.translateTargets = data.translate_targets;
      populateTranslateMenu();
    }, () => { /* the next reload has them */ });
  }
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

function switchTab(view, section) {
  document.querySelectorAll(".tab").forEach(b => b.classList.toggle("on", b.dataset.view === view));
  document.querySelectorAll(".view").forEach(s => { s.hidden = s.id !== view; });
  if (view === "history") loadHistory(true);
  if (view === "tools")   loadTools();
  if (view === "settings") settingsShow(section); else settingsHidden();
}

// firstRun opens Settings → Accounts once, in this browser, on a server
// with no account yet (and where they can be added: not behind the proxy
// alone), so a new instance starts with its setup.
const FIRST_RUN_KEY = "publisher.firstRunShown";
function firstRun(list) {
  if (!Array.isArray(list) || list.length || !instance || instance.auth === "proxy") return;
  try {
    if (localStorage.getItem(FIRST_RUN_KEY)) return;
    localStorage.setItem(FIRST_RUN_KEY, "1");
  } catch (_) { return; }
  focusFirstSteps(); // the keyboard starts at "Get started"
  switchTab("settings", "accounts");
}

// ---------------------------------------------------------------------------
// Boot
// ---------------------------------------------------------------------------

async function init() {
  // An operation that needs the owner to confirm it's them asks here.
  setReauthHandler(reauth);
  await loadConfig();
  settingsInit(instance);
  // Every successful read of the accounts (here, a Post, a draft, a
  // restore) is applied as it arrives.
  document.addEventListener("accounts", applyAccounts);
  // The accounts carry the limits, statuses and profiles. Wait briefly so
  // the composer starts with them; if they are slower, it takes them on
  // arrival.
  const first = readAccounts().catch(() => { /* shown in the composer; tried again */ });
  first.then(firstRun);
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
  // The composer's "no accounts" and the master key banner lead to them.
  document.addEventListener("open-settings", e => switchTab("settings", e.detail));
  document.addEventListener("settings-saved", settingsSaved);
  const go = $("#key-banner-go");
  if (go) go.addEventListener("click", () => switchTab("settings", "accounts"));
}

document.addEventListener("DOMContentLoaded", init);
