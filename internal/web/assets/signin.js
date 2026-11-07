"use strict";
import { passkeysSupported, getPasskey, passkeyError, postJSON, waitText } from "/webauthn.js";

// The sign-in page (local mode): served at whatever address a browser
// without a session asked for; signing in reloads it, now as the app.

const $ = s => document.querySelector(s);
const status = msg => { $("#auth-status").textContent = msg || ""; };

function warn(msg) {
  const w = $("#auth-warn");
  w.textContent = msg;
  w.hidden = !msg;
}

// placeWarning says when this page isn't where passkeys and the session
// cookie can work: PUBLIC_URL unset, another address, or plain http.
function placeWarning(m) {
  if (!m.public_url) {
    return "Publisher's address isn't set (PUBLIC_URL), so nobody can sign in. Set PUBLIC_URL to the https address you use for Publisher and restart it.";
  }
  if (location.origin !== m.public_url) {
    return "Sign in at " + m.public_url + " (Publisher's address): this page was opened at " + location.origin + ", where sign-in can't work.";
  }
  if (!window.isSecureContext) return "This page isn't on https, so passkeys and sign-in can't work here.";
  return "";
}

function failed(e, what) {
  if (e.retryAfter || e.code === "rate_limited") {
    status((e.message || "Too many attempts.") + " " + waitText(e.retryAfter));
    return;
  }
  if (e.status === 401) { status(what + " didn't work. Try again."); return; }
  status(e.message || what + " didn't work.");
}

// enterApp loads the app: this address again when it is the app's own,
// else the app's start.
function enterApp() {
  if (location.pathname === "/" || location.pathname === "/index.html") location.reload();
  else location.replace("/");
}

async function signInWithPasskey() {
  const btn = $("#passkey-btn");
  btn.disabled = true;
  status("Waiting for your passkey…");
  try {
    const opts = await postJSON("/auth/passkey/begin");
    let cred;
    try { cred = await getPasskey(opts); } catch (e) { status(passkeyError(e)); return; }
    status("Checking…");
    await postJSON("/auth/passkey/finish", cred);
    status("Signed in.");
    enterApp();
  } catch (e) {
    failed(e, "The passkey");
  } finally {
    btn.disabled = false;
  }
}

async function signInWithPassword(ev) {
  ev.preventDefault();
  const btn = $("#pw-btn"), input = $("#pw-input");
  if (!input.value) return;
  btn.disabled = true;
  status("Checking…");
  try {
    await postJSON("/auth/password", { password: input.value });
    status("Signed in.");
    enterApp();
  } catch (e) {
    input.select();
    failed(e, "That password");
  } finally {
    btn.disabled = false;
  }
}

async function init() {
  let m;
  try {
    const r = await fetch("/auth/methods", { credentials: "same-origin" });
    if (!r.ok) throw new Error("HTTP " + r.status);
    m = await r.json();
  } catch (e) {
    $("#auth-loading").textContent = "Couldn't reach Publisher (" + e.message + "). Reload to try again.";
    return;
  }
  $("#auth-loading").hidden = true;
  warn(placeWarning(m));
  if (m.setup) {
    $("#auth-setup").hidden = false;
    return;
  }
  $("#auth-methods").hidden = false;
  const pk = m.passkeys && passkeysSupported();
  $("#passkey-btn").hidden = !pk;
  $("#pw-form").hidden = !m.password;
  $("#pw-or").hidden = !(pk && m.password);
  if (!pk && !m.password) {
    status(m.passkeys ? "This browser can't use passkeys here. Use a browser with passkey support, on https." : "Passkeys need PUBLIC_URL.");
  }
  $("#passkey-btn").addEventListener("click", signInWithPasskey);
  $("#pw-form").addEventListener("submit", signInWithPassword);
  if (pk) $("#passkey-btn").focus(); else if (m.password) $("#pw-input").focus();
}

init();
