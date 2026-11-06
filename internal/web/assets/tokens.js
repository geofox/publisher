"use strict";
import { el, v1, v1All, flash, relTime, confirmModal, asDialog } from "./common.js";

// API tokens on /v1/tokens: each with scopes and an optional expiry.

// SCOPES are what the form offers, with what each allows. The two checked by
// default are what an n8n or shell publisher needs.
const SCOPES = [
  ["posts:write", "publish and manage posts", true],
  ["media:write", "upload media", true],
  ["posts:read", "read history", false],
  ["drafts:read", "read drafts", false],
  ["drafts:write", "write drafts", false],
  ["accounts:read", "see accounts", false],
  ["accounts:write", "change account settings (not part of admin)", false],
  ["admin", "everything else, including tokens and relays", false],
];

// ---------------------------------------------------------------------------
// Token list
// ---------------------------------------------------------------------------

let _tokens = [];

async function loadTokens() {
  const wrap = document.getElementById("tokens-list");
  if (!wrap) return;
  try {
    _tokens = await v1All("/v1/tokens?limit=100");
    renderTokenList(wrap, _tokens);
  } catch (e) {
    wrap.innerHTML = "";
    wrap.append(el("p", { class: "err", text: "Failed to load tokens: " + e.message }));
  }
}

function renderTokenList(wrap, tokens) {
  wrap.innerHTML = "";
  if (!tokens.length) {
    wrap.append(el("p", { class: "muted", text: "No access tokens yet." }));
    return;
  }
  for (const t of tokens) {
    wrap.append(tokenRow(t));
  }
}

function tokenRow(t) {
  const name = el("span", { class: "tok-name", text: t.name });
  const created = el("span", { class: "meta", text: "created " + relTime(t.created_at) });
  const lastUsed = t.last_used_at
    ? el("span", { class: "meta", text: "used " + relTime(t.last_used_at) })
    : el("span", { class: "meta muted", text: "never used" });

  const scopes = el("span", { class: "tok-scopes-list", text: (t.scopes || []).join(" ") });
  const expiry = t.expires_at && t.status === "active"
    ? el("span", { class: "meta", text: "expires " + new Date(t.expires_at).toLocaleDateString() })
    : null;
  const left = el("div", { class: "tok-info" }, name, scopes, created, lastUsed, expiry);

  let right;
  if (t.status === "revoked") {
    right = el("span", { class: "pill f tok-badge", text: "Revoked" });
  } else if (t.status === "expired") {
    right = el("span", { class: "pill off tok-badge", text: "Expired" });
  } else {
    const btn = el("button", {
      class: "ghost sm",
      type: "button",
      text: "Revoke",
    });
    btn.addEventListener("click", () => {
      confirmModal({
        title: "Revoke token \"" + t.name + "\"?",
        body: "Any client using this token will lose access immediately.",
        confirmText: "Revoke",
        onConfirm: async () => {
          try {
            // Revoking twice succeeds; a token already gone (404) is fine too.
            await v1("/v1/tokens/" + encodeURIComponent(t.id), { method: "DELETE" })
              .catch(e => { if (e.status !== 404) throw e; });
            flash("Token revoked");
            await loadTokens();
            return true;
          } catch (e) {
            flash("Revoke failed: " + e.message);
            return false;
          }
        },
      });
    });
    right = btn;
  }

  return el("div", { class: "tok-row" }, left, right);
}

// ---------------------------------------------------------------------------
// New-token form
// ---------------------------------------------------------------------------

function installNewTokenForm() {
  const form = document.getElementById("tokens-new-form");
  const nameInput = document.getElementById("tok-name-input");
  const createBtn = document.getElementById("tok-create-btn");
  if (!form || !nameInput || !createBtn) return;
  const box = document.getElementById("tok-scopes");
  if (box && !box.querySelector("input")) {
    for (const [scope, what, on] of SCOPES) {
      box.append(el("label", { title: what },
        el("input", { type: "checkbox", value: scope, checked: on ? "" : false }),
        el("span", { text: scope })));
    }
  }

  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    const name = nameInput.value.trim();
    if (!name) return;
    createBtn.disabled = true;
    const prev = createBtn.textContent;
    createBtn.textContent = "Creating…";
    try {
      const scopes = [...document.querySelectorAll("#tok-scopes input:checked")].map(i => i.value);
      if (!scopes.length) { flash("Pick at least one thing the token may do"); return; }
      const body = { name, scopes };
      const days = Number(document.getElementById("tok-expiry")?.value || 0);
      if (days > 0) body.expires_at = new Date(Date.now() + days * 86400e3).toISOString().replace(/\.\d{3}Z$/, "Z");
      const data = await v1("/v1/tokens", { method: "POST", body });
      nameInput.value = "";
      showNewTokenSecret(data.secret, data.name);
      await loadTokens();
    } catch (e) {
      flash("Create failed: " + e.message);
    } finally {
      createBtn.disabled = false;
      createBtn.textContent = prev;
    }
  });
}

// Shows the plaintext token in a modal with a clear one-time warning.
function showNewTokenSecret(token, name) {
  const bk = el("div", { class: "modal-bk" });
  let close = () => bk.remove();
  // No close on a backdrop tap: the secret is shown only once. Done or ✕.

  const secretInput = el("input", {
    type: "text",
    class: "tok-secret-input",
    value: token,
    readonly: "",
    "aria-label": "New token value",
  });
  secretInput.addEventListener("click", () => {
    secretInput.select();
  });

  const copyBtn = el("button", {
    class: "ghost sm",
    type: "button",
    text: "Copy",
  });
  copyBtn.addEventListener("click", () => {
    navigator.clipboard.writeText(token).then(
      () => { copyBtn.textContent = "Copied!"; setTimeout(() => { copyBtn.textContent = "Copy"; }, 1800); },
      () => { secretInput.select(); flash("Select and copy manually"); },
    );
  });

  const card = el("div", { class: "modal" });
  card.append(
    el("button", { class: "modal-x", type: "button", text: "✕", "aria-label": "Close", onclick: () => close() }),
    el("p", { class: "modal-title ok", text: "Token created — \"" + name + "\"" }),
    el("p", { class: "tok-warning", text: "Copy this now — you won't be able to see it again." }),
    el("div", { class: "tok-secret-row" }, secretInput, copyBtn),
    el("div", { class: "confirm-actions" },
      el("button", { class: "primary sm", type: "button", text: "Done", onclick: () => close() }),
    ),
  );
  bk.append(card);
  document.body.append(bk);
  close = asDialog(bk, card, close, secretInput, { escape: false }); // shown once: Done or ✕ only
  // Auto-select the secret so the user can copy immediately
  setTimeout(() => secretInput.select(), 60);
}

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

export function tokensInit() {
  installNewTokenForm();
}

export function tokensShow() {
  loadTokens();
}

// ---------------------------------------------------------------------------
// User chip + sign-out (mounted into the app shell header)
// ---------------------------------------------------------------------------

export async function loadUserChip() {
  try {
    const me = await v1("/v1/me");
    const u = me.user; // a browser session; absent behind the proxy
    if (!u) return;
    const display = u.name || u.email || u.subject;
    if (!display) return;
    mountUserChip(display);
  } catch (_) { /* OIDC disabled or network error — stay silent */ }
}

function mountUserChip(display) {
  const brand = document.querySelector(".brand");
  if (!brand) return;

  const signoutBtn = el("button", {
    class: "ghost sm user-signout",
    type: "button",
    text: "Sign out",
  });
  signoutBtn.addEventListener("click", () => {
    // Full-page POST to /auth/logout so the IdP 302 redirect is followed natively.
    const f = document.createElement("form");
    f.method = "POST";
    f.action = "/auth/logout";
    document.body.append(f);
    f.submit();
  });

  const chip = el("div", { class: "user-chip" },
    el("span", { class: "user-chip-name", text: display }),
    signoutBtn,
  );
  brand.append(chip);
}
