import { test, expect, openApp, switchTab, marker } from "./fixtures";
import type { Page } from "@playwright/test";

async function mockResolve(page: Page) {
  await page.route("**/v1/resolve", r => r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({
    platform: "mastodon", url: "https://mastodon.example/@a/1", text: "source post",
    author: { name: "A", handle: "@a@mastodon.example" }, media: [],
    actions: { reply: { allowed: true }, repost: { allowed: true }, quote: { allowed: true } },
  }) }));
}

test("a reply started while a saved draft is open doesn't use up that draft", async ({ page }) => {
  const m = marker("qadraft");
  await openApp(page);
  await page.locator("#master").fill(`Keep me ${m}`);
  await page.locator("#draft-save").click();
  await expect(page.locator("#draft-list .draft-row", { hasText: m })).toBeVisible();

  await mockResolve(page);
  let sentBody: any = null;
  await page.route("**/v1/posts", async r => {
    if (r.request().method() !== "POST") return r.continue();
    sentBody = r.request().postDataJSON();
    await r.fulfill({ status: 400, contentType: "application/json", body: JSON.stringify({ title: "stop", detail: "intercepted" }) });
  });
  await switchTab(page, "interact");
  await page.locator("#srcinput").fill("https://mastodon.example/@a/1");
  await page.locator(".act-btn", { hasText: "Reply" }).click();
  await page.locator(".modal .danger").click(); // Replace draft
  await expect(page.locator("#srcbanner")).toBeVisible();
  await page.locator("#master").fill("my reply");
  await page.locator("#submit").click();
  await expect.poll(() => sentBody).not.toBeNull();
  expect(sentBody.reply_to).toBeTruthy();
  // BUG: the reply carries the unrelated saved draft → POST /v1/posts deletes it.
  expect(sentBody.draft, "reply must not use up the unrelated saved draft").toBeUndefined();
});

test("restoring the unsaved-work snapshot keeps Bluesky/Threads reply settings", async ({ page }) => {
  const m = marker("qarec");
  await openApp(page);
  await page.locator("#master").fill(`Recover ${m}`);
  // Expand the Bluesky and Threads cards and restrict replies.
  await page.locator("#cards .ocard.p-bluesky .ocard-summary").click();
  await page.locator("#cards .ocard.p-bluesky select").nth(1).selectOption("nobody");
  await page.locator("#cards .ocard.p-threads .ocard-summary").click();
  await page.locator("#cards .ocard.p-threads select").selectOption("followers_only");
  await page.waitForTimeout(600); // autosave debounce
  await page.reload();
  await expect(page.locator("#chips .target-row")).toHaveCount(4);
  await expect(page.locator("#draft-recovery-banner")).toBeVisible();
  await page.locator("#draft-recovery-restore").click();
  await expect(page.locator("#master")).toHaveValue(`Recover ${m}`);
  await page.locator("#cards .ocard.p-bluesky .ocard-summary").click();
  await page.locator("#cards .ocard.p-threads .ocard-summary").click();
  await expect(page.locator("#cards .ocard.p-bluesky select").nth(1)).toHaveValue("nobody");
  await expect(page.locator("#cards .ocard.p-threads select")).toHaveValue("followers_only");
});
