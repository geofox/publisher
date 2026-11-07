"use strict";
import { passkeysSupported, createPasskey, passkeyError, postJSON, waitText } from "/webauthn.js";

// First-run setup (local mode). The link from the server's log carries a
// one-time token in its fragment (never sent to the server by the
// browser, nor in a Referer). The page trades it once for a setup cookie,
// takes it out of the address bar, and then creates the first passkey or
// password, which signs the owner in.

const $ = s => document.querySelector(s);
const status = msg => { $("#auth-status").textContent = msg || ""; };
const show = id => { for (const s of ["#setup-https", "#setup-choose", "#setup-expired"]) $(s).hidden = s !== id; };

function warn(msg) {
  const w = $("#auth-warn");
  w.textContent = msg;
  w.hidden = !msg;
}

function expired(text) {
  if (text) $("#setup-expired-text").textContent = text;
  show("#setup-expired");
  status("");
}

// failed answers a failed setup step: an expired setup (401) or a closed
// one (410) changes the page; anything else is said.
function failed(e) {
  if (e.status === 410) { location.replace("/"); return; }
  if (e.status === 401) { expired("This setup expired (it lasts 10 minutes after the link is opened)."); return; }
  if (e.retryAfter || e.code === "rate_limited") { status((e.message || "Too many attempts.") + " " + waitText(e.retryAfter)); return; }
  status(e.message || "That didn't work.");
}

function defaultName() {
  const ua = navigator.userAgent;
  const os = /iPhone/.test(ua) ? "iPhone" : /iPad/.test(ua) ? "iPad" : /Android/.test(ua) ? "Android"
    : /Mac OS X/.test(ua) ? "Mac" : /Windows/.test(ua) ? "Windows" : /Linux/.test(ua) ? "Linux" : "";
  return os ? "Passkey on " + os : "Passkey";
}

async function withPasskey() {
  const btn = $("#setup-passkey");
  btn.disabled = true;
  status("Waiting for your device…");
  try {
    const opts = await postJSON("/setup/passkey/begin");
    let cred;
    try { cred = await createPasskey(opts); } catch (e) { status(passkeyError(e)); return; }
    status("Saving…");
    await postJSON("/setup/passkey/finish", { name: $("#pk-name").value.trim(), credential: cred });
    status("Done. Opening Publisher…");
    location.replace("/");
  } catch (e) {
    failed(e);
  } finally {
    btn.disabled = false;
  }
}

async function withPassword(ev) {
  ev.preventDefault();
  const a = $("#setup-pw"), b = $("#setup-pw2");
  if (a.value !== b.value) { status("The two passwords differ."); b.focus(); return; }
  const btn = $("#setup-pw-btn");
  btn.disabled = true;
  status("Saving…");
  try {
    await postJSON("/setup/password", { password: a.value });
    status("Done. Opening Publisher…");
    location.replace("/");
  } catch (e) {
    failed(e);
  } finally {
    btn.disabled = false;
  }
}

async function init() {
  // Take the token out of the address bar (and history) first.
  const token = location.hash.slice(1);
  if (location.hash) history.replaceState(null, "", location.pathname);
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
  if (!m.setup) { location.replace("/"); return; }
  if (!m.public_url) { show("#setup-https"); return; }
  if (location.origin !== m.public_url) {
    warn("Open the setup link at " + m.public_url + " (Publisher's address, PUBLIC_URL). This page is at " + location.origin + ", where sign-in can't work.");
    return;
  }
  if (!window.isSecureContext) { show("#setup-https"); return; }
  if (token) {
    try {
      await postJSON("/setup/exchange", { token });
    } catch (e) {
      if (e.status === 401) { expired(); return; }
      failed(e);
      return;
    }
  }
  // Without a token in the address, a setup opened in the last 10 minutes
  // may still be valid here: offer the choices; a step answers 401 if not.
  show("#setup-choose");
  $("#pk-name").value = defaultName();
  const pk = passkeysSupported();
  $("#setup-passkey").disabled = !pk;
  if (!pk) status("This browser can't create passkeys here; you can use a password, and add a passkey later from another browser.");
  $("#setup-passkey").addEventListener("click", withPasskey);
  $("#setup-pw-form").addEventListener("submit", withPassword);
  (pk ? $("#setup-passkey") : $("#setup-pw")).focus();
}

init();
