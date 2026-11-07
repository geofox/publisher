"use strict";
import { el, v1, flash, relTime, confirmModal, inputDialog, asDialog } from "./common.js";
import { accounts, reloadAccounts, byId, loadDestinations, networkName, customLabel, handleOf, nameOf } from "./accounts.js";
import { accountTile, brandTile } from "./brands.js";
import { field, fieldSet, showProblems, sameValue, badInputs } from "./fields.js";
import { waitText } from "./webauthn.js";

// Settings → Accounts: every account, grouped by network, with its status
// and last check. Adding one: a network, a way of connecting, then a form
// built from its type's fields (GET /v1/destinations). Per account:
// checking it, its flags, renaming, its settings and credentials, moving
// an environment account into the app, showing a Nostr key, deleting.
// The server checks everything; this only shapes requests and says what
// it answered.

// What the instance said (GET /v1/instance): how the owner signs in, the
// master key's status, the features.
let inst = { auth: "", secrets: { status: "none", detail: "" }, features: {} };
export function setAccountsInstance(i) { if (i) inst = { ...inst, ...i }; }
// A media store chosen in Settings, Media counts at once.
document.addEventListener("settings-saved", e => {
  const b = e.detail && e.detail.scope === "instance" && e.detail.body;
  if (b && b.media && b.media.store) inst = { ...inst, features: { ...(inst.features || {}), media: b.media.store.value !== "none" } };
});

const proxy = () => inst.auth === "proxy";
const keyStatus = () => (inst.secrets && inst.secrets.status) || "none";
const enc = encodeURIComponent;
const panel = () => document.getElementById("set-accounts");

// writeBlocked says why credentials can't be saved here now, or "".
function writeBlocked() {
  if (proxy()) return "Behind the reverse proxy alone, credentials can't be managed here: set up sign-in first.";
  switch (keyStatus()) {
    case "ok": return "";
    case "none": return "Set a master key first.";
    default: return (inst.secrets && inst.secrets.detail) || "Fix the master key first.";
  }
}

// The types this server has, once read.
let types = null, typesError = "";
const manifest = t => (types || []).find(m => m.type === t) || null;
// usable lists the ways of connecting the app can offer now (a redirect
// comes with part E).
const usable = m => (m.auth || []).filter(a => a.flow === "fields" || a.flow === "generate");

function readTypes() {
  return loadDestinations().then(list => { types = list; typesError = ""; }, e => { typesError = e.message; });
}

// Hints, not refusals: shown under a field as the owner types.
const WARN = {
  bluesky: {
    app_password: v => /^[a-z0-9]{4}-[a-z0-9]{4}-[a-z0-9]{4}-[a-z0-9]{4}$/i.test(v) ? ""
      : "This doesn't look like an app password (xxxx-xxxx-xxxx-xxxx). If it's your main password, make an app password instead: it can post, but can't change your account.",
  },
};
const warnFor = (type, key) => (WARN[type] && WARN[type][key]) || null;

// ── the panel ───────────────────────────────────────────────────────────

let shown = false;
// pendingFocus is the control to focus once the panel is drawn again (a
// flag just changed, an account checked).
let pendingFocus = "";
// reading is how the last read of the accounts went: "loading", "error"
// (readError says why) or "ok". Without accounts, only "ok" shows the
// first steps: a failed read isn't an empty server.
let reading = "ok", readError = "";

// accountsShow draws the panel and reads the accounts and types afresh.
export function accountsShow() {
  shown = true;
  if (!accounts().length) reading = "loading";
  render();
  readTypes().then(render);
  readAccounts();
}

function readAccounts() {
  return reloadAccounts().then(() => { reading = "ok"; readError = ""; if (shown) render(); },
    e => { reading = "error"; readError = e.message; if (shown) render(); });
}

// readNote stands for the list while it is read, or couldn't be.
function readNote() {
  if (reading === "loading") return el("p", { class: "set-reading", role: "status", text: "Reading your accounts…" });
  const again = el("button", { class: "ghost sm", type: "button", text: "Try again", "data-focus-key": "acct-read-again",
    onclick: () => { reading = "loading"; pendingFocus = "acct-read-again"; render(); readAccounts(); } });
  return el("div", { class: "set-note bad", role: "alert" },
    el("p", { text: "Couldn't read your accounts: " + readError }), again);
}

export function accountsHide() { shown = false; }

// Every read of the accounts redraws the panel while it shows.
document.addEventListener("accounts", () => { if (shown) render(); });

function render() {
  const box = panel();
  if (!box) return;
  const active = document.activeElement;
  const key = pendingFocus || (box.contains(active) && active.dataset ? active.dataset.focusKey || "" : "");
  pendingFocus = "";
  const list = accounts();
  // A new server shows its first steps instead of an empty list (once the
  // read says it is empty).
  const first = list.length ? null : reading === "ok" ? firstRun() : readNote();
  box.replaceChildren(...[
    ...notes(list),
    first,
    first ? null : head(list),
    ...groups(list),
  ].filter(Boolean));
  if (key) {
    const again = box.querySelector('[data-focus-key="' + CSS.escape(key) + '"]');
    if (again && !again.disabled) again.focus({ preventScroll: true });
    else if (key === FIRST_TITLE) pendingFocus = key; // once the read says it's empty
    else if (!box.contains(document.activeElement)) {
      // What had the keyboard is gone (a deleted account): the section's
      // tab has it.
      const nav = document.querySelector('.set-nav [aria-current="true"]');
      if (nav) nav.focus({ preventScroll: true });
    }
  }
}

// The first steps' heading: where the keyboard starts when Settings opens
// by itself on a new server, or after the last account was deleted.
const FIRST_TITLE = "first-run-title";
export function focusFirstSteps() { pendingFocus = FIRST_TITLE; }

// notes are what the owner should know first: credentials can't be
// managed behind the proxy alone; the master key needs fixing; where to
// set one.
function notes(list) {
  const out = [];
  if (proxy()) {
    out.push(el("div", { class: "set-note warn" },
      el("p", { text: "Publisher has no sign-in of its own here: the reverse proxy is the only gate, so accounts' credentials can't be managed in the app. They come from the server's environment. You can still rename accounts, turn them on or off, and choose which new posts pick." }),
      el("a", { href: "https://github.com/geofox/publisher#sign-in", target: "_blank", rel: "noopener noreferrer", text: "How to set up sign-in" })));
    return out;
  }
  const ks = keyStatus();
  if (ks === "missing" || ks === "mismatch") {
    out.push(el("div", { class: "set-note bad", role: "alert" },
      el("p", { class: "set-note-title", text: ks === "missing" ? "The master key is missing" : "The master key doesn't match" }),
      el("p", { text: inst.secrets.detail || "" })));
  } else if (ks === "none" && list.length) {
    const d = el("details", { class: "set-note", open: keyNoteOpen ? "" : false },
      el("summary", { text: "To add accounts here, set a master key" }),
      ...keyHelp());
    d.addEventListener("toggle", () => { keyNoteOpen = d.open; });
    out.push(d);
  }
  return out;
}

// ── first run ───────────────────────────────────────────────────────────

const SKIP_KEY = "publisher.masterKeySkipped";
function skipped() { try { return localStorage.getItem(SKIP_KEY) === "1"; } catch (_) { return false; } }
function setSkipped(on) { try { if (on) localStorage.setItem(SKIP_KEY, "1"); else localStorage.removeItem(SKIP_KEY); } catch (_) { /* storage off */ } }

// firstRun guides a new instance: a master key (skippable: accounts from
// the environment need none), then a first account.
function firstRun() {
  if (proxy()) return null;
  const ks = keyStatus();
  const keyStep = el("li", { class: "set-step" + (ks === "ok" ? " done" : "") },
    el("p", { class: "set-step-title", text: ks === "ok" ? "Master key set" : "Set a master key" }));
  if (ks === "ok") {
    keyStep.append(el("p", { class: "set-step-note", text: "Credentials you add here are encrypted with it." }));
  } else if (ks === "none" && skipped()) {
    keyStep.classList.add("skipped");
    keyStep.append(el("p", { class: "set-step-note" },
      "Skipped: add accounts in the server's environment (see the README), then restart it. ",
      el("button", { class: "ghost xs", type: "button", text: "Set one after all", "data-focus-key": "key-unskip",
        onclick: () => { setSkipped(false); pendingFocus = "key-skip"; render(); } })));
  } else if (ks === "none") {
    keyStep.append(...keyHelp(),
      el("button", { class: "ghost sm", type: "button", text: "Skip: I'll use the server's environment", "data-focus-key": "key-skip",
        onclick: () => { setSkipped(true); pendingFocus = "key-unskip"; render(); } }));
  } else {
    keyStep.append(el("p", { class: "set-step-note", text: "See above: it needs fixing before accounts can be added here." }));
  }
  const blocked = writeBlocked();
  const add = el("button", { class: "primary sm", type: "button", text: "Add an account", "data-focus-key": "acct-add-first",
    disabled: blocked ? "" : false, onclick: addAccount });
  const addStep = el("li", { class: "set-step" },
    el("p", { class: "set-step-title", text: "Add your first account" }),
    el("p", { class: "set-step-note", text: blocked || "Bluesky, Mastodon, Threads or Nostr: one network at a time, as many accounts as you like." }),
    add);
  // Where photos and videos go: the server's choice, in Settings, Media.
  const hasMedia = !!(inst.features && inst.features.media);
  const mediaStep = el("li", { class: "set-step" + (hasMedia ? " done" : "") },
    el("p", { class: "set-step-title", text: hasMedia ? "Media store set" : "Choose where photos and videos go" }),
    el("p", { class: "set-step-note", text: hasMedia ? "Posts can carry photos and videos."
      : "A Blossom server, or this server's disk. Without one, posts are text only." }),
    hasMedia ? null : el("button", { class: "ghost sm", type: "button", text: "Open Media", "data-focus-key": "media-first",
      onclick: () => document.dispatchEvent(new CustomEvent("open-settings", { detail: "media" })) }));
  return el("section", { class: "set-first", "aria-labelledby": "set-first-title" },
    el("h2", { id: "set-first-title", class: "set-first-title", text: "Get started", tabindex: "-1", "data-focus-key": FIRST_TITLE }),
    el("ol", { class: "set-steps" }, keyStep, addStep, mediaStep));
}

// The panel is drawn again on every read of the accounts: the key shown
// and what is open stay as they were.
let shownKey = "", keyNoteOpen = false, keyEnvOpen = false;

// newKey makes a master key in this browser: 32 random bytes, base64 (as
// `publisher keygen` prints). It is never sent anywhere.
function newKey() {
  const b = new Uint8Array(32);
  crypto.getRandomValues(b);
  return btoa(String.fromCharCode(...b));
}

// keyHelp shows a new master key and how to give it to Publisher.
function keyHelp() {
  const keyIn = el("input", { class: "tok-secret-input", readonly: "", "aria-label": "A new master key", spellcheck: "false" });
  const file = el("pre", { class: "auth-cmd" });
  const env = el("pre", { class: "auth-cmd" });
  const fill = fresh => {
    if (fresh || !shownKey) shownKey = newKey();
    const k = shownKey;
    keyIn.value = k;
    file.textContent = "printf '%s' '" + k + "' > publisher_master_key && chmod 600 publisher_master_key";
    env.textContent = "- PUBLISHER_MASTER_KEY=" + k;
  };
  fill();
  keyIn.addEventListener("click", () => keyIn.select());
  const copy = el("button", { class: "ghost sm", type: "button", text: "Copy" });
  copy.addEventListener("click", () => navigator.clipboard.writeText(keyIn.value).then(
    () => { copy.textContent = "Copied"; setTimeout(() => { copy.textContent = "Copy"; }, 1800); },
    () => { keyIn.select(); flash("Select it and copy it"); }));
  const again = el("button", { class: "ghost sm", type: "button", text: "Make another", onclick: () => fill(true) });
  const envMore = el("details", { class: "set-more", open: keyEnvOpen ? "" : false }, el("summary", { text: "Or as an environment variable" }), env);
  envMore.addEventListener("toggle", () => { keyEnvOpen = envMore.open; });
  return [
    el("p", { class: "set-step-note", text: "Accounts added here keep their credentials encrypted with a master key that only the server has. Here is a new one, made in this browser and sent nowhere:" }),
    el("div", { class: "tok-secret-row" }, keyIn, copy, again),
    el("p", { class: "set-step-note", text: "Keep a copy somewhere safe, such as a password manager: without it, the credentials in a backup can't be read. Then, beside your compose file, save it as a file:" }),
    file,
    el("p", { class: "set-step-note", text: "and give it to Publisher as a Docker secret (docker inspect doesn't show it):" }),
    el("p", { class: "set-step-note", text: "(Typed like this, the key is kept in your shell's history: clear it from there, or write the file with an editor.)" }),
    el("pre", { class: "auth-cmd", text: "services:\n  publisher:\n    environment:\n      - PUBLISHER_MASTER_KEY_FILE=/run/secrets/publisher_master_key\n    secrets: [publisher_master_key]\nsecrets:\n  publisher_master_key:\n    file: ./publisher_master_key" }),
    el("p", { class: "set-step-note", text: "Restart Publisher (docker compose up -d), then reload this page." }),
    envMore,
  ];
}

// ── the list ────────────────────────────────────────────────────────────

function head(list) {
  const blocked = writeBlocked();
  const add = el("button", { class: "primary sm", type: "button", text: "Add an account", "data-focus-key": "acct-add",
    disabled: blocked ? "" : false, onclick: addAccount });
  return el("div", { class: "sec-head set-head" },
    el("span", { class: "lbl", text: "accounts" }),
    el("span", { class: "muted", text: list.length ? list.length + (list.length === 1 ? " account" : " accounts") : "none yet" }),
    el("span", { class: "flex-spacer" }),
    proxy() ? null : add);
}

function groups(list) {
  const out = [];
  for (const a of list) {
    let g = out.find(x => x.type === a.type);
    if (!g) out.push(g = { type: a.type, accts: [] });
    g.accts.push(a);
  }
  return out.map(g => el("div", { class: "set-group p-" + g.type, role: "group", "aria-label": networkName(g.type) + " accounts" },
    el("div", { class: "set-group-head" }, brandTile(g.type, { size: 18, r: 5 }), el("span", { text: networkName(g.type) })),
    ...g.accts.map(card)));
}

const STATUS = {
  connected: ["Connected", "ok"],
  disabled: ["Off", "off"],
  locked: ["Locked", "warn"],
  needs_reconnect: ["Needs reconnecting", "bad"],
  not_configured: ["Not configured", "off"],
};

// dateText is a day ("7 Jan 2027").
const dateText = iso => new Date(iso).toLocaleDateString(undefined, { day: "numeric", month: "short", year: "numeric" });

// healthLine is what the last check found, or null.
function healthLine(a) {
  if (a.status !== "connected" && a.status !== "needs_reconnect") return null;
  const h = a.health;
  if (!h) return el("p", { class: "acct-health muted", text: "Not checked yet" });
  const when = h.checked_at ? "checked " + relTime(h.checked_at) : "";
  let text, cls;
  if (h.ok) {
    text = "Works" + (when ? ", " + when : "");
    if (h.expires_at) text += ". Its token runs until " + dateText(h.expires_at);
    cls = "ok";
  } else if (!h.reachable) {
    text = "Couldn't reach its server" + (when ? ", " + when : "") + (h.message ? ": " + h.message : "");
    cls = "warn";
  } else if (h.auth_ok === false) {
    text = networkName(a.type) + " refused its credentials" + (when ? ", " + when : "") + (h.message ? ": " + h.message : "");
    cls = "bad";
  } else {
    text = (h.message || "Not working") + (when ? ", " + when : "");
    cls = "warn";
  }
  return el("p", { class: "acct-health " + cls, text });
}

function card(a) {
  const title = customLabel(a) || networkName(a.type);
  const handle = handleOf(a);
  const [statusText, statusCls] = STATUS[a.status] || [a.status, "off"];
  const profile = a.profile_url && /^https?:\/\//i.test(a.profile_url) ? a.profile_url : "";
  const handleEl = !handle ? el("span", { class: "muted", text: "handle not known yet" })
    : profile ? el("a", { href: profile, target: "_blank", rel: "noopener noreferrer", text: handle, title: a.handle })
    : el("span", { text: handle, title: a.handle });
  return el("article", { class: "acct-card", "data-account": a.id, "aria-label": title + (handle ? ", " + handle : "") },
    el("div", { class: "acct-head" },
      accountTile(a, 36),
      el("div", { class: "acct-id" },
        el("div", { class: "acct-title" },
          el("span", { class: "acct-name", text: title }),
          el("span", { class: "acct-status " + statusCls, text: statusText })),
        el("div", { class: "acct-sub" }, handleEl,
          a.source === "env" ? el("span", { class: "acc-tag", text: "from the environment" }) : null),
        a.status !== "connected" && a.status_reason ? el("p", { class: "acct-reason", text: a.status_reason }) : null,
        healthLine(a))),
    flags(a),
    actions(a));
}

// ── flags ───────────────────────────────────────────────────────────────

function flags(a) {
  const out = [
    flag(a, "enabled", "On", "Off: the composer can't pick it, and nothing is sent to it."),
    flag(a, "default_selected", "Picked for new posts", "A new post starts with it selected."),
  ];
  if (inst.features && inst.features.public_feed) {
    out.push(flag(a, "public_feed", "In the public feed", "Its posts appear in the public feed (/v1/public/feed)."));
  }
  return el("div", { class: "acct-flags" }, ...out);
}

function flag(a, key, text, help) {
  const id = "flag-" + key + "-" + a.id;
  const box = el("input", { type: "checkbox", id, checked: a[key] ? "" : false, "data-focus-key": id, "aria-describedby": id + "-help" });
  let busy = false;
  box.addEventListener("change", async () => {
    if (busy) { box.checked = !box.checked; return; }
    busy = true;
    box.setAttribute("aria-busy", "true");
    pendingFocus = id;
    let saved = false;
    try {
      await v1("/v1/accounts/" + enc(a.id), { method: "PATCH", body: { [key]: box.checked } });
      saved = true;
    } catch (e) {
      box.checked = !box.checked;
      flash("Not changed: " + e.message);
    }
    try {
      if (saved) await reloadAccounts();
    } catch (e) {
      flash("Changed, but the list couldn't be read again: " + e.message, 4000);
    } finally {
      busy = false;
      box.removeAttribute("aria-busy");
    }
  });
  return el("span", { class: "acct-flag-item" },
    el("label", { class: "acct-flag", for: id, title: help }, box, el("span", { text })),
    el("span", { id: id + "-help", class: "sr-only", text: help }));
}

// ── actions ─────────────────────────────────────────────────────────────

function btn(text, key, onclick, more = {}) {
  return el("button", { class: "ghost sm", type: "button", text, "data-focus-key": key, "data-focus-fallback": "acct-add", ...more, onclick });
}

// revealable reports whether the account holds a credential its owner may
// see again (a Nostr key), saved here.
function revealable(a) {
  const m = manifest(a.type);
  if (!m) return false;
  return (m.auth || []).some(x => (x.fields || []).some(f => f.secret && f.revealable && a.credentials && a.credentials[f.key] && a.credentials[f.key].set));
}

function actions(a) {
  const name = nameOf(a.id);
  const db = a.source === "db", env = a.source === "env";
  const out = [];
  if (a.status === "connected" || a.status === "needs_reconnect") {
    const b = btn("Check now", "check-" + a.id, () => check(a, b), { "aria-label": "Check now: " + name });
    out.push(b);
  }
  out.push(btn("Rename", "rename-" + a.id, () => rename(a), { "aria-label": "Rename " + name }));
  if (db && !proxy()) {
    const reconnect = a.status === "needs_reconnect";
    out.push(btn(reconnect ? "Reconnect" : "Settings", "edit-" + a.id, () => editAccount(a), { "aria-label": (reconnect ? "Reconnect " : "Settings of ") + name }));
    if (revealable(a)) out.push(btn("Show key", "reveal-" + a.id, () => reveal(a, false), { "aria-label": "Show key: " + name }));
    out.push(btn("Delete", "delete-" + a.id, () => del(a), { class: "ghost sm danger-text", "aria-label": "Delete " + name }));
  }
  if (env) out.push(btn("Settings", "edit-" + a.id, () => envSettings(a), { "aria-label": "Settings of " + name }));
  // One whose variables are gone can go.
  if (env && a.status === "not_configured" && !proxy()) {
    out.push(btn("Delete", "delete-" + a.id, () => del(a), { class: "ghost sm danger-text", "aria-label": "Delete " + name }));
  }
  return el("div", { class: "acct-actions" }, ...out);
}

async function check(a, b) {
  if (b.disabled) return;
  b.disabled = true;
  b.textContent = "Checking…";
  pendingFocus = b.dataset.focusKey;
  try {
    const got = await v1("/v1/accounts/" + enc(a.id) + ":check", { method: "POST" });
    const h = got && got.health;
    flash(h && h.ok ? nameOf(a.id) + " works" : nameOf(a.id) + ": " + ((h && h.message) || "it doesn't work"), 4000);
    await reloadAccounts();
  } catch (e) {
    flash(e.code === "rate_limited" ? "Checked a moment ago. " + waitText(e.retryAfter) : "Check failed: " + e.message, 4000);
  } finally {
    if (b.isConnected) { b.disabled = false; b.textContent = "Check now"; if (pendingFocus === b.dataset.focusKey) { pendingFocus = ""; b.focus(); } }
  }
}

function rename(a) {
  inputDialog({
    title: "Rename " + nameOf(a.id), label: "What you call it here", value: a.label, maxlength: 64,
    onSubmit: async ([label], say) => {
      try {
        await v1("/v1/accounts/" + enc(a.id), { method: "PATCH", body: { label } });
        await reloadAccounts();
        return true;
      } catch (e) { say(e.message); return false; }
    },
  });
}

function del(a) {
  const name = nameOf(a.id);
  confirmModal({
    title: "Delete " + name + "?",
    body: a.source === "env"
      ? "Its variables aren't set any more. Publisher forgets it and cancels what it still had to send to it. Its sent posts stay in History. Set its variables again and it comes back as a new account."
      : "Publisher erases its credentials and cancels what it still had to send to it. Its sent posts stay in History. To post as it again, add it again.",
    confirmText: "Delete",
    onConfirm: async () => {
      try {
        await v1("/v1/accounts/" + enc(a.id), { method: "DELETE" });
        flash("Deleted " + name);
        // The keyboard goes to Add an account (the first steps when it was
        // the last).
        pendingFocus = accounts().length <= 1 ? FIRST_TITLE : "acct-add";
        await reloadAccounts().catch(() => {});
        return true;
      } catch (e) {
        if (e.cancelled) { flash("Not deleted: you didn't confirm it's you."); return true; }
        flash("Not deleted: " + e.message);
        return false;
      }
    },
  });
}

// ── dialogs ─────────────────────────────────────────────────────────────

// sheet is a dialog whose body the caller fills, step by step; show()
// opens it, focusing focusEl. onClose runs when it closes (✕ or close()).
function sheet(title, { escape = true, onClose = null, closable = true } = {}) {
  const bk = el("div", { class: "modal-bk" });
  let close = () => bk.remove();
  const titleEl = el("p", { class: "modal-title", text: title });
  const body = el("div", { class: "acct-dlg-body" });
  const s = {
    body,
    setTitle: t => { titleEl.textContent = t; },
    show: focusEl => { bk.append(card); document.body.append(bk); close = asDialog(bk, card, close, focusEl, { escape }); },
    close: () => { if (onClose) onClose(); close(); },
    open: () => bk.isConnected,
  };
  const x = el("button", { class: "modal-x", type: "button", text: "✕", "aria-label": "Close", onclick: () => s.close() });
  const card = el("div", { class: "modal acct-dlg" }, closable ? x : null, titleEl, body);
  return s;
}

// controls builds the fields of a form, recording each under the JSON
// pointer the server's errors name it by.
function controls(specs, prefix, values, byPointer, opts = () => ({})) {
  return (specs || []).map(f => {
    const c = field(f, values ? values[f.key] : undefined, opts(f));
    byPointer.set(prefix + f.key, c);
    return c;
  });
}

// collect reads the values the owner gave (the ones left empty are left
// out).
function collect(ctrls) {
  const out = {};
  for (const c of ctrls) {
    if (c.locked) continue;
    const v = c.read();
    if (v !== undefined) out[c.spec.key] = v;
  }
  return out;
}

// problemText is what a failed write says, beyond the errors shown on the
// fields.
function problemText(e, m, byPointer) {
  for (const c of byPointer.values()) c.setError(""); // the last try's
  switch (e.code) {
    case "duplicate_account": {
      const other = e.problem && e.problem.account_id && byId(e.problem.account_id);
      return "This account is already here" + (other ? " (" + nameOf(other.id) + ")" : "") + ".";
    }
    case "identity_changed":
      return "These credentials are for another " + m.name + " account: add it as a new account instead.";
    case "credentials_rejected": {
      const said = ((e.problem && e.problem.errors) || []).map(x => x.detail).filter(Boolean).join(" ");
      return m.name + " refused these credentials" + (said ? ": " + said : ".");
    }
    case "rate_limited":
      return "Too many changes in a minute. " + waitText(e.retryAfter);
  }
  return showProblems(e, byPointer).join(" ");
}

// ── adding an account ───────────────────────────────────────────────────

async function addAccount() {
  const why = writeBlocked();
  if (why) { flash(why); return; }
  if (!types) await readTypes();
  if (!types) { flash("Couldn't read the networks this server has: " + typesError); return; }
  const s = sheet("Add an account");
  const first = pickNetwork(s);
  s.show(first);
}

// pickNetwork asks which network; back from a later step, the one picked
// (from) has the focus again.
function pickNetwork(s, from = "") {
  s.setTitle("Add an account");
  const list = types.filter(m => usable(m).length);
  const buttons = list.map(m => el("button", { class: "acct-pick-btn", type: "button", "data-type": m.type, onclick: () => pickMethod(s, m) },
    brandTile(m.type, { size: 28 }), el("span", { text: m.name })));
  s.body.replaceChildren(
    el("p", { class: "acct-dlg-lead", text: "Which network?" }),
    el("div", { class: "acct-pick" }, ...buttons));
  const focus = buttons.find(b => b.dataset.type === from) || buttons[0];
  if (s.open() && focus) focus.focus();
  return focus;
}

function pickMethod(s, m, from = "") {
  const ms = usable(m);
  if (ms.length === 1) return form(s, m, ms[0], () => pickNetwork(s, m.type));
  s.setTitle("Add a " + m.name + " account");
  const buttons = ms.map(a => el("button", { class: "acct-method", type: "button", "data-method": a.id, onclick: () => form(s, m, a, () => pickMethod(s, m, a.id)) },
    el("span", { class: "acct-method-name", text: a.label }),
    a.help ? el("span", { class: "acct-method-help", text: a.help }) : null));
  s.body.replaceChildren(
    el("p", { class: "acct-dlg-lead", text: "How does it connect?" }),
    el("div", { class: "acct-methods" }, ...buttons),
    el("div", { class: "confirm-actions" }, el("button", { class: "ghost sm", type: "button", text: "Back", onclick: () => pickNetwork(s, m.type) })));
  const focus = buttons.find(b => b.dataset.method === from) || buttons[0];
  focus.focus();
  return focus;
}

// howTo is a way of connecting's help and where to get the credential.
function howTo(m, a) {
  if (!a.help && !a.help_url) return null;
  return el("div", { class: "acct-how" },
    a.help ? el("p", { text: a.help }) : null,
    a.help_url && /^https:\/\//i.test(a.help_url) ? el("a", { href: a.help_url, target: "_blank", rel: "noopener noreferrer", text: "Open it on " + m.name }) : null);
}

function form(s, m, a, back) {
  s.setTitle("Add a " + m.name + " account");
  const byPointer = new Map();
  const creds = controls(a.fields, "/credentials/", null, byPointer, f => ({ warn: warnFor(m.type, f.key) }));
  const conf = controls(m.config, "/config/", null, byPointer);
  const label = field({ key: "label", label: "Label", type: "string", max_length: 64, placeholder: m.name,
    help: "What you call it here. When left empty: " + m.name + "." }, undefined);
  byPointer.set("/label", label);
  const status = el("p", { class: "acct-dlg-status", role: "status", "aria-live": "polite" });
  const submit = el("button", { class: "primary sm", type: "submit", text: a.flow === "generate" ? "Make the key and add it" : "Add the account" });
  const f = el("form", { class: "acct-form", novalidate: "" },
    howTo(m, a),
    ...fieldSet([...conf, ...creds, label]),
    status,
    el("div", { class: "confirm-actions" }, el("button", { class: "ghost sm", type: "button", text: "Back", onclick: back }), submit));
  let busy = false;
  f.addEventListener("submit", async ev => {
    ev.preventDefault();
    if (busy) return;
    if (badInputs([...conf, ...creds])) { status.className = "acct-dlg-status err"; status.textContent = "Fix the field marked above."; return; }
    busy = true;
    submit.disabled = true;
    status.className = "acct-dlg-status";
    status.textContent = a.flow === "generate" ? "Adding it…" : "Checking with " + m.name + "…";
    const body = { type: m.type, method: a.id, config: collect(conf), credentials: collect(creds) };
    const l = label.read();
    if (l) body.label = l;
    try {
      const acct = await v1("/v1/accounts", { method: "POST", body });
      s.close();
      flash("Added " + (acct.handle ? handleOf(acct) : customLabel(acct) || m.name));
      pendingFocus = "edit-" + acct.id;
      await reloadAccounts().catch(() => {});
      if (a.flow === "generate") reveal(acct, true);
    } catch (e) {
      status.textContent = problemText(e, m, byPointer);
      status.className = "acct-dlg-status err";
      if (e.code === "credentials_rejected" && creds.length) creds[creds.length - 1].focus();
    } finally {
      busy = false;
      submit.disabled = false;
    }
  });
  s.body.replaceChildren(f);
  const first = conf.find(c => !c.spec.advanced) || creds[0] || label;
  if (s.open()) first.focus();
  return first.input;
}

// ── an account's settings ───────────────────────────────────────────────

// current is a config field's value as the server applies it: the saved
// one, else the default.
const current = (a, spec) => a.config && a.config[spec.key] !== undefined ? a.config[spec.key] : spec.default;

async function editAccount(a) {
  if (!types) await readTypes();
  const m = manifest(a.type);
  if (!m) { flash("Couldn't read how " + networkName(a.type) + " accounts connect" + (typesError ? ": " + typesError : "")); return; }
  const s = sheet("Settings of " + nameOf(a.id));
  const ways = (m.auth || []).filter(x => x.flow === "fields");
  let method = (m.auth || []).find(x => x.id === a.auth_method) || ways[0];
  if (!method) { flash("This account's way of connecting isn't offered here."); return; }
  const byPointer = new Map();
  let creds = [];
  const credBox = el("div", { class: "acct-creds" });
  const status = el("p", { class: "acct-dlg-status", role: "status", "aria-live": "polite" });
  const drawCreds = () => {
    for (const k of [...byPointer.keys()]) if (k.startsWith("/credentials/")) byPointer.delete(k);
    const same = method.id === a.auth_method;
    creds = controls(method.fields, "/credentials/", same ? a.config : null, byPointer, f => ({
      keep: same && f.secret && !!(a.credentials && a.credentials[f.key] && a.credentials[f.key].set),
      warn: warnFor(a.type, f.key),
    }));
    credBox.replaceChildren(
      method.flow === "generate"
        ? el("p", { class: "fld-help", text: "Its key was made here." + (revealable(a) ? " Show key shows it." : "") })
        : el("p", { class: "fld-help", text: same ? "Leave the saved ones empty to keep them; type new ones to replace them." : "A new way of connecting needs its credentials." }),
      howTo(m, method) || "",
      ...creds.map(c => c.row));
  };
  drawCreds();
  let pick = null;
  if (method.flow === "fields" && ways.length > 1) {
    const id = "acct-way-" + a.id;
    pick = el("select", { id }, ...ways.map(x => el("option", { value: x.id, text: x.label, selected: x.id === method.id ? "" : false })));
    pick.addEventListener("change", () => { method = ways.find(x => x.id === pick.value) || method; drawCreds(); });
    byPointer.set("/method", { setError: t => { status.textContent = t; }, focus: () => pick.focus(), row: null, spec: {} });
    pick = el("div", { class: "fld" }, el("label", { for: id, class: "fld-label", text: "How it connects" }), pick);
  }
  const conf = controls(m.config, "/config/", a.config, byPointer);
  const submit = el("button", { class: "primary sm", type: "submit", text: "Save" });
  const f = el("form", { class: "acct-form", novalidate: "" },
    pick,
    el("p", { class: "acct-sect", text: "Credentials" }), credBox,
    conf.length ? el("p", { class: "acct-sect", text: "Settings" }) : null,
    ...fieldSet(conf),
    status,
    el("div", { class: "confirm-actions" }, el("button", { class: "ghost sm", type: "button", text: "Cancel", onclick: () => s.close() }), submit));
  let busy = false;
  f.addEventListener("submit", async ev => {
    ev.preventDefault();
    if (busy) return;
    if (badInputs([...conf, ...creds])) { status.className = "acct-dlg-status err"; status.textContent = "Fix the field marked above."; return; }
    const body = {};
    if (method.id !== a.auth_method) body.method = method.id;
    const typedSecret = creds.some(c => c.spec.secret && c.read() !== undefined);
    const changedPlain = creds.some(c => !c.spec.secret && !sameValue(c.spec, c.read(), a.config ? a.config[c.spec.key] : undefined));
    if (body.method || typedSecret || changedPlain) body.credentials = collect(creds);
    const cfg = {};
    for (const c of conf) {
      const v = c.read();
      if (!sameValue(c.spec, v, current(a, c.spec))) cfg[c.spec.key] = v === undefined ? null : v;
    }
    if (Object.keys(cfg).length) body.config = cfg;
    if (!Object.keys(body).length) { s.close(); flash("Nothing changed"); return; }
    busy = true;
    submit.disabled = true;
    status.className = "acct-dlg-status";
    status.textContent = "Checking with " + m.name + "…";
    try {
      await v1("/v1/accounts/" + enc(a.id), { method: "PATCH", body });
      s.close();
      flash("Saved");
      await reloadAccounts().catch(() => {});
    } catch (e) {
      let text = problemText(e, m, byPointer);
      if (e.code === "validation_failed" && !body.credentials && e.problem && (e.problem.errors || []).some(x => x.pointer === "/credentials")) {
        text = "A new server address needs the credentials again: type them above.";
        const secret = creds.find(c => c.spec.secret);
        if (secret) secret.focus();
      }
      status.textContent = text;
      status.className = "acct-dlg-status err";
    } finally {
      busy = false;
      submit.disabled = false;
    }
  });
  s.body.replaceChildren(f);
  s.show(pick ? pick.querySelector("select") : (creds[0] || conf[0] || { input: submit }).input);
}

// envSettings shows an environment account's settings, which the
// environment sets, and offers to move it into the app.
async function envSettings(a) {
  if (!types) await readTypes();
  const m = manifest(a.type);
  const s = sheet("Settings of " + nameOf(a.id));
  const method = m && (m.auth || []).find(x => x.id === a.auth_method);
  const gone = a.status === "not_configured";
  const parts = [el("p", { class: "fld-help", text: gone
    ? "This account came from the server's environment, which doesn't set it any more, so it isn't used. Set its variables again and restart Publisher, or delete it here (its sent posts stay in History)."
    : "This account comes from the server's environment, which sets everything below." })];
  if (method) {
    parts.push(el("p", { class: "acct-sect", text: "Credentials: " + method.label }));
    for (const f of method.fields || []) {
      if (f.secret) {
        const set = a.credentials && a.credentials[f.key] && a.credentials[f.key].set;
        parts.push(el("div", { class: "fld" }, el("span", { class: "fld-label", text: f.label }),
          el("p", { class: "fld-help", text: set ? "Set in the environment." : "Not set." })));
      } else parts.push(field(f, a.config ? a.config[f.key] : undefined, { locked: true }).row);
    }
  }
  if (m && (m.config || []).length) {
    parts.push(el("p", { class: "acct-sect", text: "Settings" }));
    parts.push(...fieldSet(controls(m.config, "/config/", a.config || {}, new Map(), () => ({ locked: true }))));
  }
  const done = el("button", { class: "ghost sm", type: "button", text: "Close", onclick: () => s.close() });
  const acts = [done];
  if (gone && !proxy()) {
    acts.push(el("button", { class: "ghost sm danger-text", type: "button", text: "Delete", onclick: () => { s.close(); del(a); } }));
  } else if (!proxy()) {
    const blocked = writeBlocked();
    parts.push(el("p", { class: "acct-sect", text: "Manage it here" }),
      el("p", { class: "fld-help", text: "Move it into the app to change these here: Publisher copies its settings and credentials into its database, encrypted with the master key, and ignores its variables from then on (remove them from the environment)." }),
      blocked ? el("p", { class: "fld-warn", text: blocked }) : null);
    acts.push(el("button", { class: "primary sm", type: "button", text: "Move into the app", disabled: blocked ? "" : false,
      onclick: () => adopt(a, s) }));
  }
  s.body.replaceChildren(...parts.filter(Boolean), el("div", { class: "confirm-actions" }, ...acts));
  s.show(done);
}

function adopt(a, s) {
  const name = nameOf(a.id);
  confirmModal({
    title: "Move " + name + " into the app?",
    body: "Its settings and credentials are copied into Publisher's database, encrypted with the master key; its history stays. From then on its variables are ignored: remove them from the environment (Publisher warns at startup while they're set).",
    confirmText: "Move it",
    onConfirm: async () => {
      try {
        await v1("/v1/accounts/" + enc(a.id) + ":adopt", { method: "POST" });
        flash("Moved " + name + " into the app. Remove its variables from the environment.", 5000);
        s.close();
        await reloadAccounts().catch(() => {});
        return true;
      } catch (e) {
        flash("Not moved: " + e.message, 5000);
        return false;
      }
    },
  });
}

// ── showing a key ───────────────────────────────────────────────────────

// reveal shows an account's key, once the owner confirmed it's them (the
// server asks). fresh: just made, so the owner keeps a copy now.
async function reveal(a, fresh) {
  let got;
  try {
    got = await v1("/v1/accounts/" + enc(a.id) + ":reveal", { method: "POST" });
  } catch (e) {
    if (e.cancelled) flash(fresh ? "Added. To keep a copy of its key, use Show key in Settings." : "Not shown: you didn't confirm it's you.", 5000);
    else flash("Couldn't show the key: " + e.message, 5000);
    return;
  }
  const values = Object.entries((got && got.credentials) || {});
  if (!values.length) { flash("It has no key to show."); return; }
  const m = manifest(a.type);
  const specOf = k => m && (m.auth || []).flatMap(x => x.fields || []).find(f => f.key === k);
  const inputs = [];
  // Closing (Done or ✕: it can be shown again) clears the key from the page.
  // Just made, it closes only once the owner says they kept a copy.
  const s = sheet(fresh ? "Keep a copy of the new key" : "The key of " + nameOf(a.id), { escape: false, closable: !fresh,
    onClose: () => { for (const i of inputs) i.value = ""; } });
  const rows = values.map(([k, v]) => {
    const spec = specOf(k);
    const input = el("input", { class: "tok-secret-input", readonly: "", value: v, "aria-label": (spec && spec.label) || k, spellcheck: "false" });
    input.addEventListener("click", () => input.select());
    inputs.push(input);
    const copy = el("button", { class: "ghost sm", type: "button", text: "Copy" });
    copy.addEventListener("click", () => navigator.clipboard.writeText(v).then(
      () => { copy.textContent = "Copied"; setTimeout(() => { copy.textContent = "Copy"; }, 1800); },
      () => { input.select(); flash("Select it and copy it"); }));
    return el("div", { class: "fld" }, el("span", { class: "fld-label", text: (spec && spec.label) || k }), el("div", { class: "tok-secret-row" }, input, copy));
  });
  const done = el("button", { class: "primary sm", type: "button", text: "Done" });
  const parts = [
    el("p", { class: "tok-warning", text: "Anyone with this key can post as this account. Keep it somewhere safe, such as a password manager, and never paste it into a website." }),
    ...rows,
  ];
  if (a.handle) parts.push(el("p", { class: "fld-help", text: "Its public key: " + a.handle }));
  if (fresh) {
    const id = "key-saved-" + a.id;
    const box = el("input", { type: "checkbox", id });
    done.disabled = true;
    box.addEventListener("change", () => { done.disabled = !box.checked; });
    parts.push(el("div", { class: "fld-check" }, box, el("label", { for: id, text: "I've saved it somewhere safe" })));
  }
  done.addEventListener("click", () => s.close());
  s.body.replaceChildren(...parts, el("div", { class: "confirm-actions" }, done));
  s.show(inputs[0]);
  setTimeout(() => inputs[0] && inputs[0].select(), 60);
}
