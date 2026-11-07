"use strict";

// Passkeys in the browser: the server's options (JSON, binary members in
// base64url) to navigator.credentials, and the answer back to JSON. Uses
// the browser's own JSON helpers when it has them.

const b64u = {
  dec(s) {
    const b = atob(String(s).replace(/-/g, "+").replace(/_/g, "/") + "===".slice((String(s).length + 3) % 4));
    const out = new Uint8Array(b.length);
    for (let i = 0; i < b.length; i++) out[i] = b.charCodeAt(i);
    return out.buffer;
  },
  enc(buf) {
    const bytes = new Uint8Array(buf);
    let s = "";
    for (let i = 0; i < bytes.length; i++) s += String.fromCharCode(bytes[i]);
    return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  },
};

// passkeysSupported: this browser can use passkeys here (a secure
// context with WebAuthn).
export function passkeysSupported() {
  return !!(window.isSecureContext && window.PublicKeyCredential && navigator.credentials);
}

function creationOptions(o) {
  const pk = o.publicKey || o;
  if (typeof PublicKeyCredential.parseCreationOptionsFromJSON === "function") {
    return PublicKeyCredential.parseCreationOptionsFromJSON(pk);
  }
  return {
    ...pk,
    challenge: b64u.dec(pk.challenge),
    user: { ...pk.user, id: b64u.dec(pk.user.id) },
    excludeCredentials: (pk.excludeCredentials || []).map(c => ({ ...c, id: b64u.dec(c.id) })),
  };
}

function requestOptions(o) {
  const pk = o.publicKey || o;
  if (typeof PublicKeyCredential.parseRequestOptionsFromJSON === "function") {
    return PublicKeyCredential.parseRequestOptionsFromJSON(pk);
  }
  return {
    ...pk,
    challenge: b64u.dec(pk.challenge),
    allowCredentials: (pk.allowCredentials || []).map(c => ({ ...c, id: b64u.dec(c.id) })),
  };
}

function toJSON(cred) {
  if (typeof cred.toJSON === "function") {
    try { return cred.toJSON(); } catch (_) { /* fall through */ }
  }
  const r = cred.response, out = {
    id: cred.id, rawId: b64u.enc(cred.rawId), type: cred.type,
    authenticatorAttachment: cred.authenticatorAttachment || undefined,
    clientExtensionResults: cred.getClientExtensionResults ? cred.getClientExtensionResults() : {},
    response: { clientDataJSON: b64u.enc(r.clientDataJSON) },
  };
  if (r.attestationObject) {
    out.response.attestationObject = b64u.enc(r.attestationObject);
    if (typeof r.getTransports === "function") out.response.transports = r.getTransports();
  } else {
    out.response.authenticatorData = b64u.enc(r.authenticatorData);
    out.response.signature = b64u.enc(r.signature);
    if (r.userHandle) out.response.userHandle = b64u.enc(r.userHandle);
  }
  return out;
}

// createPasskey runs navigator.credentials.create with the server's
// options and answers the credential as JSON.
export async function createPasskey(options) {
  const cred = await navigator.credentials.create({ publicKey: creationOptions(options) });
  if (!cred) throw new Error("no passkey was created");
  return toJSON(cred);
}

// getPasskey runs navigator.credentials.get with the server's options and
// answers the assertion as JSON.
export async function getPasskey(options) {
  const cred = await navigator.credentials.get({ publicKey: requestOptions(options) });
  if (!cred) throw new Error("no passkey was used");
  return toJSON(cred);
}

// passkeyError says what went wrong with a passkey prompt in words; ""
// when the person just closed it.
export function passkeyError(e) {
  if (!e) return "";
  if (e.name === "NotAllowedError" || e.name === "AbortError") return "";
  if (e.name === "InvalidStateError") return "This device already has a passkey for Publisher.";
  if (e.name === "SecurityError") return "Passkeys can't be used at this address. Open Publisher at its own https address (PUBLIC_URL).";
  if (e.name === "NotSupportedError") return "This browser or device can't make that kind of passkey.";
  return e.message || "The passkey didn't work.";
}

// postJSON posts JSON to a sign-in route (same origin) and answers the
// parsed body, or throws an Error carrying .status, .code and .problem.
export async function postJSON(path, body) {
  const r = await fetch(path, {
    method: "POST", credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body === undefined ? {} : body),
  });
  const data = r.status === 204 ? null : await r.json().catch(() => null);
  if (!r.ok) {
    const fe = data && Array.isArray(data.errors) ? data.errors : [];
    const err = new Error(fe.length ? fe.map(x => x.detail || x.code).join("; ") : (data && (data.detail || data.title)) || ("HTTP " + r.status));
    err.status = r.status;
    err.code = (data && data.code) || "";
    err.problem = data;
    const ra = Number(r.headers.get("Retry-After"));
    if (ra > 0) err.retryAfter = ra;
    throw err;
  }
  return data;
}

// waitText says how long to wait, from seconds.
export function waitText(secs) {
  if (!secs) return "";
  if (secs < 90) return "Try again in " + Math.ceil(secs) + " seconds.";
  return "Try again in " + Math.ceil(secs / 60) + " minutes.";
}
