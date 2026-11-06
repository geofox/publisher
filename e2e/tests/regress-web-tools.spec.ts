import { test, expect, openApp, switchTab } from "./fixtures";
test("a failed Pull doesn't report 'done'", async ({ page, consoleErrors }) => {
  await openApp(page);
  await switchTab(page, "tools");
  await expect(page.locator("#syncrelays details")).toBeAttached();
  await page.route("**/nostr/relays", r => r.fulfill({ status: 503, contentType: "application/problem+json",
    body: JSON.stringify({ title: "Service Unavailable", detail: "home relay unreachable", status: 503 }) }));
  await page.locator("#syncpull").click();
  await expect(page.locator("#syncout .err")).toContainText("home relay unreachable");
  await page.waitForTimeout(300);
  const toasts = await page.locator(".toast").allTextContents();
  console.log(JSON.stringify(toasts));
  expect(toasts).not.toContain("pull: done");
  consoleErrors.splice(0, consoleErrors.length, ...consoleErrors.filter(e => !/status of 503/.test(e)));
});
