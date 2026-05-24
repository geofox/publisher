"use strict";
// verify.js — the "verify" tab: submit an event/URL to /api/verify and render
// the tri-state verdict.

function byId(id) { return document.getElementById(id); }

function chip(status) {
  const map = {
    verified: ["✓ verified", "ok"],
    failed:   ["✗ failed",          "err"],
    error:    ["⚠ could not verify", "warn"],
  };
  const [label, cls] = map[status] || ["?", "warn"];
  return `<span class="vchip ${cls}">${label}</span>`;
}

function escapeHTML(s) {
  return String(s).replace(/[&<>"']/g, (c) => (
    { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]
  ));
}

function signerLabel(s) {
  if (s.acct)             return s.acct;
  if (s.handle || s.did)  return `${s.handle || ""}${s.did ? " (" + s.did + ")" : ""}`;
  if (s.npub)             return s.npub;
  return s.pubkey_hex || "(unknown)";
}

function renderVerdict(v) {
  if (!v || !v.status) return `<div class="vcard">no result</div>`;
  const rows = [];
  rows.push(`<div class="vhead">${chip(v.status)}`);
  if (v.assurance) rows.push(`<span class="vbadge">${escapeHTML(v.assurance)}</span>`);
  rows.push(`<span class="muted"> ${escapeHTML(v.platform || "")}</span></div>`);
  if (v.error)   rows.push(`<div class="verr">${escapeHTML(v.error)}</div>`);
  if (v.signer)  rows.push(`<div class="vsigner">signer: ${escapeHTML(signerLabel(v.signer))}</div>`);
  if (v.expected) {
    const m = v.expected.matches ? "matches ✓" : "does NOT match ✗";
    rows.push(
      `<div class="vmatch">${m} expected "${escapeHTML(v.expected.provided)}"` +
      `${v.expected.detail ? " — " + escapeHTML(v.expected.detail) : ""}</div>`
    );
  }
  if (v.content && v.content.text) rows.push(`<div class="vexcerpt">${escapeHTML(v.content.text)}</div>`);
  if (Array.isArray(v.warnings)) {
    v.warnings.forEach((w) => rows.push(`<div class="vwarn">${escapeHTML(w)}</div>`));
  }
  if (Array.isArray(v.checks) && v.checks.length) {
    const items = v.checks.map((c) =>
      `<li class="vcheck ${escapeHTML(c.result)}">${escapeHTML(c.name)}: ${escapeHTML(c.result)}` +
      `${c.detail ? ` — ${escapeHTML(c.detail)}` : ""}</li>`
    ).join("");
    rows.push(`<details class="vchecks"><summary>checks</summary><ul>${items}</ul></details>`);
  }
  return `<div class="vcard">${rows.join("")}</div>`;
}

async function submitVerify() {
  const out = byId("vresult");
  const input = byId("vinput").value.trim();
  if (!input) { out.innerHTML = `<div class="vcard">enter an event or URL</div>`; return; }
  out.innerHTML = `<div class="vcard muted">verifying…</div>`;
  try {
    const resp = await fetch("/api/verify", {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ input, expected: byId("vexpected").value.trim() }),
    });
    const v = await resp.json();
    out.innerHTML = renderVerdict(v);
  } catch (e) {
    out.innerHTML = `<div class="vcard verr">request failed: ${escapeHTML(e.message)}</div>`;
  }
}

export function verifyInit() {
  const btn = byId("vsubmit");
  if (btn) btn.addEventListener("click", submitVerify);
}
