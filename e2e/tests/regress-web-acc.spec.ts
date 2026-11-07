import { test, expect, ACCOUNT_ROWS, marker, switchTab } from "./fixtures";
import type { Page } from "@playwright/test";

// The accounts can't be read at boot (a blip): the composer says so, keeps
// trying (2 s after the first failure, then 4 s after that, …), and Post or
// Try again reads them at once.

/**
 * Fail every read of the accounts until ok() is called. afterRetry() waits
 * for the composer's own first try again (2 s after the boot read; other
 * views read at boot too): the next is 4 s away, so a read that follows
 * at once is the one the test caused.
 */
async function failAccounts(page: Page) {
  let failing = true;
  const at: number[] = [];
  await page.route("**/v1/accounts?*", r => { at.push(Date.now()); return failing ? r.abort("connectionreset") : r.continue(); });
  return {
    ok: () => { failing = false; },
    afterRetry: () => expect.poll(() => at.some(t => t - at[0] >= 1800), { timeout: 5000 }).toBe(true),
  };
}

test("Post reads the accounts again when none could be read at boot", async ({ page, fakes, consoleErrors }) => {
  const reads = await failAccounts(page);
  await page.goto("/");
  const chips = page.locator("#chips");
  await expect(chips).toHaveAttribute("data-state", "error");
  await expect(chips).toContainText("Couldn't read your accounts");
  const m = marker("acc");
  await page.locator("#master").fill(`after a blip ${m}`);
  await reads.afterRetry();
  reads.ok();
  await page.locator("#submit").click();
  await expect(page.locator(".pmodal .pm-title")).toHaveText("✓ Posted", { timeout: 3000 });
  expect((await fakes.waitForCalls(m, 4)).length).toBe(4); // the four accounts a new post selects
  await expect(page.locator("#chips .target-row")).toHaveCount(ACCOUNT_ROWS);
  // The dropped requests log network errors, and the preview made before
  // the accounts came back is refused (no account to plan for).
  consoleErrors.splice(0, consoleErrors.length, ...consoleErrors.filter(e => !/ERR_CONNECTION_RESET|status of 422/.test(e)));
});

test("Try again reads the accounts at once", async ({ page, consoleErrors }) => {
  const reads = await failAccounts(page);
  await page.goto("/");
  const chips = page.locator("#chips");
  await expect(chips).toHaveAttribute("data-state", "error");
  await reads.afterRetry();
  reads.ok();
  await chips.getByRole("button", { name: "Try again" }).click();
  await expect(page.locator("#chips .target-row")).toHaveCount(ACCOUNT_ROWS, { timeout: 2000 });
  await expect(chips).toHaveAttribute("data-state", "ready");
  await expect(page.locator("#targets-meta")).toHaveText("4 selected");
  consoleErrors.splice(0, consoleErrors.length, ...consoleErrors.filter(e => !/ERR_CONNECTION_RESET|status of 422/.test(e)));
});

test("a reply started before the accounts could be read gets its network's settings", async ({ page, consoleErrors }) => {
  const reads = await failAccounts(page);
  let sent: any = null;
  await page.route("**/v1/posts", async r => {
    if (r.request().method() !== "POST") return r.continue();
    sent = r.request().postDataJSON();
    await r.fulfill({ status: 400, contentType: "application/problem+json", body: JSON.stringify({ title: "stop", detail: "intercepted" }) });
  });
  await page.goto("/");
  await expect(page.locator("#chips")).toHaveAttribute("data-state", "error");
  await switchTab(page, "interact");
  await page.locator("#srcinput").fill("https://elsewhere.example/@someone/42");
  await page.locator(".act-btn", { hasText: "Reply" }).click();
  await expect(page.locator("#srcbanner")).toBeVisible();
  await page.locator("#master").fill("my answer");
  reads.ok();
  await page.locator("#submit").click();
  await expect.poll(() => sent).not.toBeNull();
  // Mastodon's language, as for any reply (the operator's first language).
  expect(sent.targets).toEqual([expect.objectContaining({ content: { languages: ["en"] } })]);
  consoleErrors.splice(0, consoleErrors.length, ...consoleErrors.filter(e => !/ERR_CONNECTION_RESET|status of 4(00|22)/.test(e)));
});

test("Try again keeps the keyboard when the read fails again", async ({ page, consoleErrors }) => {
  await failAccounts(page);
  await page.goto("/");
  const again = page.locator("#chips").getByRole("button", { name: "Try again" });
  await again.focus();
  await page.keyboard.press("Enter");
  await expect(page.locator("#chips")).toHaveAttribute("data-state", "error");
  await expect(again).toBeFocused();
  await expect(page.locator("#chips-status")).toContainText("Couldn't read your accounts");
  consoleErrors.splice(0, consoleErrors.length, ...consoleErrors.filter(e => !/ERR_CONNECTION_RESET|status of 422/.test(e)));
});
