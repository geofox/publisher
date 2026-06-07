"use strict";
import { el, api, flash, relTime, confirmModal } from "./common.js";

// ---------------------------------------------------------------------------
// Token list
// ---------------------------------------------------------------------------

let _tokens = [];

async function loadTokens() {
  const wrap = document.getElementById("tokens-list");
  if (!wrap) return;
  try {
    const data = await api("/api/tokens");
    _tokens = data.tokens || [];
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

  const left = el("div", { class: "tok-info" }, name, created, lastUsed);

  let right;
  if (t.revoked) {
    right = el("span", { class: "pill f tok-badge", text: "Revoked" });
  } else {
    const btn = el("button", {
      class: "ghost sm",
      type: "button",
      text: "Revoke",
    });
    btn.addEventListener("click", () => {
      confirmModal({
        title: "Revoke token "" + t.name + ""?",
        body: "Any client using this token will lose access immediately.",
        confirmText: "Revoke",
        onConfirm: async () => {
          try {
            const r = await fetch("/api/tokens/" + encodeURIComponent(t.id), {
              method: "DELETE",
              credentials: "same-origin",
            });
            // 204 = success; 404 = already gone — either way, re-fetch
            if (!r.ok && r.status !== 404) {
              throw new Error("HTTP " + r.status);
            }
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

  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    const name = nameInput.value.trim();
    if (!name) return;
    createBtn.disabled = true;
    const prev = createBtn.textContent;
    createBtn.textContent = "Creating…";
    try {
      const data = await api("/api/tokens", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ name }),
      });
      nameInput.value = "";
      showNewTokenSecret(data.token, data.name);
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
  const close = () => bk.remove();
  bk.addEventListener("click", (e) => { if (e.target === bk) close(); });

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
    el("button", { class: "modal-x", type: "button", text: "✕", onclick: close }),
    el("p", { class: "modal-title ok", text: "Token created — "" + name + """ }),
    el("p", { class: "tok-warning", text: "Copy this now — you won't be able to see it again." }),
    el("div", { class: "tok-secret-row" }, secretInput, copyBtn),
    el("div", { class: "confirm-actions" },
      el("button", { class: "primary sm", type: "button", text: "Done", onclick: close }),
    ),
  );
  bk.append(card);
  document.body.append(bk);
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
    const r = await fetch("/api/me", { credentials: "same-origin" });
    if (!r.ok) return; // 401 or OIDC disabled — render nothing
    const u = await r.json();
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
