"use strict";
import { el, v1, asDialog } from "./common.js";
import { passkeysSupported, getPasskey, passkeyError, postJSON, waitText } from "./webauthn.js";

// The "confirm it's you" dialog: some operations need the owner to prove,
// just now, that they are the owner. reauth(op) answers a nonce for op
// (good once, for five minutes), or null when the owner cancels.
//
// local: the passkey, or the password. oidc: a fresh sign-in with the
// identity provider in a small window, which then lets this page claim
// the nonce.

const WHY = {
  "signin.add": "add a way to sign in",
  "signin.remove": "remove a way to sign in",
  "password.set": "set your password",
  "token.create_privileged": "create a token that can make tokens or change accounts",
};

let mode = "";
// setReauthMode tells the dialog how the owner signs in (GET /v1/instance's auth).
export function setReauthMode(m) { mode = m || ""; }

let open = null; // the dialog on screen: a second request waits for it

export function reauth(op) {
  if (open) return open.then(() => reauth(op));
  open = show(op).finally(() => { open = null; });
  return open;
}

function show(op) {
  return new Promise(resolve => {
    const bk = el("div", { class: "modal-bk" });
    const card = el("div", { class: "modal reauth" });
    const statusLine = el("p", { class: "reauth-status", role: "status", "aria-live": "polite" });
    const say = t => { statusLine.textContent = t || ""; };
    let settled = false;
    let close = () => bk.remove();
    const finish = nonce => {
      if (settled) return;
      settled = true;
      window.removeEventListener("message", onMessage);
      close();
      resolve(nonce || null);
    };
    const cancel = el("button", { class: "ghost sm", type: "button", text: "Cancel", onclick: () => finish(null) });
    const body = el("div", { class: "reauth-body" }, el("p", { class: "muted", text: "Checking how you sign in…" }));
    card.append(
      el("p", { class: "modal-title", text: "Confirm it's you" }),
      el("p", { class: "reauth-why", text: "To " + (WHY[op] || op) + ", confirm it's you." }),
      body, statusLine,
      el("div", { class: "confirm-actions" }, cancel),
    );
    bk.append(card);
    document.body.append(bk);
    close = asDialog(bk, card, close, cancel);
    // asDialog's Escape calls close directly: make it cancel too.
    const obs = new MutationObserver(() => { if (!bk.isConnected) { obs.disconnect(); finish(null); } });
    obs.observe(document.body, { childList: true });

    const claim = async () => {
      try {
        const n = await postJSON("/auth/reauth/claim", { operation: op });
        finish(n.nonce);
      } catch (e) {
        say(e.code === "reauth_required" ? "Not confirmed yet: sign in again in the other window first." : e.message);
      }
    };
    const onMessage = ev => {
      if (ev.origin === location.origin && ev.data && ev.data.type === "publisher-reauth") claim();
    };

    (async () => {
      if (mode === "oidc") {
        window.addEventListener("message", onMessage);
        const url = "/auth/reauth/oidc?operation=" + encodeURIComponent(op);
        const go = el("button", { class: "primary sm", type: "button", text: "Sign in again" });
        const done = el("button", { class: "ghost sm", type: "button", text: "I've signed in", onclick: claim });
        go.addEventListener("click", () => {
          const w = window.open(url, "publisher-reauth", "popup,width=520,height=680");
          say(w ? "Sign in again in the window that opened." : "The window was blocked: allow pop-ups, or open the link in a new tab, then come back.");
          if (!w) body.append(el("p", {}, el("a", { href: url, target: "_blank", rel: "opener", text: "Open the sign-in page" })));
        });
        body.replaceChildren(
          el("p", { class: "muted", text: "Your identity provider will ask you to sign in again." }),
          el("div", { class: "acc-actions-row" }, go, done));
        go.focus();
        return;
      }
      let si;
      try {
        si = await v1("/v1/me/sign-in");
      } catch (e) {
        body.replaceChildren(el("p", { class: "err", text: "Couldn't read how you sign in: " + e.message }));
        return;
      }
      const parts = [];
      let first = null;
      if (si.passkeys.length && si.passkeys_available && passkeysSupported()) {
        const b = el("button", { class: "primary sm", type: "button", text: "Use your passkey" });
        b.addEventListener("click", async () => {
          b.disabled = true;
          say("Waiting for your passkey…");
          try {
            const opts = await postJSON("/auth/reauth/begin", { operation: op });
            let cred;
            try { cred = await getPasskey(opts); } catch (e) { say(passkeyError(e)); return; }
            const n = await postJSON("/auth/reauth", { operation: op, credential: cred });
            finish(n.nonce);
          } catch (e) {
            say(e.status === 422 ? "That passkey didn't work. Try again." : (e.message + (e.retryAfter ? " " + waitText(e.retryAfter) : "")));
          } finally {
            b.disabled = false;
          }
        });
        parts.push(b);
        first = b;
      }
      if (si.password && si.password.set) {
        if (parts.length) parts.push(el("p", { class: "auth-or" }, el("span", { text: "or" })));
        const input = el("input", { type: "password", id: "reauth-pw", autocomplete: "current-password", required: "" });
        const f = el("form", {},
          el("input", { type: "text", name: "username", value: "owner", autocomplete: "username", hidden: "" }),
          el("label", { for: "reauth-pw", text: "Your password" }), input,
          el("button", { class: "ghost sm", type: "submit", text: "Confirm with your password" }));
        f.addEventListener("submit", async ev => {
          ev.preventDefault();
          say("Checking…");
          try {
            const n = await postJSON("/auth/reauth", { operation: op, password: input.value });
            finish(n.nonce);
          } catch (e) {
            input.select();
            say(e.status === 422 ? "That isn't your password." : (e.message + (e.retryAfter ? " " + waitText(e.retryAfter) : "")));
          }
        });
        parts.push(f);
        first = first || input;
      }
      if (!parts.length) {
        parts.push(el("p", { class: "err", text: "This browser can't use your passkeys here, and you have no password. Use the browser you set up a passkey in." }));
      }
      body.replaceChildren(...parts);
      if (first) first.focus();
    })();
  });
}
