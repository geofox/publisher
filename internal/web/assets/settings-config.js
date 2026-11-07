"use strict";
import { el, v1, flash, relTime, confirmModal } from "./common.js";
import { field, showProblems, sameValue, badInputs } from "./fields.js";

// Settings forms for the settings API: the owner's (/v1/settings) and the
// server's (/v1/instance/settings). A form shows some of a scope's
// settings, each built from its field, with where its value comes from:
// saved in the app, the server's environment (then locked, with "Copy
// into the app"), or the default. Saving sends only what changed; the
// server checks everything and says what's wrong, field by field.

const PATH = { owner: "/v1/settings", instance: "/v1/instance/settings" };
const enc = encodeURIComponent;

// read is each scope's last read: { body, etag } or { error }.
const read = { owner: null, instance: null };
const forms = { owner: new Set(), instance: new Set() };

// readSettings reads a scope again and redraws its forms.
export async function readSettings(scope) {
  let etag = "";
  try {
    const body = await v1(PATH[scope], { onResponse: r => { etag = r.headers.get("ETag") || ""; } });
    read[scope] = { body, etag };
  } catch (e) {
    read[scope] = { error: e };
  }
  for (const f of forms[scope]) f.draw();
  return read[scope];
}

// leafOf is a setting as last read, or null.
export function leafOf(scope, key) {
  const r = read[scope];
  if (!r || !r.body) return null;
  const [g, n] = key.split(".");
  return (r.body[g] && r.body[g][n]) || null;
}

// What a test's failure class means, in the owner's words.
const TEST_FAIL = {
  unreachable: "couldn't be reached",
  refused: "refused the connection",
  timeout: "didn't answer in time",
  http_error: "answered, but not with success",
  blocked_destination: "is an address Publisher may not reach (a private or local one: see PRIVATE_DESTINATIONS in the README)",
  wrong_answer: "answered, but not with the test's file: is /media/ sent to this server, past any sign-in?",
  store_refused: "is this server's disk, which didn't take the test's file (is there room? can it write to MEDIA_DIR?)",
};

const code = s => el("code", { text: s });

// sourceLine says where a setting's value comes from.
function sourceLine(scope, key, L, onCopied) {
  const p = el("p", { class: "fld-src" });
  if (L.source === "env") {
    p.append("Set by ", code(L.env), " in the server's environment. ");
    const b = el("button", { class: "ghost xs", type: "button", text: "Copy into the app",
      title: "Save this value in the app, so the variable can be removed from the environment" });
    b.addEventListener("click", async () => {
      b.disabled = true;
      try {
        await v1(PATH[scope] + "/" + enc(key) + ":copy-from-env", { method: "POST" });
        onCopied("Copied. You can now remove " + L.env + " from the server's environment: the app keeps this value.");
      } catch (e) {
        onCopied("", e.message);
      } finally {
        b.disabled = false;
      }
    });
    p.append(b);
  } else if (L.source === "saved") {
    p.append("Saved in the app" + (L.updated_at ? ", " + relTime(L.updated_at) : "") + ".");
    if (L.env) p.append(" ", code(L.env), " in the server's environment would replace it.");
  } else if (L.field && L.field.secret) {
    if (!L.env) return null;
    p.append("The server's environment can set it with ", code(L.env), ".");
  } else {
    p.append("The default.");
    if (L.env) p.append(" The server's environment can set it with ", code(L.env), ".");
  }
  return p;
}

// secretControl is a secret setting: whether it is set (never its value),
// typing a new one, clearing it, and making one when the server can.
function secretControl(scope, key, L, say) {
  const spec = { ...L.field, key };
  const sv = L.value || {};
  const c = field(spec, undefined, { locked: L.locked, keep: !!sv.set && !L.locked });
  const help = c.row.querySelector(".fld-locked");
  if (help) help.remove();
  const state = el("p", { class: "fld-state" });
  if (sv.set) {
    state.append(el("span", { class: "fld-set", text: "Set" }));
    if (sv.origin) state.append(" for ", code(sv.origin));
    if (sv.updated_at && L.source === "saved") state.append(", saved " + relTime(sv.updated_at));
    state.append(".");
  } else {
    state.append(el("span", { class: "fld-unset", text: "Not set." }));
  }
  if (L.problem) state.append(" ", el("span", { class: "bad", text: "Saved, but it " + L.problem + "." }));
  const tools = el("div", { class: "fld-tools" });
  if (sv.public_key) {
    const pub = el("input", { class: "tok-secret-input", readonly: "", value: sv.public_key, "aria-label": "Its public key",
      spellcheck: "false" });
    pub.addEventListener("click", () => pub.select());
    const copy = el("button", { class: "ghost xs", type: "button", text: "Copy" });
    copy.addEventListener("click", () => navigator.clipboard.writeText(sv.public_key).then(
      () => flash("Public key copied"), () => { pub.select(); flash("Select it and copy it"); }));
    c.row.insertBefore(el("div", { class: "fld-pub" },
      el("p", { class: "fld-help", text: "Its public key, to allow on the server:" }),
      el("div", { class: "tok-secret-row" }, pub, copy)), c.row.querySelector(".fld-help"));
  }
  if (!L.locked && L.generate) {
    tools.append(el("button", { class: "ghost xs", type: "button", text: sv.set ? "Make a new one" : "Make one",
      onclick: e => generate(scope, key, L, e.target, say) }));
  }
  if (!L.locked && sv.set) {
    tools.append(el("button", { class: "ghost xs danger-text", type: "button", text: "Clear",
      onclick: () => confirmModal({ title: "Clear " + spec.label.toLowerCase() + "?",
        body: L.env ? "Without it, " + L.env + " applies if it is set in the server's environment, else nothing." : "",
        confirmText: "Clear", onConfirm: () => save(scope, { [key]: null }, say).then(ok => ok) }) }));
  }
  c.row.insertBefore(state, c.row.firstChild.nextSibling);
  if (tools.childNodes.length) c.tools = tools;
  return c;
}

// generate makes a new value on the server and shows it this once (a key
// shows its public half).
async function generate(scope, key, L, b, say) {
  const again = L.value && L.value.set;
  const go = async () => {
    b.disabled = true;
    try {
      const r = await v1(PATH[scope] + "/" + enc(key) + ":generate", { method: "POST" });
      // A key shows its public half for good; a token is shown this once.
      const pub = L.field && L.field.type === "string" && /^npub1/.test(r.value || "");
      if (!pub) shown.set(key, r.value);
      await readSettings(scope);
      say(pub ? "A new key is set: allow its public key on the server." : "Made: copy it now, it isn't shown again.");
      return true;
    } catch (e) {
      say("", e.message);
      return false;
    } finally {
      b.disabled = false;
    }
  };
  if (!again) { await go(); return; }
  confirmModal({ title: "Make a new one?", body: "The one set now stops working at once, wherever it is used.",
    confirmText: "Make a new one", onConfirm: go });
}

// shown holds a value just made, by key, to show once.
const shown = new Map();

function shownOnce(key) {
  const v = shown.get(key);
  if (!v) return null;
  shown.delete(key);
  const input = el("input", { class: "tok-secret-input", readonly: "", value: v, "aria-label": "The new value", spellcheck: "false" });
  input.addEventListener("click", () => input.select());
  const copy = el("button", { class: "ghost xs", type: "button", text: "Copy" });
  copy.addEventListener("click", () => navigator.clipboard.writeText(v).then(
    () => flash("Copied"), () => { input.select(); flash("Select it and copy it"); }));
  return el("div", { class: "set-note" },
    el("p", { text: "Here it is, shown this once:" }),
    el("div", { class: "tok-secret-row" }, input, copy));
}

// save sends values ({ "group.name": value }) to a scope; null puts a
// setting back to its default. It answers true when saved; say gets what
// to tell the owner.
async function save(scope, values, say, byPointer = null) {
  const body = {};
  for (const [k, v] of Object.entries(values)) {
    const [g, n] = k.split(".");
    (body[g] = body[g] || {})[n] = v;
  }
  const r = read[scope];
  const headers = r && r.etag ? { "If-Match": r.etag } : {};
  let etag = "";
  try {
    const next = await v1(PATH[scope], { method: "PATCH", body, headers, onResponse: x => { etag = x.headers.get("ETag") || ""; } });
    read[scope] = { body: next, etag };
    for (const f of forms[scope]) f.draw();
    document.dispatchEvent(new CustomEvent("settings-saved", { detail: { scope, body: next } }));
    say("Saved.");
    return true;
  } catch (e) {
    if (e.status === 412 || e.code === "locked_by_env") {
      await readSettings(scope);
      say("", e.status === 412 ? "These settings were changed elsewhere meanwhile: here they are as saved now. Make your change again."
        : e.message);
      return false;
    }
    if (e.status === 429) { say("", e.message + (e.retryAfter ? " (in " + e.retryAfter + " s)" : "")); return false; }
    const rest = byPointer ? showProblems(e, byPointer) : [e.message];
    say("", rest.join(" "));
    return false;
  }
}

// settingsForm is a form for keys ("group.name") of scope. opts:
//   title, intro: what heads it (intro: nodes or a string);
//   shows: { key: get => bool }, whether a setting shows now (get(key) is
//     a setting's value as the form holds it);
//   test: { group, label, what }: a button that tests the saved settings;
//   extra: (get) => nodes, drawn after the fields (warnings);
//   readOnly: why the form can't be saved (behind the proxy alone).
// It answers { node, draw }.
export function settingsForm(scope, keys, opts = {}) {
  const node = el("section", { class: "set-sect" });
  const f = { node, draw };
  forms[scope].add(f);
  let status = null;
  const say = (ok, bad) => {
    if (!status) return;
    status.className = "set-status" + (bad ? " bad" : "");
    status.textContent = bad || ok || "";
  };
  function draw() {
    const r = read[scope];
    const head = [];
    if (opts.title) head.push(el("h2", { class: "set-sect-title", text: opts.title }));
    if (opts.intro) head.push(typeof opts.intro === "string" ? el("p", { class: "set-sect-intro", text: opts.intro }) : opts.intro);
    if (!r) { node.replaceChildren(...head, el("p", { class: "set-reading", text: "Reading the settings…" })); return; }
    if (r.error) {
      const e = r.error;
      const msg = e.code === "sign_in_required"
        ? "Behind the reverse proxy alone, the server's settings can't be seen or changed in the app: set up sign-in first."
        : "The settings couldn't be read: " + e.message;
      node.replaceChildren(...head, el("div", { class: "set-note" + (e.code === "sign_in_required" ? " warn" : " bad") },
        el("p", { text: msg }),
        e.code === "sign_in_required" ? null : el("button", { class: "ghost sm", type: "button", text: "Try again", onclick: () => readSettings(scope) })));
      return;
    }
    const controls = new Map(), byPointer = new Map(), notes = [];
    const msgs = [];
    const copied = (ok, bad) => { readSettings(scope).then(() => say(ok, bad)); };
    for (const key of keys) {
      const L = leafOf(scope, key);
      if (!L) continue;
      const spec = { ...L.field, key };
      let c;
      if (spec.secret) {
        c = secretControl(scope, key, L, (ok, bad) => say(ok, bad));
      } else {
        c = field(spec, L.value, { locked: L.locked, optional: !spec.required && spec.default == null });
        const lockedNote = c.row.querySelector(".fld-locked");
        if (lockedNote) lockedNote.remove();
        const help = c.row.querySelector(".fld-help");
        if (help && !help.childNodes.length) help.hidden = true;
      }
      c.leaf = L;
      const src = sourceLine(scope, key, L, copied);
      if (src) c.row.append(src);
      if (c.tools) c.row.append(c.tools);
      const once = shownOnce(key);
      if (once) c.row.append(once);
      controls.set(key, c);
      byPointer.set("/" + key.replace(".", "/"), c);
    }
    const get = key => {
      const c = controls.get(key);
      if (!c) { const L = leafOf(scope, key); return L ? L.value : undefined; }
      if (c.spec.secret) return c.leaf.value;
      return c.locked ? c.leaf.value : c.read();
    };
    const extra = el("div", { class: "set-extra" });
    const refresh = () => {
      if (opts.shows) for (const [k, show] of Object.entries(opts.shows)) {
        const c = controls.get(k);
        if (c) c.row.hidden = !show(get);
      }
      if (opts.extra) extra.replaceChildren(...[].concat(opts.extra(get) || []).filter(Boolean));
    };
    for (const c of controls.values()) {
      c.input.addEventListener("input", refresh);
      c.input.addEventListener("change", refresh);
    }
    status = el("p", { class: "set-status", role: "status", "aria-live": "polite" });
    const changes = () => {
      const out = {};
      for (const [key, c] of controls) {
        if (c.locked || c.row.hidden) continue;
        const v = c.read();
        if (c.spec.secret) { if (v !== undefined) out[key] = v; continue; }
        if (!sameValue(c.spec, v, c.leaf.value)) out[key] = v === undefined ? null : v;
      }
      return out;
    };
    const saveBtn = el("button", { class: "primary sm", type: "submit", text: "Save" });
    const form = el("form", { class: "set-form", novalidate: "" });
    form.addEventListener("submit", async ev => {
      ev.preventDefault();
      if (saveBtn.disabled) return;
      if (badInputs([...controls.values()])) return;
      const ch = changes();
      if (!Object.keys(ch).length) { say("Nothing changed."); return; }
      saveBtn.disabled = true;
      saveBtn.textContent = ch["media.public"] === true ? "Checking…" : "Saving…";
      let ok = false;
      try {
        ok = await save(scope, ch, say, byPointer);
      } finally {
        saveBtn.disabled = false;
        saveBtn.textContent = "Save";
      }
      // Saved, the form is drawn again: the keyboard stays on Save.
      if (ok) { const again = node.querySelector(".set-actions .primary"); if (again) again.focus({ preventScroll: true }); }
    });
    const row = el("div", { class: "set-actions" });
    if (!opts.readOnly) row.append(saveBtn);
    if (opts.test) {
      const t = el("button", { class: "ghost sm", type: "button", text: opts.test.label || "Send a test" });
      t.addEventListener("click", async () => {
        if (Object.keys(changes()).length) { say("", "Save first: the test uses what is saved."); return; }
        t.disabled = true;
        say("Testing…");
        try {
          const res = await v1(PATH[scope] + "/" + enc(opts.test.group) + ":test", { method: "POST" });
          if (res.ok) say((opts.test.what || "It") + " worked.");
          else say("", (opts.test.what || "It") + " " + (TEST_FAIL[res.error] || "failed") + ".");
        } catch (e) {
          say("", e.code === "invalid_state" ? "Nothing is set to test: set its address and save first." : e.message);
        } finally {
          t.disabled = false;
        }
      });
      row.append(t);
    }
    if (opts.readOnly) notes.push(el("p", { class: "set-status", text: opts.readOnly }));
    form.append(...[...controls.values()].map(c => c.row), extra, ...notes, row, status);
    node.replaceChildren(...head, form, ...msgs);
    refresh();
  }
  draw();
  return f;
}
