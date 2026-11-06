import { test, expect, openApp, marker } from "./fixtures";
test("a failed accounts read at boot is retried before Post refuses", async ({ page, fakes, consoleErrors }) => {
  let n = 0;
  await page.route("**/v1/accounts?*", r => (n++ === 0 ? r.abort("connectionreset") : r.continue()));
  await openApp(page);
  const m = marker("acc");
  await page.locator("#master").fill(`after a blip ${m}`);
  await page.waitForTimeout(500);
  await page.locator("#submit").click();
  await page.waitForTimeout(1500);
  const toasts = await page.locator(".toast").allTextContents();
  console.log(JSON.stringify({ n, toasts }));
  await fakes.waitForCalls(m, 1);
  // The dropped request logs a network error, and the preview made before
  // the accounts came back is refused (no account to plan for).
  consoleErrors.splice(0, consoleErrors.length, ...consoleErrors.filter(e => !/ERR_CONNECTION_RESET|status of 422/.test(e)));
});
