"use strict";
import { el, $, flash, v1 } from "./common.js";
import { loadAccounts, accountsLoaded, connected, byId, isConnected, primaryOf, nameOf } from "./accounts.js";

// Relay sync for a Nostr account, on /v1/accounts/{id}/nostr/relays. With
// several connected Nostr accounts a picker chooses which one.

// nostrId is the Nostr account the tab works on ("" before one is known).
let nostrId = "";

// nostrAccounts lists the connected Nostr accounts.
function nostrAccounts() { return connected().filter(a => a.type === "nostr"); }

// relaysPath is the chosen Nostr account's relays resource; it throws when
// there is no connected Nostr account.
async function relaysPath() {
  await loadAccounts();
  if (!isConnected(byId(nostrId))) { const a = primaryOf("nostr"); nostrId = a ? a.id : ""; }
  if (!nostrId) throw new Error("no Nostr account is connected");
  return "/v1/accounts/" + encodeURIComponent(nostrId) + "/nostr/relays";
}

// renderPicker shows which Nostr account the relays are for, as a picker
// when there are several.
function renderPicker() {
  const host = $("#syncacct");
  if (!host) return;
  const list = nostrAccounts();
  host.replaceChildren();
  host.hidden = list.length < 2;
  if (list.length < 2) return;
  const sel = el("select", { "aria-label": "Nostr account", onchange: e => {
    // A load, scan or sync in progress works on the account it started
    // with: switching now would show its result under the other one.
    if (syncBusy) { e.target.value = nostrId; flash("Wait until the relay work in progress finishes"); return; }
    nostrId = e.target.value;
    const out = $("#syncout"); if (out) out.innerHTML = "";
    loadSyncRelays();
  } });
  for (const a of list) sel.append(el("option", { value: a.id, text: nameOf(a.id) }));
  sel.value = nostrId;
  host.append(el("label", {}, el("span", { class: "fl", text: "account" }), sel));
}

// ---------------------------------------------------------------------------
// Sync busy serialization
// ---------------------------------------------------------------------------

let syncBusy = false;
function setSyncBusy(b) {
  syncBusy = b;
  for (const id of ["#syncscan", "#syncpull", "#syncpush"]) { const e = $(id); if (e) e.disabled = b; }
}

// ---------------------------------------------------------------------------
// Relay set — collapsed wrapper with summary showing N auto · M custom
// ---------------------------------------------------------------------------

async function loadSyncRelays() {
  if (syncBusy) return;
  setSyncBusy(true);
  const c = $("#syncrelays"); c.innerHTML = "Loading…";
  try {
    const d = await v1(await relaysPath());
    syncSecondary = d.secondary || [];
    c.innerHTML = "";
    const home = $("#synchome");
    if (home) home.textContent = d.home ? "home: " + d.home.replace(/^wss?:\/\//, "") : "";

    const nip65Count = (d.nip65 || []).length;
    const secondaryCount = (d.secondary || []).length;

    // Collapsed <details> wrapper: summary shows counts + "edit" affordance
    const details = el("details", { class: "relay-set-details" });
    const summary = el("summary", { class: "relay-set-summary" },
      el("span", { class: "relay-set-label", text: "relay set" }),
      el("span", { class: "relay-set-counts",
        text: `· ${nip65Count} auto · ${secondaryCount} custom · edit` }),
    );
    details.append(summary);

    const body = el("div", { class: "relay-set-body" });

    const mk = (url, group) => el("div", { class: "relay" },
      el("span", { class: "rurl", text: url.replace(/^wss?:\/\//, "") }),
      group === "nip65"
        ? el("span", { class: "rnote", text: "auto · nip-65" })
        : el("button", { class: "rm", type: "button", text: "remove",
            onclick: () => removeSyncRelay(url) }),
    );
    for (const u of (d.nip65 || [])) body.append(mk(u, "nip65"));
    for (const u of (d.secondary || [])) body.append(mk(u, "secondary"));
    body.append(el("div", { class: "fields" },
      el("input", { type: "text", id: "syncadd", placeholder: "wss://relay.example" }),
      el("button", { class: "ghost sm", type: "button", text: "+ add",
        onclick: () => addSyncRelay($("#syncadd").value) }),
    ));

    details.append(body);
    c.append(details);
    if (d.nip65_error) c.append(el("div", { class: "rmsg", text: "Your relay list (NIP-65) couldn't be read: " + d.nip65_error }));
  } catch (e) { c.innerHTML = ""; c.append(el("div", { class: "err", text: "Error: " + e.message })); }
  finally { setSyncBusy(false); }
}

// The custom relays as last read; adding or removing one replaces the list.
let syncSecondary = [];

async function putSecondary(list, what) {
  try {
    await v1(await relaysPath(), { method: "PUT", body: { secondary: list } });
    loadSyncRelays();
  } catch (e) { flash(what + ": " + e.message); }
}

function addSyncRelay(url) {
  url = (url || "").trim();
  if (!url) return;
  putSecondary([...syncSecondary, url], "Add relay");
}

function removeSyncRelay(url) {
  putSecondary(syncSecondary.filter(u => u !== url), "Remove relay");
}

async function fetchSyncTargets() {
  const d = await v1(await relaysPath());
  return d.targets || [];
}

// renderPendingTargets lays out one pending row per target and returns a
// url→{row, meta, msg} map so each row can be filled in live as results arrive.
function renderPendingTargets(out, targets) {
  const rows = {};
  for (const t of targets) {
    const meta = el("span", { class: "meta", text: "·" });
    const row = el("div", { class: "relay" }, el("span", { class: "rurl", text: t.url.replace(/^wss?:\/\//, "") }), meta);
    rows[t.url] = { row, meta, msg: null };
    out.append(row);
  }
  return rows;
}

// updateSyncRow flips one row to its result; long error text goes on its own
// wrapping line (a sibling) so it never widens the row.
function updateSyncRow(entry, ok, metaText, message) {
  if (!entry) return;
  entry.row.className = "relay " + (ok ? "rok" : "rfail");
  entry.meta.textContent = metaText;
  if (entry.msg) { entry.msg.remove(); entry.msg = null; }
  if (!ok && message) { entry.msg = el("div", { class: "rmsg", text: message }); entry.row.after(entry.msg); }
}

// runSyncLive scans/applies the targets one at a time, filling each row live
// with an n/m counter. fetchOne(url, entry) handles a single relay and
// answers whether it went well. It answers the number of relays that
// didn't, or -1 when it couldn't run at all (busy, none, an error shown).
async function runSyncLive(label, fetchOne) {
  if (syncBusy) return -1;
  setSyncBusy(true);
  const out = $("#syncout"); out.innerHTML = "";
  try {
    const targets = await fetchSyncTargets();
    if (targets.length === 0) { out.append(el("div", { class: "muted", text: "no target relays" })); return -1; }
    const head = el("div", { class: "relays-head", text: `${label}  0/${targets.length}` });
    out.append(head);
    const rows = renderPendingTargets(out, targets);
    let done = 0, failed = 0;
    for (const t of targets) {
      if (rows[t.url]) rows[t.url].meta.textContent = "…";
      if ((await fetchOne(t.url, rows[t.url])) === false) failed++;
      head.textContent = `${label}  ${++done}/${targets.length}`;
    }
    head.textContent = `${label} · ${targets.length} relays`;
    return failed;
  } catch (e) { out.innerHTML = ""; out.append(el("div", { class: "err", text: "Error: " + e.message })); return -1; }
  finally { setSyncBusy(false); }
}

function syncScan() {
  return runSyncLive("scanning", async (url, entry) => {
    try {
      const d = await v1(await relaysPath() + ":scan", { method: "POST", body: { relays: [url] } });
      const res = (d.relays && d.relays[0]) || { status: "error", message: "no result" };
      updateSyncRow(entry, res.status === "ok",
        res.status === "ok" ? `↓${res.missing_at_home}  ↑${res.missing_at_relay}` : res.status, res.message);
    } catch (e) { updateSyncRow(entry, false, "error", e.message); }
  });
}

function syncApply(direction) {
  return runSyncLive(direction, async (url, entry) => {
    try {
      const d = await v1(await relaysPath() + ":sync", { method: "POST", body: { direction, relays: [url] } });
      const res = (d.results && d.results[0]) || { status: "error", message: "no result" };
      let meta;
      if (res.status === "ok") meta = `published ${res.published}`;
      else if (res.failed) meta = (res.published ? `published ${res.published} · ` : "") + `failed ${res.failed}`;
      else meta = res.status; // unreachable / auth
      updateSyncRow(entry, res.status === "ok", meta, res.message);
      return res.status === "ok";
    } catch (e) { updateSyncRow(entry, false, "error", e.message); return false; }
  }).then(failed => {
    // Only a run that finished says so; its errors are on screen.
    if (failed === 0) flash(`${direction}: done`);
    else if (failed > 0) flash(`${direction}: ${failed} relay(s) failed`);
  });
}

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

// relaySyncOn is features.relay_sync (GET /v1/instance): a connected
// Nostr account can sync relays.
let relaySyncOn = true;
export function relaySyncOff() { relaySyncOn = false; }

export async function loadTools() {
  if (!relaySyncOn) { showNoRelaySync(); return; }
  try { await loadAccounts(); } catch { /* relaysPath says so */ }
  if (accountsLoaded() && !nostrAccounts().length) { showNoRelaySync(); return; }
  showRelaySync();
  await relaysPath().catch(() => {});
  renderPicker();
  loadSyncRelays();
}

// showNoRelaySync says why there is nothing to sync, instead of an error.
function showNoRelaySync() {
  const c = $("#syncrelays");
  if (c) c.replaceChildren(el("div", { class: "muted", text: "Relay sync needs a connected Nostr account (set NSEC_HEX)." }));
  for (const id of ["#syncacct", "#reconcile-head", "#syncscan", "#syncpull", "#syncpush", "#syncout"]) { const e = $(id); if (e) e.hidden = true; }
}

// showRelaySync shows the controls again (a Nostr account came back).
function showRelaySync() {
  for (const id of ["#reconcile-head", "#syncscan", "#syncpull", "#syncpush", "#syncout"]) { const e = $(id); if (e) e.hidden = false; }
}

export function toolsInit() {
  // Wire reconcile buttons with labeled text + hint one-liners (spec §4)
  const scanBtn = $("#syncscan");
  const pullBtn = $("#syncpull");
  const pushBtn = $("#syncpush");

  if (scanBtn) {
    scanBtn.innerHTML = "";
    scanBtn.append(
      document.createTextNode("Scan"),
      el("span", { class: "hint", text: "compare home ↔ relays, show what's missing on each side" }),
    );
    scanBtn.addEventListener("click", syncScan);
  }
  if (pullBtn) {
    pullBtn.innerHTML = "";
    pullBtn.append(
      document.createTextNode("Pull → home"),
      el("span", { class: "hint", text: "copy events your relays have that home is missing" }),
    );
    pullBtn.addEventListener("click", () => syncApply("pull"));
  }
  if (pushBtn) {
    pushBtn.innerHTML = "";
    pushBtn.append(
      document.createTextNode("Push → relays"),
      el("span", { class: "hint", text: "broadcast home's events out to your write relays" }),
    );
    pushBtn.addEventListener("click", () => syncApply("push"));
  }
}
