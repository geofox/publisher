import { test, expect, openApp, switchTab, marker } from "./fixtures";
import type { APIRequestContext } from "@playwright/test";

// What the engine promises when things go wrong: a post is sent once however
// often it is submitted, a restart carries a thread on from the part it
// stopped at, an outcome nobody knows is the owner's to settle, and a
// refusal that would repeat is not retried.

type Account = { id: string; type: string; label: string; status: string; status_reason?: string; enabled: boolean };
type Item = { ordinal: number; status: string; error_class?: string; next_attempt_at?: string; attempts?: number };
type Target = { id: number; account: string; type: string; status: string; error_class?: string; gave_up_at?: string; items: Item[] };
type Post = { id: string; status: string; targets: Target[] };

async function accounts(request: APIRequestContext): Promise<Account[]> {
  return (await (await request.get("/v1/accounts")).json()).data;
}

/** The account each type posts as from the composer: the first connected one listed. */
async function connected(request: APIRequestContext, type: string): Promise<Account> {
  const a = (await accounts(request)).find(x => x.status === "connected" && x.type === type);
  expect(a, `a ${type} account`).toBeTruthy();
  return a!;
}

async function getPost(request: APIRequestContext, id: string): Promise<Post> {
  const r = await request.get(`/v1/posts/${id}`);
  expect(r.status()).toBe(200);
  return r.json();
}

async function createPost(request: APIRequestContext, body: object, headers: Record<string, string> = {}) {
  return request.post("/v1/posts", { headers: { "Content-Type": "application/json", ...headers }, data: body });
}

/** Poll the post until check passes; answers the post. */
async function waitForPost(request: APIRequestContext, id: string, check: (p: Post) => boolean, what: string): Promise<Post> {
  let p: Post | undefined;
  await expect.poll(async () => { p = await getPost(request, id); return check(p); }, { message: what, timeout: 10_000 }).toBe(true);
  return p!;
}

test("a post submitted again after its answer was lost is posted once", async ({ page, fakes, consoleErrors }) => {
  const m = marker("lost");
  await openApp(page);
  // The server saves the post, but the browser never sees the answer, as
  // with a dropped connection.
  let dropped = false;
  await page.route("**/v1/posts", async route => {
    if (route.request().method() !== "POST" || dropped) return route.fallback();
    dropped = true;
    await route.fetch();
    await route.abort("connectionfailed");
  });
  await page.locator("#master").fill(`Lost answer ${m}`);
  await page.locator("#submit").click();
  await expect.poll(() => dropped).toBe(true);
  await expect(page.locator("#submit")).toBeEnabled();
  // The browser reports the dropped request; that is the test's doing.
  consoleErrors.splice(0, consoleErrors.length, ...consoleErrors.filter(e => !/ERR_CONNECTION_FAILED|Failed to fetch/.test(e)));

  await page.locator("#submit").click(); // the same body, so the same key
  await expect(page.locator(".pmodal .pm-title")).toHaveText("✓ Posted");
  const calls = await fakes.waitForCalls(m, 4);
  await page.waitForTimeout(500); // a second post would have been sent by now
  expect((await fakes.calls()).filter(c => c.text.includes(m))).toHaveLength(calls.length);
  expect(calls).toHaveLength(4);
  const list = await (await page.request.get(`/v1/posts?q=${m}`)).json();
  expect(list.data).toHaveLength(1);
});

test("two submits racing with one key make one post", async ({ request, fakes }) => {
  const m = marker("race");
  const masto = await connected(request, "mastodon");
  const body = { content: { text: `Racing ${m}` }, targets: [{ account: masto.id }] };
  const key = { "Idempotency-Key": `"${m}"` };
  const [a, b] = await Promise.all([
    createPost(request, body, { ...key, Prefer: "wait=5" }),
    createPost(request, body, { ...key, Prefer: "wait=5" }),
  ]);
  // Each answer is the post, or "that key is in use" while the first waits.
  const ids = new Set<string>();
  for (const r of [a, b]) {
    if (r.status() === 409) {
      expect((await r.json()).code).toBe("idempotency_key_in_use");
      continue;
    }
    expect([201, 202]).toContain(r.status());
    ids.add((await r.json()).id);
  }
  expect(ids.size).toBe(1);
  await fakes.waitForCalls(m, 1);
  await new Promise(r => setTimeout(r, 300));
  expect((await fakes.calls()).filter(c => c.text.includes(m))).toHaveLength(1);
  expect((await (await request.get(`/v1/posts?q=${m}`)).json()).data).toHaveLength(1);
});

test("a restart in the middle of a thread carries on from the part it stopped at", async ({ request, fakes }) => {
  const m = marker("restart");
  const masto = await connected(request, "mastodon");
  await fakes.hold("mastodon", 1); // part 1 goes out; part 2 is in flight when the engine stops
  const r = await createPost(request, {
    content: { text: `${m} one\ntwo\nthree`, breaks: [`${m} one\n`.length, `${m} one\ntwo\n`.length] },
    targets: [{ account: masto.id }],
    threading: { numbering: false },
  });
  expect(r.status()).toBe(202);
  const { id } = await r.json();
  await waitForPost(request, id, p => p.targets[0].items[1]?.status === "sending", "part 2 is being sent");
  // Restart only once the call is at the platform (held), not merely claimed.
  await expect.poll(async () => (await fakes.attempts()).mastodon).toBe(2);

  await fakes.restart();
  // Mastodon drops a repeat of an idempotency key, so recovery sends the
  // part again (that the key is the same is pinned in the Go engine tests).
  const done = await waitForPost(request, id, p => p.status !== "publishing", "the thread settles");
  expect(done.status).toBe("published");
  expect(done.targets[0].items.map(i => i.status)).toEqual(["published", "published", "published"]);

  const calls = (await fakes.calls()).filter(c => c.platform === "mastodon");
  const thread = calls.slice(calls.findIndex(c => c.text.includes(m)));
  expect(thread.map(c => c.text)).toEqual([`${m} one`, "two", "three"]); // part 1 once, nothing lost
  expect(thread[1].reply_to).toBe(thread[0].remote_id);
  expect(thread[2].reply_to).toBe(thread[1].remote_id);
  expect((await fakes.attempts()).mastodon).toBe(4); // the held call, then parts 2 and 3
});

test("an item that may have been posted waits for the owner, who sends it again from History", async ({ page, request, fakes }) => {
  const m = marker("unknown");
  const bsky = await connected(request, "bluesky");
  await fakes.hold("bluesky", 1);
  const r = await createPost(request, {
    content: { text: `${m} first\nsecond\nthird`, breaks: [`${m} first\n`.length, `${m} first\nsecond\n`.length] },
    targets: [{ account: bsky.id }],
    threading: { numbering: false },
  });
  const { id } = await r.json();
  await waitForPost(request, id, p => p.targets[0].items[1]?.status === "sending", "part 2 is being sent");
  await expect.poll(async () => (await fakes.attempts()).bluesky).toBe(2);
  await fakes.restart();
  // Bluesky can't drop a repeat: the engine doesn't guess.
  const stuck = await waitForPost(request, id, p => p.targets[0].items[1].status === "unknown", "part 2 is unknown");
  expect(stuck.targets[0].items[2].status).toBe("planned");
  await page.waitForTimeout(300);
  expect((await fakes.calls()).filter(c => c.platform === "bluesky" && !c.text.includes(m))).toHaveLength(0);

  await openApp(page);
  await switchTab(page, "history");
  await page.locator(".hitem", { hasText: m }).click();
  const detail = page.locator("#hdetail");
  await expect(detail.getByText("Check Bluesky me.bsky.e2e.invalid: did this part go out?")).toBeVisible();
  await expect(detail.locator(".retry-status", { hasText: "it may have been posted: the server stopped while sending" })).toBeVisible();
  await detail.getByRole("button", { name: "It wasn't: send it again" }).click();
  await expect(detail.locator(".lbl", { hasText: "result · published" })).toBeVisible();

  const calls = (await fakes.calls()).filter(c => c.platform === "bluesky");
  const thread = calls.slice(calls.findIndex(c => c.text.includes(m)));
  expect(thread.map(c => c.text)).toEqual([`${m} first`, "second", "third"]);
  // Each part replies to the one before (the fake's Bluesky ids are at:// URIs).
  const uri = (c: { remote_id: string }) => `at://did:plc:e2e/app.bsky.feed.post/${c.remote_id}`;
  expect(thread[1].reply_to).toBe(uri(thread[0]));
  expect(thread[2].reply_to).toBe(uri(thread[1]));
});

test("a refusal that would repeat is not retried; a transient failure is", async ({ page, request, fakes }) => {
  const m = marker("invalid");
  const masto = await connected(request, "mastodon");
  const threads = await connected(request, "threads");
  const bsky = await connected(request, "bluesky");
  await fakes.invalid("mastodon");
  await fakes.down("threads");
  const r = await createPost(request, {
    content: { text: `Refused ${m}` },
    targets: [{ account: masto.id }, { account: threads.id }, { account: bsky.id }],
  });
  expect(r.status()).toBe(202);
  const { id } = await r.json();
  try {
    // Threads' retry is pending, so the post hasn't settled; wait for each
    // target's first outcome.
    const post = await waitForPost(request, id, p => p.targets.every(t => t.items[0].status !== "planned" && t.items[0].status !== "sending"),
      "every target has an outcome");
    expect(post.status).toBe("needs_attention"); // the refused target is the owner's now
    const by = (type: string) => post.targets.find(t => t.type === type)!;

    const refused = by("mastodon");
    expect(refused.status).toBe("failed");
    expect(refused.error_class).toBe("invalid");
    expect(refused.gave_up_at).toBeTruthy();
    expect(refused.items[0].next_attempt_at ?? null).toBeNull();

    const down = by("threads");
    expect(down.status).toBe("failed");
    expect(down.error_class).toBe("transient");
    expect(down.gave_up_at ?? null).toBeNull();
    expect(down.items[0].next_attempt_at).toBeTruthy(); // the engine will try again

    expect(by("bluesky").status).toBe("published");
    expect(await fakes.attempts()).toMatchObject({ mastodon: 1, threads: 1, bluesky: 1 });

    await openApp(page);
    await switchTab(page, "history");
    await page.locator(".hitem", { hasText: m }).click();
    await expect(page.locator("#hdetail").getByText("the network refused this content; it won't be retried")).toBeVisible();
  } finally {
    // Stop Threads' pending retry, so it can't land in a later test.
    expect((await request.post(`/v1/posts/${id}:cancel`)).status()).toBe(200);
  }
});

test("an account the environment no longer configures is listed but can't post; the composer posts as the first connected one", async ({ page, request, fakes }) => {
  const m = marker("second");
  const mastos = (await accounts(request)).filter(a => a.type === "mastodon");
  expect(mastos.map(a => [a.label, a.status])).toEqual([
    ["Mastodon", "connected"], ["Mastodon (long posts)", "connected"], ["Mastodon (old)", "not_configured"]]);
  const [main, , old] = mastos;

  // /v1 refuses to send as an account the server has no credentials for.
  for (const targets of [[{ account: old.id }], [{ account: main.id }, { account: old.id }]]) {
    const r = await createPost(request, { content: { text: `Old ${m}` }, targets });
    expect(r.status()).toBe(422);
    const errs = (await r.json()).errors as { pointer: string; code: string; detail: string }[];
    // Only that account is refused, wherever it is in the list, with its reason.
    expect(errs).toEqual([expect.objectContaining({ pointer: `/targets/${targets.length - 1}/account`, code: "account_disabled" })]);
    expect(errs[0].detail).toBe(`the account is not configured: ${old.status_reason}`);
  }

  // The composer still shows one row per type and posts as the first connected account.
  await openApp(page);
  await page.locator("#master").fill(`From the composer ${m}`);
  await page.locator("#submit").click();
  await expect(page.locator(".pmodal .pm-title")).toHaveText("✓ Posted");
  const calls = await fakes.waitForCalls(m, 4);
  expect(calls.filter(c => c.platform === "mastodon").map(c => c.account)).toEqual([main.id]);
  const list = await (await request.get(`/v1/posts?q=${m}`)).json();
  expect(list.data).toHaveLength(1);
  const post = await getPost(request, list.data[0].id);
  expect(post.targets.find(t => t.type === "mastodon")!.account).toBe(main.id);
});
