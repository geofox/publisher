"use strict";
import { el, $, flash } from "./common.js";

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
    const r = await fetch("/api/sync/relays", { credentials: "same-origin" });
    const d = await r.json();
    if (!r.ok) throw new Error(d.error || ("HTTP " + r.status));
    c.innerHTML = "";

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
  } catch (e) { c.innerHTML = ""; c.append(el("div", { class: "err", text: "Error: " + e.message })); }
  finally { setSyncBusy(false); }
}

async function addSyncRelay(url) {
  url = (url || "").trim();
  if (!url) return;
  try {
    const r = await fetch("/api/sync/relays", { method: "POST", credentials: "same-origin",
      headers: { "Content-Type": "application/json" }, body: JSON.stringify({ url }) });
    const d = await r.json();
    if (!r.ok) throw new Error(d.error || ("HTTP " + r.status));
    loadSyncRelays();
  } catch (e) { flash("Add relay: " + e.message); }
}

async function removeSyncRelay(url) {
  try {
    const r = await fetch("/api/sync/relays", { method: "DELETE", credentials: "same-origin",
      headers: { "Content-Type": "application/json" }, body: JSON.stringify({ url }) });
    if (!r.ok) { const d = await r.json(); throw new Error(d.error || ("HTTP " + r.status)); }
    loadSyncRelays();
  } catch (e) { flash("Remove relay: " + e.message); }
}

async function fetchSyncTargets() {
  const r = await fetch("/api/sync/targets", { credentials: "same-origin" });
  const d = await r.json();
  if (!r.ok) throw new Error(d.error || ("HTTP " + r.status));
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
// with an n/m counter. fetchOne(url, entry) handles a single relay.
async function runSyncLive(label, fetchOne) {
  if (syncBusy) return;
  setSyncBusy(true);
  const out = $("#syncout"); out.innerHTML = "";
  try {
    const targets = await fetchSyncTargets();
    if (targets.length === 0) { out.append(el("div", { class: "muted", text: "no target relays" })); return; }
    const head = el("div", { class: "relays-head", text: `${label}  0/${targets.length}` });
    out.append(head);
    const rows = renderPendingTargets(out, targets);
    let done = 0;
    for (const t of targets) {
      if (rows[t.url]) rows[t.url].meta.textContent = "…";
      await fetchOne(t.url, rows[t.url]);
      head.textContent = `${label}  ${++done}/${targets.length}`;
    }
    head.textContent = `${label} · ${targets.length} relays`;
  } catch (e) { out.innerHTML = ""; out.append(el("div", { class: "err", text: "Error: " + e.message })); }
  finally { setSyncBusy(false); }
}

function syncScan() {
  return runSyncLive("scanning", async (url, entry) => {
    try {
      const r = await fetch("/api/sync/scan", { method: "POST", credentials: "same-origin",
        headers: { "Content-Type": "application/json" }, body: JSON.stringify({ relays: [url] }) });
      const d = await r.json();
      if (!r.ok) throw new Error(d.error || ("HTTP " + r.status));
      const res = (d.relays && d.relays[0]) || { status: "error", message: "no result" };
      updateSyncRow(entry, res.status === "ok",
        res.status === "ok" ? `↓${res.missing_at_home}  ↑${res.missing_at_relay}` : res.status, res.message);
    } catch (e) { updateSyncRow(entry, false, "error", e.message); }
  });
}

function syncApply(direction) {
  return runSyncLive(direction, async (url, entry) => {
    try {
      const r = await fetch("/api/sync/apply", { method: "POST", credentials: "same-origin",
        headers: { "Content-Type": "application/json" }, body: JSON.stringify({ direction, relays: [url] }) });
      const d = await r.json();
      if (!r.ok) throw new Error(d.error || ("HTTP " + r.status));
      const res = (d.results && d.results[0]) || { status: "error", message: "no result" };
      let meta;
      if (res.status === "ok") meta = `published ${res.published}`;
      else if (res.failed) meta = (res.published ? `published ${res.published} · ` : "") + `failed ${res.failed}`;
      else meta = res.status; // unreachable / auth
      updateSyncRow(entry, res.status === "ok", meta, res.message);
    } catch (e) { updateSyncRow(entry, false, "error", e.message); }
  }).then(() => flash(`${direction}: done`));
}

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

export function loadTools() {
  loadSyncRelays();
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
