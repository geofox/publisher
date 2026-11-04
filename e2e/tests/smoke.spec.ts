import { test, expect, openApp, switchTab } from "./fixtures";

test("boots with all four platforms and switches tabs @mobile", async ({ page }) => {
  await openApp(page);
  await expect(page.locator("#targets-meta")).toHaveText("4 selected");
  for (const view of ["history", "tools", "verify", "interact", "compose"] as const) {
    await switchTab(page, view);
  }
  await page.screenshot({ path: test.info().outputPath("compose.png"), fullPage: true });
});
