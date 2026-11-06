import { test, expect, ACCOUNT_ROWS, openApp, switchTab, marker, mastodonSource } from "./fixtures";


test("unsaved edits to an opened draft survive a reload (recovery)", async ({ page }) => {
  const m = marker("edit");
  await openApp(page);
  await page.locator("#master").fill(`v1 ${m}`);
  await page.locator("#draft-save").click();
  await expect(page.locator("#draft-status")).toHaveText("saved just now");
  await page.locator("#master").fill(`v2 ten minutes of edits ${m}`);
  await expect(page.locator("#draft-status")).toHaveText("unsaved changes");
  await page.waitForTimeout(600);
  await page.reload(); // the phone dropped the PWA, a crash, a mis-tap on reload
  await expect(page.locator("#chips .target-row")).toHaveCount(ACCOUNT_ROWS);
  const banner = await page.locator("#draft-recovery-banner").isVisible();
  const stored = await page.evaluate(() => localStorage.getItem("compose_recovery_v1"));
  console.log(JSON.stringify({ banner, stored }));
  expect(banner || (stored || "").includes("v2 ten minutes"), "edits to a loaded draft are recoverable").toBe(true);
});

test("restoring a reply in progress keeps it a reply", async ({ page }) => {
  const SRC = await mastodonSource(page.request);
  await page.route("**/v1/resolve", r => r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(SRC) }));
  await openApp(page);
  await switchTab(page, "interact");
  await page.locator("#srcinput").fill("https://mastodon.example/@a/1");
  await page.locator(".act-btn", { hasText: "Reply" }).click();
  await page.locator("#master").fill("my half-written reply");
  await page.waitForTimeout(600);
  await page.reload();
  await expect(page.locator("#chips .target-row")).toHaveCount(ACCOUNT_ROWS);
  await page.locator("#draft-recovery-restore").click();
  await expect(page.locator("#master")).toHaveValue("my half-written reply");
  console.log("submit:", await page.locator("#submit").textContent(), "banner hidden:", await page.locator("#srcbanner").isHidden());
  await expect(page.locator("#srcbanner")).toBeVisible();
  await expect(page.locator("#submit")).toHaveText("Reply");
});
