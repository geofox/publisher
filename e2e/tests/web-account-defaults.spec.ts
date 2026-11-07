import { test, expect, openApp, rowOf, switchTab } from "./fixtures";

// What the web app shows from the accounts and the instance: a new post
// selects the connected accounts that new posts select, and controls the
// server can't serve stay hidden. The answers are altered in
// the browser, so nothing changes on the server.

type Acct = { type: string; status: string; status_reason?: string; default_selected: boolean };

test("a new post selects the connected accounts that new posts select; one that isn't connected says why", async ({ page }) => {
  await page.route("**/v1/accounts?*", async r => {
    const res = await r.fetch();
    const body = await res.json();
    for (const a of body.data as Acct[]) {
      if (a.type === "bluesky") a.default_selected = false; // the owner unselected both
      if (a.type === "threads") { a.status = "not_configured"; a.status_reason = "set THREADS_ACCESS_TOKEN to use it again"; }
    }
    await r.fulfill({ response: res, json: body });
  });
  await openApp(page);
  await expect(await rowOf(page, "Bluesky")).toHaveClass(/\boff\b/);
  await expect(await rowOf(page, "Bluesky (alt)")).toHaveClass(/\boff\b/);
  await expect(await rowOf(page, "Mastodon")).not.toHaveClass(/\boff\b/);
  await expect(await rowOf(page, "Mastodon (long posts)")).toHaveClass(/\boff\b/); // not selected by default
  await expect(await rowOf(page, "Nostr")).not.toHaveClass(/\boff\b/);
  await expect(page.locator("#targets-meta")).toHaveText("2 selected");

  // An account that isn't connected shows why, as text, and can't be ticked.
  const threads = await rowOf(page, "Threads");
  await expect(threads).toHaveClass(/\bunavailable\b/);
  await expect(threads.locator(".t-reason")).toHaveText("set THREADS_ACCESS_TOKEN to use it again");
  await expect(threads).toHaveAttribute("aria-disabled", "true");
  await expect(threads).toHaveAccessibleDescription("set THREADS_ACCESS_TOKEN to use it again");
  await threads.click({ force: true }); // Playwright waits for aria-disabled ones otherwise
  await threads.press("Space");
  await expect(threads).toHaveAttribute("aria-checked", "false");
  await expect(page.locator("#targets-meta")).toHaveText("2 selected");
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
