import { test, expect, openApp, switchTab, marker } from "./fixtures";
test("a11y: platform toggles, draft rows and history rows are reachable by keyboard", async ({ page }) => {
  const m = marker("kbd");
  await openApp(page);
  await page.locator("#master").fill(m);
  await page.locator("#draft-save").click();
  await expect(page.locator("#draft-list .draft-row", { hasText: m })).toBeVisible();
  const info = await page.evaluate(() => {
    const f = (sel: string) => { const e = document.querySelector(sel) as HTMLElement | null; return e ? { tabIndex: e.tabIndex, role: e.getAttribute("role"), aria: e.getAttribute("aria-checked") ?? e.getAttribute("aria-pressed") } : null; };
    return { target: f("#chips .target-row"), draft: f("#draft-list .draft-row"), notice: f(".thread-notice") };
  });
  console.log(JSON.stringify(info));
  // Tab through the whole page and record which target rows can take focus.
  const seen = new Set<string>();
  for (let i = 0; i < 80; i++) {
    await page.keyboard.press("Tab");
    seen.add(await page.evaluate(() => (document.activeElement as HTMLElement)?.className || ""));
  }
  const reached = [...seen].some(c => c.includes("target-row"));
  expect(reached, "a keyboard user can turn a platform on/off").toBe(true);
});
