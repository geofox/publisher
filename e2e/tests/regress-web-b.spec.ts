import { test, expect, openApp, switchTab, marker } from "./fixtures";
import type { Page } from "@playwright/test";

const SRC = {
  platform: "mastodon", url: "https://mastodon.example/@a/1", text: "source post",
  author: { name: "A", handle: "@a@mastodon.example" }, media: [],
  actions: { reply: { allowed: true }, repost: { allowed: true }, quote: { allowed: true } },
};

test("clearing the Interact input while a lookup is in flight leaves no stale card", async ({ page }) => {
  await page.route("**/v1/resolve", async r => {
    await new Promise(res => setTimeout(res, 1200));
    await r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(SRC) });
  });
  await openApp(page);
  await switchTab(page, "interact");
  await page.locator("#srcinput").fill("https://mastodon.example/@a/1");
  await page.waitForTimeout(500); // debounce fired, request in flight
  await page.locator("#srcinput").fill("");
  await page.waitForTimeout(1800);
  await expect(page.locator("#srcinput")).toHaveValue("");
  await expect(page.locator("#srccard .src-card"), "a cleared input must not show the old post").toHaveCount(0);
});

test("leaving a reply restores the Schedule label when a time is set", async ({ page }) => {
  await page.route("**/v1/resolve", r => r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(SRC) }));
  await openApp(page);
  const later = await page.evaluate(() => {
    const d = new Date(Date.now() + 2 * 86400e3); const pad = (n: number) => String(n).padStart(2, "0");
    return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
  });
  await page.locator("#schedat").fill(later);
  await expect(page.locator("#submit")).toHaveText("Schedule");
  await switchTab(page, "interact");
  await page.locator("#srcinput").fill("https://mastodon.example/@a/1");
  await page.locator(".act-btn", { hasText: "Reply" }).click();
  await page.locator(".srcb-x").click();
  await expect(page.locator("#schedat")).toHaveValue(later);
  await expect(page.locator("#submit")).toHaveText("Schedule");
});

test("a dropped preview request is retried (thread notice shows once the network is back)", async ({ page, consoleErrors }) => {
  let n = 0;
  await page.route("**/v1/posts:preview", r => { if (n++ === 0) return r.abort("internetdisconnected"); return r.continue(); });
  await openApp(page);
  await page.locator("#master").fill("word ".repeat(120)); // 600 chars: threads on Bluesky
  await page.waitForTimeout(2500);
  expect(n).toBeGreaterThanOrEqual(1);
  await expect(page.locator("#threadnotice .thread-notice")).toBeVisible({ timeout: 3000 });
  consoleErrors.splice(0, consoleErrors.length, ...consoleErrors.filter(e => !/ERR_INTERNET_DISCONNECTED/.test(e)));
});

test("a new token's one-time secret survives a stray tap on the backdrop", async ({ page }) => {
  await openApp(page);
  await switchTab(page, "tokens");
  await page.locator("#tok-name-input").fill(marker("tok"));
  await page.locator("#tok-create-btn").click();
  await expect(page.locator(".tok-secret-input")).toBeVisible();
  await page.mouse.click(5, 300); // backdrop
  await page.keyboard.press("Escape");
  await expect(page.locator(".tok-secret-input"), "the secret is shown only once").toBeVisible();
});

test("a11y: Escape closes a confirm dialog and focus moves into it", async ({ page }) => {
  await openApp(page);
  await switchTab(page, "tokens");
  await page.locator("#tok-name-input").fill(marker("tok2"));
  await page.locator("#tok-create-btn").click();
  await page.locator(".modal .primary", { hasText: "Done" }).click();
  await page.locator(".tok-row button", { hasText: "Revoke" }).first().click();
  await expect(page.locator(".modal.confirm")).toBeVisible();
  const inModal = await page.evaluate(() => !!document.activeElement?.closest(".modal"));
  const role = await page.locator(".modal.confirm").getAttribute("role");
  await page.keyboard.press("Escape");
  const open = await page.locator(".modal.confirm").count();
  console.log(JSON.stringify({ inModal, role, openAfterEscape: open }));
  expect({ inModal, role, open }).toEqual({ inModal: true, role: "dialog", open: 0 });
});
