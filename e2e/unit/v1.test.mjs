// Unit tests for v1() in internal/web/assets/common.js: an operation that
// needs the owner to confirm it's them is sent once more with the nonce
// the dialog gives, and only once. Run: node --test e2e/unit/
import { test, before } from "node:test";
import assert from "node:assert/strict";

let C;
let calls = [];
let answers = [];

function reply(status, body, headers = {}) {
  return { ok: status >= 200 && status < 300, status, headers: { get: k => headers[k] ?? null }, json: async () => body };
}

before(async () => {
  globalThis.fetch = async (path, opts) => {
    calls.push({ path, headers: { ...opts.headers } });
    return answers.shift();
  };
  C = await import("../../internal/web/assets/common.js");
});

const needs = op => reply(403, { code: "reauth_required", operation: op, detail: "confirm it's you first" });

test("a reauth_required answer asks once and resends with the nonce", async () => {
  calls = [];
  answers = [needs("token.create"), reply(201, { id: "tok_1" })];
  const asked = [];
  C.setReauthHandler(async op => { asked.push(op); return "nonce-1"; });
  const got = await C.v1("/v1/tokens", { method: "POST", body: { name: "x" } });
  assert.deepEqual(got, { id: "tok_1" });
  assert.deepEqual(asked, ["token.create"]);
  assert.equal(calls.length, 2);
  assert.equal(calls[0].headers["Publisher-Reauth"], undefined);
  assert.equal(calls[1].headers["Publisher-Reauth"], "nonce-1");
});

test("a second reauth_required isn't asked again", async () => {
  calls = [];
  answers = [needs("signin.add"), needs("signin.add")];
  let asked = 0;
  C.setReauthHandler(async () => { asked++; return "n"; });
  await assert.rejects(C.v1("/v1/me/passkeys:begin", { method: "POST" }), e => e.code === "reauth_required" && !e.cancelled);
  assert.equal(asked, 1);
  assert.equal(calls.length, 2);
});

test("cancelling the dialog cancels the operation", async () => {
  calls = [];
  answers = [needs("signin.remove")];
  C.setReauthHandler(async () => null);
  await assert.rejects(C.v1("/v1/me/password", { method: "DELETE" }), e => e.cancelled === true && /didn't confirm/.test(e.message));
  assert.equal(calls.length, 1);
});

test("reauth: false leaves the answer to the caller", async () => {
  calls = [];
  answers = [needs("signin.add")];
  let asked = 0;
  C.setReauthHandler(async () => { asked++; return "n"; });
  await assert.rejects(C.v1("/v1/me/passkeys:begin", { method: "POST", reauth: false }), e => e.code === "reauth_required");
  assert.equal(asked, 0);
});

test("a 429 carries Retry-After", async () => {
  answers = [reply(429, { code: "rate_limited", detail: "slow down" }, { "Retry-After": "42" })];
  await assert.rejects(C.v1("/v1/me/passkeys:begin", { method: "POST" }), e => e.retryAfter === 42 && e.status === 429);
});
