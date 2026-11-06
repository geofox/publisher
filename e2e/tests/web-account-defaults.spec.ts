import { test, expect, openApp, switchTab } from "./fixtures";

// What the web app shows from the accounts and the instance: a new post
// selects the networks with a connected account that new posts select, and
// controls the server can't serve stay hidden. The answers are altered in
// the browser, so nothing changes on the server.

type Acct = { type: string; status: string; default_selected: boolean };

test("a new post selects only networks with a connected account that new posts select", async ({ page }) => {
  await page.route("**/v1/accounts?*", async r => {
    const res = await r.fetch();
    const body = await res.json();
    for (const a of body.data as Acct[]) {
      if (a.type === "bluesky") a.default_selected = false; // the owner unselected both
      if (a.type === "threads") a.status = "not_configured";
    }
    await r.fulfill({ response: res, json: body });
  });
  await openApp(page);
  const row = (p: string) => page.locator(`#chips .target-row.p-${p}`);
  await expect(row("bluesky")).toHaveClass(/\boff\b/);
  await expect(row("threads")).toHaveClass(/\boff\b/);
  await expect(row("mastodon")).not.toHaveClass(/\boff\b/);
  await expect(row("nostr")).not.toHaveClass(/\boff\b/);

  // Ticking a network with no connected account is refused at Post, saying so.
  await row("threads").click();
  await page.locator("#master").fill("hello");
  await page.locator("#submit").click();
  await expect(page.locator(".toast").last()).toContainText("Threads has no connected account");
});

test("without a media store or relay sync, their controls are hidden", async ({ page }) => {
  await page.route("**/v1/instance", async r => {
    const res = await r.fetch();
    const body = await res.json();
    body.features = { ...body.features, media: false, relay_sync: false };
    await r.fulfill({ response: res, json: body });
  });
  await openApp(page);
  await expect(page.locator("#addimg")).toBeHidden();
  await expect(page.locator("#vidpreset")).toBeHidden();
  await expect(page.locator("#media-meta")).toContainText("no media store");

  let relayCalls = 0;
  await page.route("**/nostr/relays*", r => { relayCalls++; return r.continue(); });
  await switchTab(page, "tools");
  await expect(page.locator("#syncrelays")).toContainText("needs a connected Nostr account");
  await expect(page.locator("#syncscan")).toBeHidden();
  await expect(page.locator("#reconcile-head")).toBeHidden();
  expect(relayCalls).toBe(0);
});
