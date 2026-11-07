"use strict";
import { el, v1, flash, relTime, confirmModal, asDialog } from "./common.js";
import { passkeysSupported, createPasskey, passkeyError } from "./webauthn.js";

// Access: how the owner signs in (passkeys and the password, in local
// mode), where they are signed in, and recent sign-in activity. Lives in
// the Access tab, above the API tokens.

let mode = "";
// setAccessMode tells the panel how the owner signs in (GET /v1/instance's auth).
export function setAccessMode(m) { mode = m || ""; }

const box = () => document.getElementById("access");

function section(label, note, ...kids) {
  return el("div", { class: "acc-section" },
    el("div", { class: "sec-head" }, el("span", { class: "lbl", text: label }), note ? el("span", { class: "muted", text: note }) : null),
    ...kids);
}

// signOut ends this browser's session, then goes where the server says
// (the identity provider's sign-out, or the start). A fetch, not a form:
// the app's Referrer-Policy makes a form post send "Origin: null", which
// the server refuses as cross-site.
async function signOut() {
  try {
    const r = await fetch("/auth/logout", { method: "POST", credentials: "same-origin", headers: { Accept: "application/json" } });
    const data = r.ok ? await r.json().catch(() => null) : null;
    if (!r.ok) throw new Error("HTTP " + r.status);
    location.assign((data && data.redirect) || "/");
  } catch (e) {
    flash("Sign-out failed: " + e.message);
  }
}

export async function accessShow() {
  const wrap = box();
  if (!wrap) return;
  if (mode === "proxy") {
    wrap.replaceChildren(section("sign-in", "",
      el("p", { class: "acc-note", text: "Publisher has no sign-in of its own here: the reverse proxy in front of it is the only gate, and account credentials can't be managed in the app." }),
      el("p", { class: "acc-note" }, "To sign in with a passkey, set ", el("code", { text: "AUTH_MODE=local" }), " and ", el("code", { text: "PUBLIC_URL" }),
        " (the https address you use), restart, and open the setup link from the log. ",
        el("a", { href: "https://github.com/geofox/publisher#sign-in", target: "_blank", rel: "noopener noreferrer", text: "How to set up sign-in" }))));
    return;
  }
  if (!wrap.childElementCount) wrap.append(el("p", { class: "muted", text: "Reading how you sign in…" }));
  let si, sessions, events;
  try {
    [si, sessions, events] = await Promise.all([v1("/v1/me/sign-in"), v1("/v1/me/sessions"), v1("/v1/me/sign-in-events")]);
  } catch (e) {
    wrap.replaceChildren(el("p", { class: "err", text: "Couldn't read your sign-in settings: " + e.message }));
    return;
  }
  wrap.replaceChildren(
    signInSection(si),
    section("where you're signed in", "", ...sessions.data.map(sessionRow)),
    section("recent activity", "sign-ins, failed attempts and changes", eventList(events.data)),
  );
}

// ── sign-in methods ─────────────────────────────────────────────────────

function signInSection(si) {
  if (si.mode !== "local") {
    return section("sign-in", "",
      el("p", { class: "acc-note", text: "You sign in with your identity provider (OIDC); passkeys and passwords are managed there." }),
      el("div", { class: "acc-actions-row" }, el("button", { class: "ghost sm", type: "button", text: "Sign out", onclick: signOut })));
  }
  const rows = si.passkeys.map(passkeyRow);
  if (!rows.length) rows.push(el("p", { class: "acc-note", text: "No passkey yet." }));
  const canAdd = si.passkeys_available && passkeysSupported();
  const add = el("button", { class: "primary sm", type: "button", text: "Add a passkey", onclick: addPasskey, disabled: canAdd ? false : "" });
  const pw = si.password;
  const pwRow = el("div", { class: "acc-row" },
    el("div", { class: "acc-info" },
      el("span", { class: "acc-name", text: "Password" }),
      el("span", { class: "meta", text: pw.set ? "set " + relTime(pw.updated_at) : "not set" })),
    el("div", { class: "acc-actions" },
      el("button", { class: "ghost sm", type: "button", text: pw.set ? "Change" : "Set a password", onclick: () => passwordDialog(pw.set) }),
      pw.set ? el("button", { class: "ghost sm", type: "button", text: "Remove", onclick: removePassword }) : null));
  return section("sign-in", "passkeys and password",
    ...rows,
    el("div", { class: "acc-actions-row" }, add,
      canAdd ? null : el("span", { class: "muted", text: si.passkeys_available ? "this browser can't make passkeys here" : "passkeys need PUBLIC_URL" })),
    pwRow,
    el("div", { class: "acc-actions-row" }, el("button", { class: "ghost sm", type: "button", text: "Sign out", onclick: signOut })));
}

function passkeyRow(p) {
  return el("div", { class: "acc-row" },
    el("div", { class: "acc-info" },
      el("span", { class: "acc-name", text: p.name }),
      p.synced ? el("span", { class: "acc-tag", text: "synced" }) : null,
      el("span", { class: "meta", text: "added " + relTime(p.created_at) }),
      el("span", { class: "meta", text: p.last_used_at ? "used " + relTime(p.last_used_at) : "never used" })),
    el("div", { class: "acc-actions" },
      el("button", { class: "ghost sm", type: "button", text: "Rename", "aria-label": "Rename " + p.name, onclick: () => renamePasskey(p) }),
      el("button", { class: "ghost sm", type: "button", text: "Remove", "aria-label": "Remove " + p.name, onclick: () => removePasskey(p) })));
}

// inputDialog asks for one value; onSubmit answers true to close.
function inputDialog({ title, label, value = "", type = "text", fields, submitText, onSubmit }) {
  const bk = el("div", { class: "modal-bk" });
  let close = () => bk.remove();
  const err = el("p", { class: "reauth-status", role: "status", "aria-live": "polite" });
  const inputs = (fields || [{ label, value, type }]).map((f, i) => {
    const id = "dlg-in-" + i;
    return { label: el("label", { for: id, text: f.label }), input: el("input", { id, type: f.type || "text", value: f.value || "", autocomplete: f.autocomplete || "off", maxlength: f.maxlength || false }) };
  });
  const ok = el("button", { class: "primary sm", type: "submit", text: submitText || "Save" });
  const form = el("form", { class: "reauth" },
    ...inputs.flatMap(x => [x.label, x.input]), err,
    el("div", { class: "confirm-actions" }, el("button", { class: "ghost sm", type: "button", text: "Cancel", onclick: () => close() }), ok));
  form.addEventListener("submit", async ev => {
    ev.preventDefault();
    ok.disabled = true;
    try {
      if (await onSubmit(inputs.map(x => x.input.value), m => { err.textContent = m; })) close();
    } finally {
      ok.disabled = false;
    }
  });
  const card = el("div", { class: "modal reauth" }, el("p", { class: "modal-title", text: title }), form);
  bk.append(card);
  document.body.append(bk);
  close = asDialog(bk, card, close, inputs[0].input);
}

function deviceName() {
  const ua = navigator.userAgent;
  const os = /iPhone/.test(ua) ? "iPhone" : /iPad/.test(ua) ? "iPad" : /Android/.test(ua) ? "Android"
    : /Mac OS X/.test(ua) ? "Mac" : /Windows/.test(ua) ? "Windows" : /Linux/.test(ua) ? "Linux" : "";
  return os ? "Passkey on " + os : "Passkey";
}

function addPasskey() {
  inputDialog({
    title: "Add a passkey", label: "Name it (to tell it apart later)", value: deviceName(), submitText: "Create",
    onSubmit: async ([name], say) => {
      try {
        const opts = await v1("/v1/me/passkeys:begin", { method: "POST" });
        say("Waiting for your device…");
        let cred;
        try { cred = await createPasskey(opts); } catch (e) { say(passkeyError(e)); return false; }
        await v1("/v1/me/passkeys", { method: "POST", body: { name: name.trim(), credential: cred } });
        flash("Passkey added");
        accessShow();
        return true;
      } catch (e) {
        say(e.message);
        return false;
      }
    },
  });
}

function renamePasskey(p) {
  inputDialog({
    title: "Rename passkey", label: "Name", value: p.name,
    onSubmit: async ([name], say) => {
      try {
        await v1("/v1/me/passkeys/" + encodeURIComponent(p.id), { method: "PATCH", body: { name } });
        accessShow();
        return true;
      } catch (e) { say(e.message); return false; }
    },
  });
}

function lastMethod(e) {
  return e.code === "last_sign_in_method" ? "That's your last way to sign in: add a passkey or set a password first." : e.message;
}

function removePasskey(p) {
  confirmModal({
    title: "Remove the passkey \"" + p.name + "\"?",
    body: "It won't sign in here any more, and every other browser signed in to Publisher is signed out.",
    confirmText: "Remove",
    onConfirm: async () => {
      try {
        await v1("/v1/me/passkeys/" + encodeURIComponent(p.id), { method: "DELETE" });
        flash("Passkey removed");
        accessShow();
        return true;
      } catch (e) {
        if (!e.cancelled) flash(lastMethod(e));
        return !!e.cancelled;
      }
    },
  });
}

function passwordDialog(isSet) {
  const fields = [];
  if (isSet) fields.push({ label: "Current password", type: "password", autocomplete: "current-password" });
  fields.push({ label: "New password (at least 12 characters)", type: "password", autocomplete: "new-password" },
    { label: "The new password again", type: "password", autocomplete: "new-password" });
  inputDialog({
    title: isSet ? "Change your password" : "Set a password", fields, submitText: isSet ? "Change" : "Set",
    onSubmit: async (vals, say) => {
      const [cur, a, b] = isSet ? vals : [null, ...vals];
      if (a !== b) { say("The two new passwords differ."); return false; }
      const body = { password: a };
      if (isSet) body.current_password = cur;
      try {
        await v1("/v1/me/password", { method: "PUT", body });
        flash(isSet ? "Password changed; other browsers are signed out" : "Password set");
        accessShow();
        return true;
      } catch (e) { say(e.message); return false; }
    },
  });
}

function removePassword() {
  confirmModal({
    title: "Remove your password?",
    body: "You'll sign in with a passkey only, and every other browser signed in to Publisher is signed out.",
    confirmText: "Remove",
    onConfirm: async () => {
      try {
        await v1("/v1/me/password", { method: "DELETE" });
        flash("Password removed");
        accessShow();
        return true;
      } catch (e) {
        if (!e.cancelled) flash(lastMethod(e));
        return !!e.cancelled;
      }
    },
  });
}

// ── sessions ────────────────────────────────────────────────────────────

const METHOD = { passkey: "a passkey", password: "the password", setup: "setup", oidc: "your identity provider" };

// uaText names a browser and system from a User-Agent ("Safari on iPhone").
export function uaText(ua) {
  ua = ua || "";
  const browser = /Edg\//.test(ua) ? "Edge" : /Firefox\//.test(ua) ? "Firefox" : /OPR\//.test(ua) ? "Opera"
    : /Chrome\//.test(ua) ? "Chrome" : /Safari\//.test(ua) ? "Safari" : "";
  const os = /iPhone/.test(ua) ? "iPhone" : /iPad/.test(ua) ? "iPad" : /Android/.test(ua) ? "Android"
    : /Mac OS X/.test(ua) ? "Mac" : /Windows/.test(ua) ? "Windows" : /CrOS/.test(ua) ? "ChromeOS" : /Linux/.test(ua) ? "Linux" : "";
  if (browser && os) return browser + " on " + os;
  return browser || os || "a browser";
}

function sessionRow(s) {
  const end = el("button", { class: "ghost sm", type: "button", text: "Sign out" });
  wireSignOut(end, s);
  return el("div", { class: "acc-row" },
    el("div", { class: "acc-info" },
      el("span", { class: "acc-name", text: uaText(s.user_agent) }),
      s.current ? el("span", { class: "acc-tag", text: "this browser" }) : null,
      s.addr ? el("span", { class: "meta", text: s.addr }) : null,
      el("span", { class: "meta", text: "signed in " + relTime(s.created_at) + (METHOD[s.method] ? " with " + METHOD[s.method] : "") }),
      s.last_seen_at ? el("span", { class: "meta", text: "active " + relTime(s.last_seen_at) }) : null),
    el("div", { class: "acc-actions" }, end));
}

function wireSignOut(end, s) {
  end.addEventListener("click", async () => {
    if (s.current) { signOut(); return; }
    end.disabled = true;
    try {
      await v1("/v1/me/sessions/" + encodeURIComponent(s.id), { method: "DELETE" }).catch(e => { if (e.status !== 404) throw e; });
      flash("Signed out");
      accessShow();
    } catch (e) {
      flash("Sign-out failed: " + e.message);
      end.disabled = false;
    }
  });
  end.setAttribute("aria-label", s.current ? "Sign out of this browser" : "Sign out " + uaText(s.user_agent));
}

// ── events ──────────────────────────────────────────────────────────────

function eventText(e) {
  const how = e.method === "passkey" ? "a passkey" : e.method === "password" ? "the password" : e.method === "setup" ? "setup" : e.method === "oidc" ? "your identity provider" : e.method;
  switch (e.type) {
    case "login.succeeded": return ["Signed in" + (how ? " with " + how : ""), ""];
    case "login.failed": return ["Failed sign-in" + (how ? " with " + how : ""), "bad"];
    case "signin.added": return [e.method === "password" ? "Set a password" : "Added a passkey", ""];
    case "signin.changed": return ["Changed the password", ""];
    case "signin.removed": return [e.method === "password" ? "Removed the password" : e.method === "reset-login" ? "Reset sign-in from the server (reset-login)" : "Removed a passkey", "warn"];
    case "token.created": return ["Created an API token" + (e.scopes && e.scopes.length ? " (" + e.scopes.join(" ") + ")" : ""), ""];
  }
  return [e.type, ""];
}

function eventList(list) {
  if (!list.length) return el("p", { class: "acc-note", text: "Nothing yet." });
  return el("ul", { class: "acc-events" }, ...list.map(e => {
    const [text, cls] = eventText(e);
    return el("li", {},
      el("span", { class: "when", text: relTime(e.created_at), title: new Date(e.created_at).toLocaleString() }),
      el("span", { class: cls, text }),
      e.addr ? el("span", { class: "meta", text: e.addr }) : null);
  }));
}
