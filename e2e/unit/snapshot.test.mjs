// Unit tests for the composer's saved form (internal/web/assets/snapshot.js):
// older snapshots (keyed by network, or with "---" breaks) become version 3
// (keyed by account id). Run: node --test e2e/unit/
import { test } from "node:test";
import assert from "node:assert/strict";
import { upgrade, cleanOv, primaryIn, standIn, VERSION } from "../../internal/web/assets/snapshot.js";
import { fromMarkers } from "../../internal/web/assets/textpos.js";

const acct = (id, type, extra = {}) => ({ id, type, status: "connected", default_selected: true, ...extra });
const list = [
  acct("acc_b1", "bluesky"),
  acct("acc_m1", "mastodon", { default_selected: false }),
  acct("acc_m2", "mastodon"),
  acct("acc_t1", "threads", { status: "not_configured" }),
  acct("acc_n1", "nostr"),
];

test("primaryIn prefers a connected account that new posts select", () => {
  assert.equal(primaryIn(list, "mastodon").id, "acc_m2");
  assert.equal(primaryIn(list, "bluesky").id, "acc_b1");
  assert.equal(primaryIn(list, "threads"), null); // not connected
  assert.equal(primaryIn(list, "unknown"), null);
  // None selected by default: the first connected one.
  const quiet = [acct("a", "mastodon", { default_selected: false }), acct("b", "mastodon", { default_selected: false })];
  assert.equal(primaryIn(quiet, "mastodon").id, "a");
});

test("standIn: the primary account, else any that isn't deleted, else none", () => {
  assert.equal(standIn(list, "mastodon").id, "acc_m2");
  assert.equal(standIn(list, "threads").id, "acc_t1");
  assert.equal(standIn([acct("x", "threads", { status: "deleted" })], "threads"), null);
  assert.equal(standIn(list, "unknown"), null);
});

test("a network where the owner has no account is left out, saying so", () => {
  const snap = upgrade({ v: 2, master_text: "x", platforms: ["bluesky", "unknown"], overrides: { unknown: { text: "own" } },
    interaction: { action: "reply", platform: "threads", sourceInput: "u", prevPlatforms: ["bluesky"] } }, list);
  assert.deepEqual(snap.targets, ["acc_b1"]);
  assert.deepEqual(snap.dropped, ["unknown"]);
  assert.deepEqual(Object.keys(snap.overrides), []);
  // A reply needs a connected account to act: none on Threads.
  assert.equal(snap.interaction.account, "");
});

test("version 1: breaks from --- lines, media by part number, networks to accounts", () => {
  const master = "one\n---\ntwo\n---\nthree";
  const own = "mine\n---\nmore";
  const snap = upgrade({
    master_text: master,
    platforms: ["mastodon", "bluesky", "threads"],
    overrides: {
      mastodon: { text: own, spoiler_text: "cw" },
      bluesky: { bluesky_reply: "nobody", bluesky_disable_quotes: true, langs: ["en", "fr"] },
      threads: { threads_reply_control: "followers_only" },
    },
    anchors: { img1: 2, img2: 0, img3: 9 },
    interaction: { action: "reply", platform: "mastodon", sourceInput: "https://m.example/@a/1", prevPlatforms: ["bluesky", "threads"] },
  }, list);
  const conv = fromMarkers(master);
  assert.equal(snap.v, VERSION);
  assert.equal(snap.master_text, conv.text);
  assert.deepEqual(snap.breaks, conv.breaks);
  // Threads' account isn't connected: it comes back selected (and shows
  // why it can't post), with its settings.
  assert.deepEqual(snap.targets, ["acc_m2", "acc_b1", "acc_t1"]);
  assert.deepEqual(snap.dropped, []);
  assert.equal(snap.overrides.acc_t1.reply_control, "followers_only");
  assert.equal("platforms" in snap, false);
  const mineConv = fromMarkers(own);
  assert.equal(snap.overrides.acc_m2.text, mineConv.text);
  assert.deepEqual(snap.overrides.acc_m2.breaks, mineConv.breaks);
  assert.equal(snap.overrides.acc_m2.spoiler_text, "cw");
  assert.deepEqual(snap.overrides.acc_b1, { reply: "nobody", disable_quotes: true, langs: "en,fr", text: null, breaks: null, mediaAt: null });
  // Part 2 starts at "three"; part 0 isn't a position; past the end is the last part.
  assert.equal(snap.anchors.img1, conv.parts[2]);
  assert.equal(conv.text.slice(snap.anchors.img1), "three");
  assert.equal("img2" in snap.anchors, false);
  assert.equal(snap.anchors.img3, conv.parts[conv.parts.length - 1]);
  assert.deepEqual(snap.interaction, {
    action: "reply", sourceInput: "https://m.example/@a/1", platform: "mastodon", account: "acc_m2", prevTargets: ["acc_b1", "acc_t1"],
  });
});

test("version 2: positions kept, settings of networks not selected dropped", () => {
  const snap = upgrade({
    v: 2,
    master_text: "hello world",
    breaks: [6, "x", -1, 6.5],
    platforms: ["nostr", "bluesky", "nostr"],
    overrides: { nostr: { pow: 22, text: "own", breaks: [2], mediaAt: { a: 1 } }, mastodon: { visibility: "direct" } },
    anchors: { a: 6, b: -3, c: "1" },
    interaction: { action: "quote", platform: "bluesky", sourceInput: "at://x", prevPlatforms: "not a list" },
  }, list);
  assert.deepEqual(snap.breaks, [6]);
  assert.deepEqual(snap.targets, ["acc_n1", "acc_b1"]);
  assert.deepEqual(Object.keys(snap.overrides), ["acc_n1"]);
  assert.deepEqual(snap.overrides.acc_n1, { pow: 22, text: "own", breaks: [2], mediaAt: { a: 1 } });
  assert.deepEqual(snap.anchors, { a: 6 });
  assert.equal(snap.interaction.account, "acc_b1");
  assert.deepEqual(snap.interaction.prevTargets, []);
  assert.deepEqual(snap.dropped, []);
});

test("version 3: accounts that are gone are left out", () => {
  const snap = upgrade({
    v: 3,
    master_text: "x",
    breaks: [],
    targets: ["acc_m1", "acc_gone", "acc_m1", 7],
    overrides: { acc_m1: { visibility: "unlisted", text: 5 }, acc_gone: { text: "lost" } },
    interaction: { action: "reply", account: "acc_m1", platform: "mastodon", sourceInput: "u", prevTargets: ["acc_gone", "acc_b1", "acc_b1"] },
    tags: ["a"], draft_id: "drf_1", etag: "\"3\"",
  }, list);
  assert.deepEqual(snap.targets, ["acc_m1"]);
  assert.deepEqual(snap.dropped, ["acc_gone", "7"]);
  assert.deepEqual(Object.keys(snap.overrides), ["acc_m1"]);
  assert.equal(snap.overrides.acc_m1.text, null); // not a string
  assert.equal(snap.interaction.account, "acc_m1");
  assert.deepEqual(snap.interaction.prevTargets, ["acc_b1"]);
  // What it doesn't know about is kept.
  assert.deepEqual(snap.tags, ["a"]);
  assert.equal(snap.draft_id, "drf_1");
  assert.equal(snap.etag, "\"3\"");
});

test("version 3: an acting account that is gone is left for the server to pick", () => {
  const snap = upgrade({ v: 3, targets: [], interaction: { action: "quote", account: "acc_gone", platform: "mastodon", sourceInput: "u" } }, list);
  assert.equal(snap.interaction.account, "");
  assert.equal(snap.interaction.platform, "mastodon");
  const known = upgrade({ v: 3, targets: [], interaction: { action: "quote", account: "acc_b1", sourceInput: "u" } }, list);
  assert.equal(known.interaction.platform, "bluesky"); // from the account
});

test("cleanOv: own media positions only with own text; old names renamed", () => {
  assert.deepEqual(cleanOv({ mediaAt: { a: 1 } }), { mediaAt: null, text: null, breaks: null });
  assert.deepEqual(cleanOv({ text: "t", mediaAt: { a: 1, b: -1, c: "2" }, breaks: [1, "2"] }), { text: "t", mediaAt: { a: 1 }, breaks: [1] });
  // A new key already there wins over the old one.
  assert.deepEqual(cleanOv({ bluesky_reply: "old", reply: "new" }), { reply: "new", text: null, breaks: null, mediaAt: null });
});

test("anything that isn't a snapshot becomes an empty version 3", () => {
  for (const bad of [null, undefined, "x", 3, []]) {
    const snap = upgrade(bad, list);
    assert.equal(snap.v, VERSION);
    assert.equal(snap.master_text, "");
    assert.deepEqual(snap.targets, []);
    assert.equal(snap.interaction, null);
  }
  assert.deepEqual(upgrade({ v: 2, platforms: ["mastodon"] }, []).dropped, ["mastodon"]);
});

test("cleanOv: keys that would reach a prototype are dropped", () => {
  const parsed = JSON.parse('{"text":"x","__proto__":{"polluted":1},"constructor":{"a":1},"prototype":{}}');
  const ov = cleanOv(parsed);
  assert.equal(Object.getPrototypeOf(ov), Object.prototype);
  assert.deepEqual(Object.keys(ov).filter(k => ["__proto__", "constructor", "prototype"].includes(k)), []);
  const target = { langs: "en" };
  Object.assign(target, ov);
  assert.equal(target.polluted, undefined);
  assert.equal(Object.getPrototypeOf(target), Object.prototype);
  assert.equal(target.text, "x");
});
