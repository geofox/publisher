import { test, expect, openApp, switchTab } from "./fixtures";

test("boots with all four platforms and switches tabs @mobile", async ({ page }) => {
  await openApp(page);
  await expect(page.locator("#targets-meta")).toHaveText("4 selected");
  for (const view of ["history", "tools", "verify", "interact", "compose"] as const) {
    await switchTab(page, view);
  }
  await page.screenshot({ path: test.info().outputPath("compose.png"), fullPage: true });
});

test("a stale in-flight post from an earlier visit is dropped quietly", async ({ page }) => {
  await page.goto("/");
  await page.evaluate(() => localStorage.setItem("inflight_post_v1", "pst_doesnotexist"));
  await page.reload();
  await expect(page.locator("#chips .target-row")).toHaveCount(4);
  await expect.poll(() => page.evaluate(() => localStorage.getItem("inflight_post_v1"))).toBeNull();
  await expect(page.locator(".pmodal")).toHaveCount(0);
});
