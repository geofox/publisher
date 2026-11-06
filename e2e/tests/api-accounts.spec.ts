import type { APIRequestContext } from "@playwright/test";
import { test, expect, marker, listAccounts, accountLabelled, type Account } from "./fixtures";

// Several accounts of one network, through /v1 (the composer is still one
// row per network until part A3). The harness's accounts are in
// harnessAccounts (e2e/harness/main.go): two connected Mastodon accounts,
// the second with a 1000-character limit, one the environment no longer
// configures, and two Bluesky accounts, the second not selected by default.

type Item = { ordinal: number; status: string };
type Target = { id: string; account: string; type: string; status: string; items: Item[] };
type Post = { id: string; status: string; targets: Target[] };

const json = { "Content-Type": "application/json" };

async function createPost(request: APIRequestContext, body: object, headers: Record<string, string> = {}) {
  return request.post("/v1/posts", { headers: { ...json, ...headers }, data: body });
}

async function getPost(request: APIRequestContext, id: string): Promise<Post> {
  const r = await request.get(`/v1/posts/${id}`);
  expect(r.status()).toBe(200);
  return r.json();
}

async function waitForPost(request: APIRequestContext, id: string, check: (p: Post) => boolean, what: string): Promise<Post> {
  let p: Post | undefined;
  await expect.poll(async () => { p = await getPost(request, id); return check(p); }, { message: what, timeout: 10_000 }).toBe(true);
  return p!;
}

async function patchAccount(request: APIRequestContext, id: string, body: object, headers: Record<string, string> = {}) {
  return request.patch(`/v1/accounts/${id}`, { headers: { ...json, ...headers }, data: body });
}

/** Words up to n characters, starting with prefix. */
function words(prefix: string, n: number) {
  let s = prefix;
  while (s.length + 6 <= n) s += " words";
  return s;
}

test("accounts are listed in order, each with its status and flags", async ({ request }) => {
  const all = await listAccounts(request); // oldest first
  // order numbers them from 0: networks in a fixed order, then oldest first.
  const ordered = [...all].sort((a, b) => a.order - b.order);
  expect(ordered.map(a => a.order)).toEqual(all.map((_, i) => i));
  const types = ordered.map(a => a.type);
  expect(types, "each network's accounts together").toEqual([...types].sort((a, b) => types.indexOf(a) - types.indexOf(b)));
  const of = (type: string) => ordered.filter(a => a.type === type);
  expect(of("mastodon").map(a => a.id)).toEqual(all.filter(a => a.type === "mastodon").map(a => a.id));
  expect(of("mastodon").map(a => [a.label, a.status])).toEqual([
    ["Mastodon", "connected"], ["Mastodon (long posts)", "connected"], ["Mastodon (old)", "not_configured"]]);
  // The reason names what to set, without any value.
  expect(of("mastodon")[2].status_reason).toMatch(/^set MASTODON_\w+ .*to use it again$/);
  expect(of("mastodon")[0].status_reason ?? "").toBe("");
  expect(of("bluesky").map(a => [a.label, a.default_selected])).toEqual([["Bluesky", true], ["Bluesky (alt)", false]]);
  // Each account reads back alone, with the ETag a change needs.
  const one = await request.get(`/v1/accounts/${of("mastodon")[1].id}`);
  expect(one.status()).toBe(200);
  expect(one.headers()["etag"]).toMatch(/^"\d+"$/);
  expect((await one.json()).label).toBe("Mastodon (long posts)");
});

test("a post to two Mastodon accounts goes out from each, with its own text and limit", async ({ request, fakes }) => {
  const m = marker("two");
  const main = await accountLabelled(request, "Mastodon");
  const long = await accountLabelled(request, "Mastodon (long posts)");
  const shared = words(`Shared ${m}`, 700); // two posts at 500 characters
  const own = words(`Own ${m}`, 900); // one post at 1000
  const res = await createPost(request, {
    content: { text: shared },
    targets: [{ account: main.id }, { account: long.id, content: { text: own } }],
  }, { "Prefer": "wait=30" });
  expect(res.status()).toBe(201);
  const post = (await res.json()) as Post;
  expect(post.status).toBe("published");
  expect(post.targets.map(t => [t.account, t.items.length])).toEqual([[main.id, 2], [long.id, 1]]);

  // The post is published, so every call is in; only the first part of
  // the shared text carries the marker.
  const calls = await fakes.calls();
  const from = (id: string) => calls.filter(c => c.account === id);
  expect(from(main.id).map(c => c.action)).toEqual(["post", "reply"]);
  expect(from(main.id)[1].reply_to).toBe(from(main.id)[0].remote_id);
  expect(from(main.id).map(c => c.text).join(" ")).toContain(`Shared ${m}`);
  expect(from(long.id)).toEqual([expect.objectContaining({ action: "post", text: own })]);
});

test("retrying one account's failed item sends it again from that account only", async ({ request, fakes }) => {
  const m = marker("retry1");
  const main = await accountLabelled(request, "Mastodon");
  const long = await accountLabelled(request, "Mastodon (long posts)");
  await fakes.down(long.id); // that account only: the other Mastodon account is up
  const res = await createPost(request, { content: { text: `One down ${m}` }, targets: [{ account: main.id }, { account: long.id }] });
  expect(res.status()).toBe(202);
  const { id } = await res.json();
  try {
    const failed = await waitForPost(request, id, p => p.targets.every(t => t.status !== "publishing"), "both settle");
    const byAccount = (p: Post, a: Account) => p.targets.find(t => t.account === a.id)!;
    expect(byAccount(failed, main).status).toBe("published");
    expect(byAccount(failed, long).status).toBe("failed");
    expect(await fakes.attempts()).toMatchObject({ [main.id]: 1, [long.id]: 1 });

    await fakes.up(long.id);
    const r = await request.post(`/v1/posts/${id}:retry`, { headers: json, data: { targets: [byAccount(failed, long).id] } });
    expect(r.status()).toBe(202);
    const done = await waitForPost(request, id, p => p.status === "published", "the retry goes out");
    expect(byAccount(done, long).status).toBe("published");
    const calls = (await fakes.calls()).filter(c => c.text.includes(m));
    expect(calls.map(c => c.account).sort()).toEqual([main.id, long.id].sort());
    expect(await fakes.attempts()).toMatchObject({ [main.id]: 1, [long.id]: 2 });
  } finally {
    await request.post(`/v1/posts/${id}:cancel`); // nothing left to retry in a later test
  }
});

test.describe("changing an account", () => {
  let restore: { id: string; label: string } | undefined;
  test.afterEach(async ({ request }) => {
    if (restore) {
      expect((await patchAccount(request, restore.id, { label: restore.label })).status()).toBe(200);
      restore = undefined;
    }
  });

  test("PATCH renames an account; a change based on an old read is refused", async ({ request }) => {
    const m = marker("label");
    const acct = await accountLabelled(request, "Bluesky (alt)");
    restore = { id: acct.id, label: acct.label };
    const read = await request.get(`/v1/accounts/${acct.id}`);
    const etag = read.headers()["etag"];

    const r = await patchAccount(request, acct.id, { label: `Alt ${m}` }, { "If-Match": etag });
    expect(r.status()).toBe(200);
    expect((await r.json()).label).toBe(`Alt ${m}`);
    expect(r.headers()["etag"]).not.toBe(etag);
    expect((await listAccounts(request)).find(a => a.id === acct.id)!.label).toBe(`Alt ${m}`);

    // The same ETag again: the account changed since that read.
    const stale = await patchAccount(request, acct.id, { label: "Lost update" }, { "If-Match": etag });
    expect(stale.status()).toBe(412);
    expect(stale.headers()["etag"]).toBe(r.headers()["etag"]);
    expect((await (await request.get(`/v1/accounts/${acct.id}`)).json()).label).toBe(`Alt ${m}`);

    // Refused values change nothing.
    const bad = await patchAccount(request, acct.id, { label: "" });
    expect(bad.status()).toBe(422);
    expect((await bad.json()).errors).toEqual([expect.objectContaining({ pointer: "/label" })]);
  });
});

test("a reply goes out from the Mastodon account named, which looks the post up on its own server", async ({ request, fakes }) => {
  const m = marker("reply");
  const main = await accountLabelled(request, "Mastodon");
  const long = await accountLabelled(request, "Mastodon (long posts)");
  const url = "https://elsewhere.example/@someone/1234";

  const res = await createPost(request, { reply_to: { url }, content: { text: `Reply ${m}` }, targets: [{ account: long.id }] },
    { "Prefer": "wait=30" });
  expect(res.status()).toBe(201);
  expect((await res.json()).status).toBe("published");
  const [call] = await fakes.waitForCalls(m, 1);
  // The status id is the one the replying account's server knows.
  expect(call).toMatchObject({ account: long.id, action: "reply", reply_to: `${long.id}/1234` });

  // The post exists once on its network: one of the accounts acts on it.
  const two = await createPost(request, { reply_to: { url }, content: { text: `Twice ${m}` }, targets: [{ account: main.id }, { account: long.id }] });
  expect(two.status()).toBe(422);
  expect((await two.json()).errors).toEqual([expect.objectContaining({ pointer: "/targets/1/account", code: "one_native_actor" })]);
});

test("/v1/resolve looks a post up with the account asked for", async ({ request }) => {
  const main = await accountLabelled(request, "Mastodon");
  const long = await accountLabelled(request, "Mastodon (long posts)");
  const old = await accountLabelled(request, "Mastodon (old)");
  const bsky = await accountLabelled(request, "Bluesky");
  const url = "https://elsewhere.example/@someone/42";
  const resolve = (data: object) => request.post("/v1/resolve", { headers: json, data });

  // Without one, the first connected account of the post's network.
  const first = await resolve({ url });
  expect(first.status()).toBe(200);
  expect(await first.json()).toMatchObject({ platform: "mastodon", account: main.id, text: `A post looked up by ${main.id}` });

  const named = await resolve({ url, account: long.id });
  expect(named.status()).toBe(200);
  expect(await named.json()).toMatchObject({ account: long.id, text: `A post looked up by ${long.id}` });

  for (const [account, code] of [[old.id, "account_disabled"], [bsky.id, "not_allowed"], ["acc_nosuchaccount", "unknown_account"]]) {
    const r = await resolve({ url, account });
    expect(r.status(), account).toBe(422);
    expect((await r.json()).errors).toEqual([expect.objectContaining({ pointer: "/account", code })]);
  }

  // A post the account's server doesn't have.
  const missing = await resolve({ url: "https://elsewhere.example/@someone/missing", account: long.id });
  expect(missing.status()).toBe(422);
  expect((await missing.json()).errors).toEqual([expect.objectContaining({ pointer: "/url", code: "unresolvable" })]);
});
