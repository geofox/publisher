"use strict";
import { el, v1, flash, relTime, confirmModal, restoreFocus } from "./common.js";
import { field, showProblems, sameValue, badInputs } from "./fields.js";
import { state } from "./state.js";

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

// readSettings reads a scope again and redraws its forms. What the owner
// typed and hasn't saved stays (see settingsForm), except in the keys of
// saved (just saved: what is saved now shows).
export async function readSettings(scope, saved = []) {
  let etag = "";
  try {
    const body = await v1(PATH[scope], { onResponse: r => { etag = r.headers.get("ETag") || ""; } });
    read[scope] = { body, etag };
  } catch (e) {
    read[scope] = { error: e };
  }
  for (const f of forms[scope]) f.draw(new Set(saved));
  return read[scope];
}

// lcFirst is a label inside a sentence ("Public feed token" → "public
// feed token"), names kept ("Blossom signing key").
const lcFirst = s => /^[A-Z][a-z]/.test(s) && !/^(Blossom|DeepL|Nostr|Mastodon|Threads|Bluesky)\b/.test(s) ? s[0].toLowerCase() + s.slice(1) : s;

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
function sourceLine(scope, key, L, onCopied, fallbackFocus) {
  const p = el("p", { class: "fld-src", id: "src-" + key.replace(/\W/g, "-") });
  if (L.source === "env" && L.also_saved) {
    p.append("Set by ", code(L.env), " in the server's environment, and also saved in the app: remove ", code(L.env),
      " from the environment to use what the app keeps.");
  } else if (L.source === "env") {
    p.append("Set by ", code(L.env), " in the server's environment. ");
    const label = lcFirst((L.field && L.field.label) || key);
    const b = el("button", { class: "ghost xs", type: "button", text: "Copy into the app",
      "aria-label": "Copy " + label + " into the app",
      title: "Save this value in the app, so the variable can be removed from the environment",
      "data-focus-key": "copy-" + key, "data-focus-fallback": fallbackFocus });
    b.addEventListener("click", async () => {
      b.disabled = true;
      // What else the copy takes along (an address's secret, the store
      // BLOSSOM_URL chooses): every variable that can go now is named.
      const before = new Set(alsoSaved(scope));
      try {
        await v1(PATH[scope] + "/" + enc(key) + ":copy-from-env", { method: "POST" });
        onCopied(r => {
          const vars = [...new Set(alsoSaved(scope).filter(k => !before.has(k)).map(k => leafOf(scope, k).env).filter(Boolean))];
          if (!vars.length) vars.push(L.env);
          return "Copied. You can now remove " + vars.join(" and ") + " from the server's environment: the app keeps " +
            (vars.length > 1 ? "these values." : "this value.");
        });
      } catch (e) {
        b.disabled = false;
        onCopied(null, e.message);
      }
      restoreFocus(b);
    });
    p.append(b);
  } else if (L.source === "saved") {
    p.append("Saved in the app" + (L.updated_at ? ", " + relTime(L.updated_at) : "") + ".");
    if (L.env) p.append(" ", code(L.env), " in the server's environment would replace it.");
  } else if (L.field && L.field.secret) {
    if (!L.env) return null;
    p.append("The server's environment can set it with ", code(L.env), ".");
  } else {
    p.append(L.value == null || L.value === "" ? "Not set." : "The default.");
    if (L.env) p.append(" The server's environment can set it with ", code(L.env), ".");
  }
  return p;
}

// alsoSaved are the keys of a scope set by the environment with a value
// saved in the app behind it.
function alsoSaved(scope) {
  const r = read[scope];
  const out = [];
  if (!r || !r.body) return out;
  for (const [g, leaves] of Object.entries(r.body)) {
    for (const [n, L] of Object.entries(leaves || {})) if (L && L.also_saved) out.push(g + "." + n);
  }
  return out;
}

// describe adds ids to what an input is described by.
function describe(input, ...ids) {
  const cur = (input.getAttribute("aria-describedby") || "").split(/\s+/).filter(Boolean);
  input.setAttribute("aria-describedby", [...cur, ...ids.filter(Boolean)].join(" "));
}

// secretControl is a secret setting: whether it is set (never its value),
// typing a new one, clearing it, and making one when the server can.
function secretControl(scope, key, L, say, noSecrets) {
  const spec = { ...L.field, key };
  const sv = L.value || {};
  const locked = L.locked || !!noSecrets;
  const c = field(spec, undefined, { locked, keep: !!sv.set && !locked });
  const help = c.row.querySelector(".fld-locked");
  if (help) help.remove();
  const label = lcFirst(spec.label);
  const state = el("p", { class: "fld-state", id: "state-" + key.replace(/\W/g, "-") });
  if (sv.set) {
    state.append(el("span", { class: "fld-set", text: "Set" }));
    if (sv.origin) state.append(" for ", code(sv.origin));
    if (sv.updated_at && L.source === "saved") state.append(", saved " + relTime(sv.updated_at));
    state.append(".");
  } else {
    state.append(el("span", { class: "fld-unset", text: "Not set." }));
  }
  if (L.problem) state.append(" ", el("span", { class: "bad", text: "Saved, but it " + L.problem + "." }));
  if (noSecrets && !L.locked) state.append(" ", noSecrets);
  describe(c.input, state.id);
  const tools = el("div", { class: "fld-tools" });
  if (sv.public_key) {
    const pub = el("input", { class: "tok-secret-input", readonly: "", value: sv.public_key, "aria-label": "Its public key",
      spellcheck: "false" });
    pub.addEventListener("click", () => pub.select());
    const copy = el("button", { class: "ghost xs", type: "button", text: "Copy", "aria-label": "Copy its public key",
      "data-focus-key": "pub-" + key });
    copy.addEventListener("click", () => navigator.clipboard.writeText(sv.public_key).then(
      () => flash("Public key copied"), () => { pub.select(); flash("Select it and copy it"); }));
    c.row.insertBefore(el("div", { class: "fld-pub" },
      el("p", { class: "fld-help", text: "Its public key, to allow on your Blossom server:" }),
      el("div", { class: "tok-secret-row" }, pub, copy)), c.row.querySelector(".fld-help"));
  }
  if (!locked && L.generate) {
    tools.append(el("button", { class: "ghost xs", type: "button", text: sv.set ? "Make a new one" : "Make one",
      "aria-label": (sv.set ? "Make a new " : "Make a ") + label, "data-focus-key": "gen-" + key,
      onclick: e => generate(scope, key, L, e.currentTarget, say) }));
  }
  if (!locked && sv.set) {
    tools.append(el("button", { class: "ghost xs danger-text", type: "button", text: "Clear", "aria-label": "Clear " + label,
      "data-focus-key": "clear-" + key, "data-focus-fallback": "input-" + key,
      onclick: () => confirmModal({ title: "Clear " + label + "?",
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
      await readSettings(scope, [key]);
      say(pub ? "A new key is set: allow its public key on your Blossom server." : "Made: copy it now, it isn't shown again once you're done.");
      restoreFocus(b);
      return true;
    } catch (e) {
      say("", e.message);
      b.disabled = false;
      restoreFocus(b);
      return false;
    }
  };
  if (!again) { await go(); return; }
  confirmModal({ title: "Make a new one?", body: "The one set now stops working at once, wherever it is used.",
    confirmText: "Make a new one", onConfirm: go });
}

// shown holds a value just made, by key, shown until the owner says they
// copied it (a redraw keeps it: another form saved, a section shown again).
const shown = new Map();

function shownOnce(key, label, done) {
  const v = shown.get(key);
  if (!v) return null;
  const input = el("input", { class: "tok-secret-input", readonly: "", value: v, "aria-label": "The new " + label, spellcheck: "false" });
  input.addEventListener("click", () => input.select());
  const copy = el("button", { class: "ghost xs", type: "button", text: "Copy", "aria-label": "Copy the new " + label,
    "data-focus-key": "once-copy-" + key });
  copy.addEventListener("click", () => navigator.clipboard.writeText(v).then(
    () => flash("Copied"), () => { input.select(); flash("Select it and copy it"); }));
  const ok = el("button", { class: "ghost xs", type: "button", text: "Done, I copied it", "data-focus-key": "once-done-" + key });
  ok.addEventListener("click", () => {
    shown.delete(key);
    done();
    const next = document.querySelector('[data-focus-key="' + CSS.escape("gen-" + key) + '"]');
    if (next) next.focus();
  });
  return el("div", { class: "set-note" },
    el("p", { text: "Here it is, shown this once: once you're done, it can't be seen again." }),
    el("div", { class: "tok-secret-row" }, input, copy), el("div", { class: "fld-tools" }, ok));
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
    const saved = new Set(Object.keys(values));
    for (const f of forms[scope]) f.draw(saved);
    document.dispatchEvent(new CustomEvent("settings-saved", { detail: { scope, body: next } }));
    say("Saved.");
    return true;
  } catch (e) {
    if (e.status === 412 || e.code === "locked_by_env") {
      // Read again: what was typed stays where the saved value under it
      // didn't change, so Save once more sends it.
      await readSettings(scope);
      say("", e.status === 412 ? "Settings changed elsewhere meanwhile: what you typed is kept where nothing else changed. Check it, then save again."
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
  const f = { node, draw, controls: new Map() };
  forms[scope].add(f);
  // One status line for the form's life: a live region made again at each
  // draw may not be announced.
  const status = el("p", { class: "set-status", role: "status", "aria-live": "polite" });
  const say = (ok, bad) => {
    status.className = "set-status" + (bad ? " bad" : "");
    status.textContent = bad || ok || "";
  };
  const formKey = keys.join(",");
  // draw draws the form from the last read. What the owner typed and
  // hasn't saved is put back where the saved value under it is still the
  // one they started from (another form saved, a section shown again, a
  // 412), except in the keys just saved.
  function draw(saved = new Set()) {
    const typed = new Map();
    for (const [key, c] of f.controls) {
      if (c.locked || saved.has(key) || !c.input.isConnected) continue;
      const dirty = c.spec.secret ? c.input.value !== "" : !sameValue(c.spec, c.read(), c.leaf.value);
      if (dirty) typed.set(key, { value: c.input.value, checked: c.input.checked, was: JSON.stringify(c.leaf.value == null ? null : c.leaf.value) });
    }
    const active = document.activeElement;
    const focusKey = active && node.contains(active) && active.dataset ? active.dataset.focusKey || "" : "";
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
    const copied = (ok, bad) => { readSettings(scope).then(() => say(typeof ok === "function" ? ok() : ok, bad)); };
    // Behind the reverse proxy alone the server refuses secret settings:
    // they show locked, saying why, rather than refused on Save.
    const noSecrets = state.proxyAuth ? "Behind the reverse proxy alone, secrets can't be set in the app: set up sign-in first." : null;
    for (const key of keys) {
      const L = leafOf(scope, key);
      if (!L) continue;
      const spec = { ...L.field, key };
      let c;
      if (spec.secret) {
        c = secretControl(scope, key, L, (ok, bad) => say(ok, bad), noSecrets);
      } else {
        // Blossom chosen, its address is needed (the server says so too).
        if (key === "media.blossom_url") spec.required = true;
        c = field(spec, L.value, { locked: L.locked, optional: !spec.required && spec.default == null });
        const lockedNote = c.row.querySelector(".fld-locked");
        if (lockedNote) lockedNote.remove();
        const help = c.row.querySelector(".fld-help");
        if (help && !help.childNodes.length) help.hidden = true;
      }
      c.leaf = L;
      c.input.dataset.focusKey = "input-" + key;
      const src = sourceLine(scope, key, L, copied, "save-" + formKey);
      if (src) { c.row.append(src); describe(c.input, src.id); }
      if (c.tools) c.row.append(c.tools);
      const once = shownOnce(key, lcFirst(spec.label), () => draw());
      if (once) c.row.append(once);
      const t = typed.get(key);
      if (t && !c.locked && JSON.stringify(L.value == null ? null : L.value) === t.was) {
        if (spec.type === "bool") c.input.checked = t.checked; else c.input.value = t.value;
      }
      controls.set(key, c);
      byPointer.set("/" + key.replace(".", "/"), c);
    }
    f.controls = controls;
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
    const saveBtn = el("button", { class: "primary sm", type: "submit", text: "Save", "data-focus-key": "save-" + formKey });
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
      // The form is drawn again: the keyboard stays on Save (unless a
      // field with a problem took it).
      if (!node.contains(document.activeElement) || document.activeElement === document.body) {
        const again = node.querySelector(".set-actions .primary");
        if (again) again.focus({ preventScroll: true });
      }
      void ok;
    });
    const row = el("div", { class: "set-actions" });
    if (!opts.readOnly) row.append(saveBtn);
    if (opts.test) {
      const t = el("button", { class: "ghost sm", type: "button", text: opts.test.label || "Send a test", "data-focus-key": "test-" + formKey });
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
    // The keyboard stays where it was: on the same control, drawn again.
    if (focusKey) {
      const again = node.querySelector('[data-focus-key="' + CSS.escape(focusKey) + '"]');
      if (again && !again.disabled && !again.closest("[hidden]")) again.focus({ preventScroll: true });
    }
  }
  draw();
  return f;
}
