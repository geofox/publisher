import { test, expect, ACCOUNT_ROWS, openApp, switchTab, marker, listAccounts, type Call } from "./fixtures";
import type { Page } from "@playwright/test";

// The web app picks accounts, not networks (step 4, part A3): two accounts
// of a network each get a row, a tab, a Compare column, a limit, their own
// wording and their own post. The harness's accounts (e2e/harness/main.go):
// "Mastodon" (@me@one, 500), "Mastodon (long posts)" (@me@two, 1000, not
// selected by default), "Mastodon (old)" (not configured), and one or two
// of each other network.

type Ids = { main: string; long: string; old: string; bsky: string; alt: string; nostr: string; threads: string };
let ids: Ids;
test.beforeEach(async ({ request }) => {
  if (ids) return;
  const by = Object.fromEntries((await listAccounts(request)).map(a => [a.label, a.id]));
  ids = { main: by["Mastodon"], long: by["Mastodon (long posts)"], old: by["Mastodon (old)"],
    bsky: by["Bluesky"], alt: by["Bluesky (alt)"], nostr: by["Nostr"], threads: by["Threads"] };
});

const row = (page: Page, id: string) => page.locator(`#chips .target-row[data-account="${id}"]`);
const tab = (page: Page, id: string) => page.locator(`#editor .ed-tab[data-tab="${id}"]`);
const own = (page: Page) => page.locator("#editor .ed-platform textarea.ec-text");
const byAccount = (calls: Call[], id: string) => calls.filter(c => c.account === id);

/** Select exactly these accounts (each a click on its row). */
async function selectOnly(page: Page, want: string[]) {
  for (const id of Object.values(ids)) {
    const r = row(page, id);
    if ((await r.getAttribute("aria-disabled")) === "true") continue;
    const on = (await r.getAttribute("aria-checked")) === "true";
    if (on !== want.includes(id)) await r.click();
  }
  await expect(page.locator("#targets-meta")).toHaveText(`${want.length} selected`);
}

test("two accounts of a network: each has its own tab, limit and wording, and gets its own post", async ({ page, fakes }) => {
  const m = marker("two");
  await openApp(page);
  await selectOnly(page, [ids.main, ids.long]);

  // 700 characters: two posts at Mastodon's 500, one at the long account's 1000.
  await page.locator("#master").fill(`${m} ` + "Words that fill a post. ".repeat(28).trim());

  // Each row counts against its own account's limit.
  await expect(row(page, ids.main).locator(".t-count")).toHaveText(/^\d+\/500$/);
  await expect(row(page, ids.long).locator(".t-count")).toHaveText(/^\d+\/1000$/);

  // Both short handles read "@me": each tab shows its full handle, and says
  // which network it is to assistive technology.
  await expect(tab(page, ids.main).locator(".ed-tab-lbl")).toHaveText("@me@one.e2e.invalid");
  await expect(tab(page, ids.long).locator(".ed-tab-lbl")).toHaveText("@me@two.e2e.invalid");
  await expect(tab(page, ids.main)).toHaveAccessibleName("Mastodon, @me@one.e2e.invalid");
  await expect(tab(page, ids.long)).toHaveAttribute("role", "tab");

  // Compare: a column each, each with its own limit.
  await page.locator("#editor .ed-compare").click();
  const cmp = page.locator(".modal.cmp");
  await expect(cmp.locator(".cmp-head")).toHaveCount(2);
  await expect(cmp.locator(`.cmp-head[data-account="${ids.main}"]`)).toContainText("2 posts");
  await expect(cmp.locator(`.cmp-head[data-account="${ids.long}"]`)).toContainText("1 post");
  await expect(cmp.locator(`.cmp-cell[data-account="${ids.main}"] .pp-count`).first()).toHaveText(/\/500$/);
  await expect(cmp.locator(`.cmp-cell[data-account="${ids.long}"] .pp-count`)).toHaveText(/^\d+\/1000$/);
  await cmp.getByRole("button", { name: "Close" }).click();

  // The long account says it differently; the other keeps the shared text.
  await tab(page, ids.long).click();
  await page.getByRole("button", { name: "Edit wording for Mastodon @me@two.e2e.invalid" }).click();
  await own(page).fill(`Long account's own words ${m}`);
  await expect(tab(page, ids.long).locator(".ed-tab-mode")).toHaveText("✎");
  await expect(tab(page, ids.main).locator(".ed-tab-mode")).toHaveCount(0);

  await page.locator("#submit").click();
  await expect(page.locator(".pmodal .pm-title")).toHaveText("✓ Posted");
  const calls = await fakes.calls();
  expect(byAccount(calls, ids.long).map(c => c.text)).toEqual([`Long account's own words ${m}`]);
  const main = byAccount(calls, ids.main);
  expect(main).toHaveLength(2);
  expect(main[0].text.startsWith(m)).toBe(true);
  expect(main[1].action).toBe("reply"); // the second part answers the first
  expect(calls.filter(c => c.account !== ids.main && c.account !== ids.long)).toEqual([]);
});

test("one account's failed post is retried alone from History", async ({ page, fakes }) => {
  const m = marker("retry-acct");
  await fakes.down(ids.long);
  await openApp(page);
  await selectOnly(page, [ids.main, ids.long]);
  await page.locator("#master").fill(`Two Mastodon accounts ${m}`);
  await page.locator("#submit").click();
  const modal = page.locator(".pmodal");
  await expect(modal.locator(".pm-title")).toHaveText("⚠ Posted with issues");
  await modal.getByText("Open in history ↗").click();

  const detail = page.locator("#hdetail");
  const long = detail.locator(`.res[data-account="${ids.long}"]`);
  await expect(long.locator(".bad")).toContainText("is down");
  await expect(detail.locator(`.res[data-account="${ids.main}"] .retry`)).toHaveCount(0);

  await fakes.up(ids.long);
  await long.getByRole("button", { name: "Retry" }).click();
  await expect(page.getByText("Mastodon @me@two.e2e.invalid: retrying")).toBeVisible();
  await expect(detail.locator(".lbl", { hasText: "result · published" })).toBeVisible();
  const calls = (await fakes.calls()).filter(c => c.text.includes(m));
  expect(byAccount(calls, ids.main)).toHaveLength(1); // not sent again
  expect(byAccount(calls, ids.long)).toHaveLength(1);
});

test("unsaved work from before accounts comes back on the accounts that stand for its networks", async ({ page }) => {
  const m = marker("v2");
  await openApp(page);
  // A version-2 snapshot: targets and their settings keyed by network.
  await page.evaluate(m => localStorage.setItem("compose_recovery_v1", JSON.stringify({
    v: 2, master_text: `Shared words ${m}`, breaks: [], platforms: ["mastodon", "bluesky", "threads"],
    delay_seconds: 0, images: [], anchors: {}, number: true,
    overrides: {
      bluesky: { text: `Bluesky words ${m}`, langs: ["en"], bluesky_reply: "following" },
      mastodon: { text: null, visibility: "unlisted" },
      threads: { text: null },
    },
  })), m);
  await page.reload();
  await expect(page.locator("#chips .target-row")).toHaveCount(ACCOUNT_ROWS);
  await page.locator("#draft-recovery-restore").click();
  await expect(page.locator("#master")).toHaveValue(`Shared words ${m}`);

  // Each network went to the account a new post selects there.
  for (const id of [ids.main, ids.bsky, ids.threads]) await expect(row(page, id)).toHaveAttribute("aria-checked", "true");
  for (const id of [ids.long, ids.alt, ids.nostr]) await expect(row(page, id)).toHaveAttribute("aria-checked", "false");
  await expect(page.locator("#targets-meta")).toHaveText("3 selected");

  // Bluesky's own text came with it, on that account's tab.
  await tab(page, ids.bsky).click();
  await expect(own(page)).toHaveValue(`Bluesky words ${m}`);
  await tab(page, ids.main).click();
  await expect(own(page)).toHaveCount(0);
});

test("a reply saved before accounts comes back as a reply by the account that stands for its network", async ({ page, fakes }) => {
  const m = marker("v2reply");
  await openApp(page);
  await page.evaluate(m => localStorage.setItem("compose_recovery_v1", JSON.stringify({
    v: 2, master_text: `My answer ${m}`, breaks: [], platforms: ["mastodon"], delay_seconds: 0,
    images: [], anchors: {}, number: true, overrides: { mastodon: { text: null } },
    interaction: { action: "reply", platform: "mastodon", sourceInput: "https://elsewhere.example/@someone/42",
      prevPlatforms: ["mastodon", "bluesky"] },
  })), m);
  await page.reload();
  await expect(page.locator("#chips .target-row")).toHaveCount(ACCOUNT_ROWS);
  await page.locator("#draft-recovery-restore").click();
  await expect(page.locator("#srcbanner")).toBeVisible();
  await expect(page.locator("#targets-meta")).toHaveText("1 selected");
  await expect(row(page, ids.main)).toHaveAttribute("aria-checked", "true");

  await page.locator("#submit").click();
  await expect(page.locator(".pmodal .pm-title")).toHaveText("✓ Posted");
  const [reply] = await fakes.waitForCalls(m, 1);
  expect(reply).toMatchObject({ account: ids.main, action: "reply", text: `My answer ${m}` });
  expect(reply.reply_to).toContain(ids.main); // the post as the replying account's server knows it
  await page.locator(".pmodal").getByRole("button", { name: "Done" }).click();

  // Afterwards the composer goes back to what was selected before the reply.
  await expect(page.locator("#srcbanner")).toBeHidden();
  await expect(page.locator("#targets-meta")).toHaveText("2 selected");
  await expect(row(page, ids.bsky)).toHaveAttribute("aria-checked", "true");
});

test("a draft for two accounts of a network keeps both, and each one's wording", async ({ page, fakes }) => {
  const m = marker("draft2");
  await openApp(page);
  await selectOnly(page, [ids.main, ids.long]);
  await page.locator("#master").fill(`Draft for both ${m}`);
  await tab(page, ids.long).click();
  await page.getByRole("button", { name: "Edit wording for Mastodon @me@two.e2e.invalid" }).click();
  await own(page).fill(`Long wording ${m}`);
  await page.locator("#draft-save").click();
  await expect(page.locator("#draft-list .draft-row", { hasText: m })).toBeVisible();

  await page.reload();
  await expect(page.locator("#chips .target-row")).toHaveCount(ACCOUNT_ROWS);
  await page.locator("#draft-list .draft-row", { hasText: m }).click();
  await expect(page.locator("#master")).toHaveValue(`Draft for both ${m}`);
  await expect(page.locator("#targets-meta")).toHaveText("2 selected");
  await expect(row(page, ids.main)).toHaveAttribute("aria-checked", "true");
  await expect(row(page, ids.long)).toHaveAttribute("aria-checked", "true");
  await tab(page, ids.long).click();
  await expect(own(page)).toHaveValue(`Long wording ${m}`);
  await tab(page, ids.main).click();
  await expect(own(page)).toHaveCount(0);

  page.once("dialog", d => d.accept());
  await page.locator("#draft-delete").click();
  await expect(page.locator("#draft-list .draft-row", { hasText: m })).toHaveCount(0);
  expect(await fakes.calls()).toEqual([]);
});

test("the account rows: grouped by network, a reason for one that can't post, and Select all", async ({ page, request }) => {
  await openApp(page);
  const old = (await listAccounts(request)).find(a => a.id === ids.old)!;
  expect(old.status).toBe("not_configured");

  const group = page.locator("#chips .target-group.p-mastodon");
  await expect(group).toHaveAttribute("role", "group");
  await expect(group.locator(".tg-head")).toContainText("3 accounts");
  const r = row(page, ids.old);
  await expect(r).toHaveClass(/\bunavailable\b/);
  await expect(r).toHaveAttribute("aria-disabled", "true");
  await expect(r.locator(".t-reason")).toHaveText(old.status_reason || "not configured");
  await expect(r.locator(".t-reason")).toBeVisible();
  await expect(r.locator(".t-handle")).toHaveText("@old@one.e2e.invalid");

  // Select all takes the two that can post; the third stays out.
  await group.getByRole("button", { name: "Select all Mastodon accounts" }).click();
  await expect(row(page, ids.main)).toHaveAttribute("aria-checked", "true");
  await expect(row(page, ids.long)).toHaveAttribute("aria-checked", "true");
  await expect(r).toHaveAttribute("aria-checked", "false");
  await group.getByRole("button", { name: "Clear all Mastodon accounts" }).click();
  await expect(row(page, ids.main)).toHaveAttribute("aria-checked", "false");
  await expect(row(page, ids.long)).toHaveAttribute("aria-checked", "false");
  // A network with one account has no header.
  await expect(page.locator("#chips .target-group.p-threads .tg-head")).toHaveCount(0);
});

test("Interact: the account that replies can be picked when the network has several", async ({ page, fakes }) => {
  const m = marker("pick");
  const asked: (string | undefined)[] = [];
  page.on("request", r => { if (r.url().endsWith("/v1/resolve")) asked.push(r.postDataJSON().account); });
  await openApp(page);
  await switchTab(page, "interact");
  await page.locator("#srcinput").fill("https://elsewhere.example/@someone/42");
  const pick = page.locator("#srcacct");
  await expect(pick).toBeVisible();
  // The two connected Mastodon accounts; the one not configured isn't offered.
  await expect(pick.locator("option")).toHaveText(["Mastodon @me@one.e2e.invalid", "Mastodon @me@two.e2e.invalid"]);
  await expect(pick).toHaveValue(ids.main);
  await expect(page.locator("#srccard .src-text")).toHaveText(`A post looked up by ${ids.main}`);

  await pick.selectOption(ids.long);
  await expect(page.locator("#srccard .src-text")).toHaveText(`A post looked up by ${ids.long}`);
  expect(asked).toEqual([undefined, ids.long]);

  await page.locator(".act-btn", { hasText: "Reply" }).click();
  await expect(page.locator("#srcbanner")).toBeVisible();
  await expect(page.locator("#targets-meta")).toHaveText("1 selected");
  await expect(row(page, ids.long)).toHaveAttribute("aria-checked", "true");
  // The other Mastodon account can't join: one account replies.
  await expect(row(page, ids.main)).toHaveClass(/\bbarred\b/);
  await page.locator("#master").fill(`Answer from the long account ${m}`);
  await page.locator("#submit").click();
  await expect(page.locator(".pmodal .pm-title")).toHaveText("✓ Posted");
  const [reply] = await fakes.waitForCalls(m, 1);
  expect(reply).toMatchObject({ account: ids.long, action: "reply" });
  expect(reply.reply_to).toContain(ids.long);
});

test("tab keys: arrows move between tabs, Alt+number picks one", async ({ page }) => {
  await openApp(page);
  await selectOnly(page, [ids.main, ids.long, ids.bsky]);
  await page.locator("#master").fill("Some words");
  // Tabs follow the accounts' order (the server's `order`).
  const order = (await listAccounts(page.request)).filter(a => [ids.main, ids.long, ids.bsky].includes(a.id))
    .sort((a, b) => a.order - b.order).map(a => a.id);
  const tabs = page.locator("#editor .ed-tab");
  await expect(tabs).toHaveCount(4);
  expect(await tabs.evaluateAll(ts => ts.map(t => (t as HTMLElement).dataset.tab))).toEqual(["shared", ...order]);

  await tab(page, "shared").focus();
  await page.keyboard.press("ArrowRight");
  await expect(tab(page, order[0])).toHaveAttribute("aria-selected", "true");
  await expect(tab(page, order[0])).toBeFocused();
  await page.keyboard.press("End");
  await expect(tab(page, order[2])).toBeFocused();
  await page.keyboard.press("ArrowRight"); // wraps
  await expect(tab(page, "shared")).toBeFocused();
  await page.keyboard.press("ArrowLeft");
  await expect(tab(page, order[2])).toHaveAttribute("aria-selected", "true");
  await page.keyboard.press("Home");
  await expect(tab(page, "shared")).toHaveAttribute("aria-selected", "true");
  // Only the selected tab is in the Tab order.
  await expect(page.locator('#editor .ed-tab[tabindex="0"]')).toHaveCount(1);

  await page.keyboard.press("Alt+3"); // from the tab row; a text field takes it too
  await expect(tab(page, order[1])).toHaveAttribute("aria-selected", "true");
  await page.locator("#master").focus();
  await page.keyboard.press("Alt+1");
  await expect(tab(page, "shared")).toHaveAttribute("aria-selected", "true");
  await page.keyboard.press("Alt+3");
  await expect(tab(page, order[1])).toHaveAttribute("aria-selected", "true");
  await expect(tab(page, order[1])).toHaveAttribute("title", /\(Alt\+3\)$/);
});

test("History filters by account", async ({ page, fakes }) => {
  const a = marker("hist-main"), b = marker("hist-long");
  await openApp(page);
  await selectOnly(page, [ids.main]);
  await page.locator("#master").fill(`From the main account ${a}`);
  await page.locator("#submit").click();
  await expect(page.locator(".pmodal .pm-title")).toHaveText("✓ Posted");
  await page.locator(".pmodal").getByRole("button", { name: "Done" }).click();
  await selectOnly(page, [ids.long]);
  await page.locator("#master").fill(`From the long account ${b}`);
  await page.locator("#submit").click();
  await expect(page.locator(".pmodal .pm-title")).toHaveText("✓ Posted");
  await page.locator(".pmodal").getByRole("button", { name: "Done" }).click();
  await fakes.waitForCalls(b, 1);

  await switchTab(page, "history");
  const pick = page.locator("#hacct");
  await expect(pick).toBeVisible();
  await expect(page.locator(".hitem", { hasText: a })).toBeVisible();
  await expect(page.locator(".hitem", { hasText: b })).toBeVisible();
  await expect(page.locator(".hitem", { hasText: b }).locator(`.pill[data-account="${ids.long}"]`)).toHaveText("Mastodon @me@two.e2e.invalid ✓");
  await pick.selectOption(ids.long);
  await expect(page.locator(".hitem", { hasText: a })).toHaveCount(0);
  await expect(page.locator(".hitem", { hasText: b })).toBeVisible();
  await pick.selectOption("");
  await expect(page.locator(".hitem", { hasText: a })).toBeVisible();
});

test("the tab row scrolls on a phone and keeps the active tab in view @mobile", async ({ page }) => {
  await openApp(page);
  await selectOnly(page, [ids.nostr, ids.main, ids.long, ids.bsky, ids.alt, ids.threads]);
  await page.locator("#master").fill("Some words for every account");
  const tabs = page.locator("#editor .ed-tabs");
  await expect(page.locator("#editor .ed-tab")).toHaveCount(7);
  // The page itself never scrolls sideways.
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  const wide = await tabs.evaluate(t => t.scrollWidth > t.clientWidth);
  const last = page.locator("#editor .ed-tab").last();
  await last.click();
  await expect(last).toHaveAttribute("aria-selected", "true");
  // The active tab sits inside the row's visible box.
  await expect.poll(async () => {
    const [row, t] = [await tabs.boundingBox(), await last.boundingBox()];
    return !!row && !!t && t.x >= row.x - 1 && t.x + t.width <= row.x + row.width + 1;
  }).toBe(true);
  if (test.info().project.name === "mobile-chromium") expect(wide, "seven tabs don't fit a phone's width").toBe(true);
});
