// Unit tests for how the web app orders and names accounts
// (internal/web/assets/accounts.js). Run: node --test e2e/unit/
import { test, before } from "node:test";
import assert from "node:assert/strict";

let A;
const page = [
  { id: "acc_late", type: "mastodon", label: "Mastodon (new)", status: "connected", order: -1, default_selected: true, created_at: "2026-11-06T10:00:00Z" },
  { id: "acc_m2", type: "mastodon", label: "Mastodon 2", handle: "@me@two.example", status: "connected", order: 2, default_selected: false },
  { id: "acc_m1", type: "mastodon", label: "Mastodon", handle: "@me@one.example", status: "connected", order: 1, default_selected: true },
  { id: "acc_b1", type: "bluesky", label: "Bluesky", handle: "me.bsky.social", status: "connected", order: 0, default_selected: true, display_name: "Me" },
  { id: "acc_n1", type: "nostr", label: "Nostr", handle: "npub1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq", status: "connected", order: 4, default_selected: true },
  { id: "acc_t1", type: "threads", label: "Work", status: "disabled", order: 3, default_selected: true },
];

before(async () => {
  let events = 0;
  globalThis.document = { dispatchEvent() { events++; } };
  globalThis.fetch = async () => ({ ok: true, status: 200, json: async () => ({ data: page, next_cursor: null }) });
  A = await import("../../internal/web/assets/accounts.js");
  await A.loadAccounts();
  assert.equal(events, 1);
});

test("accounts are in the server's order, one it hasn't loaded last", () => {
  assert.deepEqual(A.accounts().map(a => a.id), ["acc_b1", "acc_m1", "acc_m2", "acc_t1", "acc_n1", "acc_late"]);
  assert.deepEqual(A.inOrder(["acc_n1", "acc_x", "acc_b1"]), ["acc_b1", "acc_n1", "acc_x"]);
});

test("defaults: connected accounts that new posts select", () => {
  assert.deepEqual(A.defaultTargets(), ["acc_b1", "acc_m1", "acc_n1", "acc_late"]);
  assert.equal(A.primaryOf("mastodon").id, "acc_m1");
  assert.equal(A.primaryOf("threads"), null);
});

test("short handles, and full ones when two of a network read the same", () => {
  assert.equal(A.shortHandleOf({ handle: "@me@one.example" }), "@me");
  assert.equal(A.shortHandleOf({ handle: "me.bsky.social" }), "@me");
  assert.equal(A.shortHandleOf({ handle: "me_threads" }), "@me_threads");
  assert.equal(A.shortHandleOf({ handle: "@local" }), "@local");
  const labels = A.labelsFor(["acc_b1", "acc_m1", "acc_m2", "acc_t1", "acc_late"]);
  assert.equal(labels.acc_b1, "Bluesky"); // the owner's only Bluesky account: the network, as before accounts
  assert.equal(labels.acc_n1, undefined);
  assert.equal(labels.acc_m1, "@me@one.example");
  assert.equal(labels.acc_m2, "@me@two.example");
  assert.equal(labels.acc_t1, "Work"); // the only Threads account, named by the owner
  assert.equal(labels.acc_late, "Mastodon (new)");
  assert.deepEqual({ ...A.labelsFor(["acc_m1"]) }, { acc_m1: "@me" });
});

test("names in sentences and for assistive technology", () => {
  assert.equal(A.nameOf("acc_b1"), "Bluesky"); // the only Bluesky account
  assert.equal(A.nameOf("acc_m2"), "Mastodon @me@two.example");
  assert.equal(A.nameOf("acc_late"), "Mastodon (new)");
  assert.equal(A.nameOf("acc_gone"), "an account that is gone");
  assert.equal(A.accessibleName("acc_m1"), "Mastodon, @me@one.example");
  assert.equal(A.accessibleName("acc_t1"), "Threads, Work");
  assert.equal(A.accessibleName("acc_m2"), "Mastodon 2, @me@two.example"); // the label names the network already
  assert.equal(A.accessibleName("acc_late"), "Mastodon (new)");
  assert.match(A.accessibleName("acc_n1"), /^Nostr, npub1qqqqq…/);
});

test("the header shows the first account with a display name", () => {
  const id = A.identity();
  assert.equal(id.name, "Me");
  assert.equal(id.monogram, "M");
});

test("the header skips an account that can't be used", () => {
  const b1 = A.byId("acc_b1");
  b1.status = "needs_reconnect";
  try {
    const id = A.identity();
    assert.equal(id.name, "@me@one.example"); // no connected one has a display name: the first handle
  } finally { b1.status = "connected"; }
});

test("long colliding handles keep the server that tells them apart", () => {
  assert.equal(A.fitHandle("@me@one.example"), "@me@one.example");
  assert.equal(A.fitHandle("@averyveryverylongusername@one.example"), "@averyveryv…@one.example");
  assert.equal(A.fitHandle("@averyveryverylongusername@two.example"), "@averyveryv…@two.example");
  assert.equal(A.fitHandle("@x@a-very-very-long-server-name.example").length, 24);
  assert.equal(A.fitHandle(""), "");
});
