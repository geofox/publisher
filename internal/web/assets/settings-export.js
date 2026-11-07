"use strict";
import { el, v1, flash, copyText, asDialog } from "./common.js";
import { accounts, loadAccounts, reloadAccounts, networkName } from "./accounts.js";
import { readSettings } from "./settings-config.js";

// Settings → System → Export and import (spec §9). An export is a file of
// the accounts and settings, credentials included, sealed with a
// passphrase the server makes and shows once. An import reads such a file
// into this server: first what it would do, then, confirmed, does it.

// MAX_FILE is the largest file the server reads (export.MaxFileBytes).
const MAX_FILE = 768 << 10;

let node = null;
let importArea = null;

// portSection is the section, built once; showPort reads again whether
// this server can take an import.
export function portSection() {
  node = el("section", { class: "set-sect", id: "set-port" },
    el("h2", { class: "set-sect-title", text: "Export and import" }),
    el("p", { class: "set-sect-intro", text: "Move your accounts and settings to another Publisher, or keep a copy: " +
      "the accounts added here with their credentials, what you chose for the environment's accounts, your account sets, " +
      "and the settings saved here (the server's too). Not posts, drafts, media or API tokens." }),
    exportPart(),
    importArea = el("div", { class: "port-import" }));
  importArea.append(importPart());
  return node;
}

export function showPort() {
  if (!node) return;
  loadAccounts().then(() => markBlocked(), () => {});
}

// ── export ──────────────────────────────────────────────────────────────

function exportPart() {
  const status = el("p", { class: "set-status", role: "status" });
  const btn = el("button", { class: "primary sm", type: "button", text: "Export…" });
  btn.addEventListener("click", async () => {
    btn.disabled = true;
    status.className = "set-status";
    status.textContent = "Sealing the file…";
    try {
      const out = await v1("/v1/export", { method: "POST" });
      status.textContent = "";
      showExport(out);
    } catch (e) {
      status.textContent = e.cancelled ? "" : "The export failed: " + e.message;
      status.className = "set-status bad";
    } finally {
      btn.disabled = false;
    }
  });
  return el("div", { class: "port-part" },
    el("h3", { class: "set-sub", text: "Export" }),
    el("p", { class: "set-sect-intro", text: "Publisher makes a passphrase and shows it once: the file can't be opened without it. " +
      "With it, the file opens every account, so keep the two apart." }),
    el("div", { class: "set-item-actions" }, btn),
    status);
}

// showExport shows the passphrase, once, and the file to save. It closes
// only by its buttons: what it shows can't be shown again.
function showExport(out) {
  const words = (out.passphrase || "").split(" ");
  let saved = false;
  const bk = el("div", { class: "modal-bk" });
  let close = () => bk.remove();
  const list = el("ol", { class: "port-words", "aria-label": "The passphrase, " + words.length + " words" },
    ...words.map(w => el("li", { text: w })));
  const copy = el("button", { class: "ghost sm", type: "button", text: "Copy the passphrase" });
  copy.addEventListener("click", () => copyText(out.passphrase, list, "Passphrase copied"));
  const download = el("button", { class: "primary sm", type: "button", text: "Download the file" });
  download.addEventListener("click", () => {
    const url = URL.createObjectURL(new Blob([out.file], { type: "application/octet-stream" }));
    const a = el("a", { href: url, download: out.filename || "publisher.export", hidden: "" });
    document.body.append(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 30000);
    saved = true;
    warn.hidden = true;
  });
  const warn = el("p", { class: "fld-warn", hidden: "", role: "alert",
    text: "You haven't downloaded the file: it can't be shown again (export again for a new one). Close anyway?" });
  const done = el("button", { class: "ghost sm", type: "button", text: "Done" });
  done.addEventListener("click", () => {
    if (!saved && warn.hidden) { warn.hidden = false; return; }
    finish();
  });
  const parts = [[out.accounts, "account"], [out.account_sets, "account set"], [out.settings, "setting"]].filter(([n]) => n > 0).map(([n, w]) => count(n, w));
  const what = parts.length ? "The file holds " + parts.slice(0, -1).join(", ") + (parts.length > 1 ? " and " : "") + parts[parts.length - 1] +
    ", credentials included. " : "The file holds no accounts or settings yet. ";
  const card = el("div", { class: "modal port-dlg" },
    el("p", { class: "modal-title", text: "Your export" }),
    el("p", { class: "modal-desc", text: what +
      "Write the passphrase down or keep it in a password manager, apart from the file: it is shown this once, and Publisher doesn't keep it." }),
    list,
    el("div", { class: "confirm-actions port-actions" }, copy, download),
    warn,
    el("div", { class: "confirm-actions" }, done));
  const finish = () => {
    list.replaceChildren();
    out.passphrase = "";
    out.file = "";
    close();
  };
  bk.append(card);
  document.body.append(bk);
  close = asDialog(bk, card, close, download, { escape: false });
}

const count = (n, what) => (n || 0) + " " + what + (n === 1 ? "" : "s");

// ── import ──────────────────────────────────────────────────────────────

// markBlocked says, before anything is read, when this server can't take
// an import: it has accounts added in the app.
function markBlocked() {
  const note = importArea && importArea.querySelector(".port-blocked");
  if (!note) return;
  const n = accounts().filter(a => a.source === "db").length;
  note.hidden = n === 0;
}

function importPart() {
  const fileIn = el("input", { type: "file", id: "imp-file", accept: ".export,text/plain", "aria-describedby": "imp-file-err" });
  const fileErr = el("p", { class: "fld-err", id: "imp-file-err" });
  const passIn = el("textarea", { id: "imp-pass", rows: "3", autocomplete: "off", autocapitalize: "off", spellcheck: "false",
    "aria-describedby": "imp-pass-help imp-pass-err" });
  const passErr = el("p", { class: "fld-err", id: "imp-pass-err" });
  const read = el("button", { class: "primary sm", type: "submit", text: "Read the file" });
  const status = el("p", { class: "set-status", role: "status" });
  const out = el("div", { class: "port-summary" });
  const form = el("form", { class: "port-form", novalidate: "" },
    el("div", { class: "fld" }, el("label", { for: "imp-file", class: "fld-label", text: "Export file" }), fileIn, fileErr),
    el("div", { class: "fld" }, el("label", { for: "imp-pass", class: "fld-label", text: "Passphrase" }), passIn,
      el("p", { class: "fld-help", id: "imp-pass-help", text: "The 24 words shown at the export, in order." }), passErr),
    el("div", { class: "set-item-actions" }, read),
    status);
  const fieldErr = (input, p, msg) => {
    p.textContent = msg || "";
    if (msg) input.setAttribute("aria-invalid", "true"); else input.removeAttribute("aria-invalid");
  };
  const clearErrs = () => { fieldErr(fileIn, fileErr, ""); fieldErr(passIn, passErr, ""); status.textContent = ""; status.className = "set-status"; };
  form.addEventListener("submit", async ev => {
    ev.preventDefault();
    clearErrs();
    out.replaceChildren();
    const f = fileIn.files && fileIn.files[0];
    if (!f) { fieldErr(fileIn, fileErr, "Choose the file the export saved."); fileIn.focus(); return; }
    if (f.size > MAX_FILE) { fieldErr(fileIn, fileErr, "This file is larger than an export file can be."); fileIn.focus(); return; }
    if (!passIn.value.trim()) { fieldErr(passIn, passErr, "Type or paste the passphrase."); passIn.focus(); return; }
    read.disabled = true;
    status.textContent = "Opening the file…";
    try {
      const text = await f.text();
      const sum = await v1("/v1/import", { method: "POST", body: { file: text, passphrase: passIn.value } });
      status.textContent = "";
      // Opened: the passphrase isn't needed again.
      passIn.value = "";
      form.hidden = true;
      showSummary(out, sum, () => { form.hidden = false; out.replaceChildren(); fileIn.value = ""; });
    } catch (e) {
      const errs = (e.problem && e.problem.errors) || [];
      const at = ptr => errs.filter(x => x.pointer === ptr).map(x => x.detail).join("; ");
      if (at("/file")) { fieldErr(fileIn, fileErr, at("/file")); fileIn.focus(); }
      if (at("/passphrase")) { fieldErr(passIn, passErr, at("/passphrase")); if (!at("/file")) passIn.focus(); }
      if (!at("/file") && !at("/passphrase")) { status.textContent = e.message; status.className = "set-status bad"; }
      else status.textContent = "";
    } finally {
      read.disabled = false;
    }
  });
  return el("div", { class: "port-part" },
    el("h3", { class: "set-sub", text: "Import" }),
    el("p", { class: "set-sect-intro", text: "Into a server without accounts added in the app: what its environment sets stays as it is. " +
      "You see what the file would do before anything changes." }),
    el("div", { class: "set-note warn port-blocked", hidden: "" },
      el("p", { text: "This server has accounts added in the app, so it can't take an import (two setups are never merged). " +
        "Import on a new server, or delete those accounts first." })),
    form,
    out);
}

const ACTION = { add: "Will be added", update: "Here from the environment: its label, flags and defaults will be set", set: "Will be set" };

const when = iso => { const d = new Date(iso); return isNaN(d) ? "" : d.toLocaleString(); };

// accountLine is one account in a summary: its network and name, where
// its credentials go, and what happens (or happened) to it.
function accountLine(a, done) {
  const net = networkName(a.type);
  const label = a.label && a.label !== net ? " · " + a.label : "";
  const name = net + label + (a.handle && a.handle !== a.label ? " (" + a.handle + ")" : "");
  let what, cls = "";
  if (!done) {
    what = a.action === "skip" ? "Skipped: " + (a.why || "") : ACTION[a.action] || a.action;
    if (a.action === "skip") cls = "is-skip";
  } else {
    switch (a.status) {
      case "connected": what = "Connected" + (a.handle ? " as " + a.handle : ""); cls = "is-ok"; break;
      case "refused": what = "Added, but its network refused the credentials: sign in again from Accounts"; cls = "is-bad"; break;
      case "unchecked": what = "Added; its network didn't answer, so the next check will tell"; cls = "is-warn"; break;
      case "updated": what = "Updated"; cls = "is-ok"; break;
      default: what = "Skipped: " + (a.detail || a.why || ""); cls = "is-skip";
    }
    if (a.detail && a.status !== "skipped") what += " (" + a.detail + ")";
  }
  return el("li", { class: cls },
    el("span", { class: "port-name", text: name }),
    a.server && !done ? el("span", { class: "port-where" }, " credentials go to ", el("code", { text: a.server })) : null,
    el("span", { class: "port-what", text: what }));
}

function settingLine(s) {
  let val = "";
  if (s.secret) val = s.value ? "secret, for " + s.value : "secret";
  else if (Array.isArray(s.value)) val = s.value.join(", ");
  else if (s.value !== undefined && s.value !== null) val = String(s.value);
  return el("li", { class: s.action === "skip" ? "is-skip" : "" },
    el("span", { class: "port-name", text: s.label || s.key }),
    val ? el("span", { class: "port-where" }, " ", el("code", { text: val })) : null,
    el("span", { class: "port-what", text: s.action === "skip" ? "Skipped: " + (s.why || "") : "Will be set" + (s.scope === "instance" ? " (the server's)" : "") }));
}

function lists(sum, done) {
  const parts = [];
  const accts = sum.accounts || [];
  parts.push(el("h4", { class: "port-h", text: "Accounts" }),
    accts.length ? el("ul", { class: "port-list" }, ...accts.map(a => accountLine(a, done))) : el("p", { class: "set-status", text: "None." }));
  const sets = sum.account_sets || [];
  if (sets.length) {
    parts.push(el("h4", { class: "port-h", text: "Account sets" }), el("ul", { class: "port-list" }, ...sets.map(s =>
      el("li", { class: s.action === "skip" ? "is-skip" : "" }, el("span", { class: "port-name", text: s.name }),
        el("span", { class: "port-what", text: s.action === "skip" ? "Skipped: " + (s.why || "")
          : (done ? "Added" : "Will be added") + ", with " + count(s.accounts, "account") })))));
  }
  const settings = sum.settings || [];
  if (settings.length) {
    parts.push(el("h4", { class: "port-h", text: "Settings" }), el("ul", { class: "port-list" }, ...settings.map(s => {
      const li = settingLine(s);
      if (done && s.action !== "skip") li.querySelector(".port-what").textContent = "Set" + (s.scope === "instance" ? " (the server's)" : "");
      return li;
    })));
  }
  return parts;
}

// showSummary shows what the file would do, to import it or not.
function showSummary(out, sum, reset) {
  const status = el("p", { class: "set-status", role: "status" });
  const go = el("button", { class: "primary sm", type: "button", text: "Import" });
  const cancel = el("button", { class: "ghost sm", type: "button", text: "Cancel" });
  const adds = (sum.accounts || []).filter(a => a.action !== "skip").length + (sum.settings || []).filter(s => s.action !== "skip").length +
    (sum.account_sets || []).filter(s => s.action !== "skip").length;
  if (!adds) go.disabled = true;
  cancel.addEventListener("click", reset);
  go.addEventListener("click", async () => {
    go.disabled = cancel.disabled = true;
    status.className = "set-status";
    status.textContent = "Importing: each account is opened and asked who it is. This can take a minute…";
    try {
      const res = await v1("/v1/import:confirm", { method: "POST", body: { confirmation: sum.confirmation } });
      showResult(out, res, reset);
      flash("Imported");
      reloadAccounts().then(() => markBlocked(), () => {});
      readSettings("owner");
      readSettings("instance");
    } catch (e) {
      go.disabled = cancel.disabled = false;
      status.className = "set-status bad";
      status.textContent = e.cancelled ? e.message : "The import failed: " + e.message;
      if (e.status === 404) {
        go.disabled = true;
        status.textContent = "This file waited too long (10 minutes): read it again.";
      }
    }
  });
  out.replaceChildren(
    el("div", { class: "set-note" },
      el("p", { class: "set-note-title", text: "What this file would do" }),
      el("p", { text: "Exported " + when(sum.created_at) + ". Nothing changes until you import it" +
        (sum.expires_at ? ", by " + new Date(sum.expires_at).toLocaleTimeString() : "") + "." }),
      ...lists(sum, false)),
    el("div", { class: "set-item-actions" }, go, cancel),
    status);
  go.focus();
}

function showResult(out, res, reset) {
  const again = el("button", { class: "ghost sm", type: "button", text: "Done" });
  again.addEventListener("click", reset);
  const needs = (res.accounts || []).filter(a => a.status === "refused" || a.status === "unchecked").length;
  out.replaceChildren(
    el("div", { class: "set-note" + (needs ? " warn" : "") },
      el("p", { class: "set-note-title", text: "Imported" }),
      needs ? el("p", { text: count(needs, "account") + " need a look: see Accounts." }) : null,
      ...lists(res, true)),
    el("div", { class: "set-item-actions" }, again));
  again.focus();
}
