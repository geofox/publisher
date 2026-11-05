import { test, expect, openApp, switchTab, marker } from "./fixtures";

test("saves a draft, survives a reload, reopens it and deletes it", async ({ page, fakes }) => {
  const m = marker("draft");
  const text = `Draft body ${m}\n---\nsecond part`;
  await openApp(page);
  await page.locator("#master").fill(text);
  const save = page.locator("#draft-save");
  await expect(save).toBeEnabled();
  await save.click();
  const row = page.locator("#draft-list .draft-row", { hasText: m });
  await expect(row).toBeVisible();

  // A fresh page load reads drafts from the server, not from memory.
  await page.reload();
  await expect(page.locator("#chips .target-row")).toHaveCount(4);
  await page.locator("#master").fill(""); // nothing unsaved → no discard prompt
  await page.locator("#draft-list .draft-row", { hasText: m }).click();
  await expect(page.locator("#master")).toHaveValue(text);
  await expect(page.locator("#preview .pv-seg")).toHaveCount(2);

  page.once("dialog", d => d.accept());
  await page.locator("#draft-delete").click();
  await expect(page.locator("#draft-list .draft-row", { hasText: m })).toHaveCount(0);

  // Saving a draft never publishes anything.
  expect(await fakes.calls()).toEqual([]);
});

test("schedules a post for later and cancels it from History", async ({ page, fakes }) => {
  const m = marker("sched");
  await openApp(page);
  await page.locator("#master").fill(`Scheduled ${m}`);

  // datetime-local wants local wall time; the browser runs in Europe/Paris.
  const later = await page.evaluate(() => {
    const d = new Date(Date.now() + 2 * 24 * 3600 * 1000);
    d.setSeconds(0, 0);
    const pad = (n: number) => String(n).padStart(2, "0");
    return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
  });
  await page.locator("#schedat").fill(later);
  await expect(page.locator("#submit")).toHaveText("Schedule");
  await page.locator("#submit").click();
  await expect(page.getByText(/^Scheduled for /)).toBeVisible();
  await expect(page.locator("#submit")).toHaveText("Post");

  await switchTab(page, "history");
  await page.locator('#hseg button[data-status="scheduled"]').click();
  const item = page.locator(".hitem", { hasText: m });
  await expect(item.locator(".sched-when")).toContainText("scheduled");
  await item.click();
  await page.locator("#hdetail").getByRole("button", { name: "cancel" }).click();
  await expect(page.getByText("canceled", { exact: true })).toBeVisible();
  await expect(page.locator(".hitem", { hasText: m })).toHaveCount(0);

  // Nothing was ever sent.
  expect(await fakes.calls()).toEqual([]);

  // The canceled post stays in the archive and can still be sent by hand;
  // the detail follows it to the result, although its event stream starts
  // with the old "completed" of the cancel.
  await page.locator('#hseg button[data-status="all"]').click();
  await page.locator(".hitem", { hasText: m }).click();
  const detail = page.locator("#hdetail");
  await expect(detail.locator(".lbl", { hasText: "result · canceled" })).toBeVisible();
  const row = detail.locator(".res", { has: page.locator(".bdg", { hasText: "mastodon" }) });
  await row.getByRole("button", { name: "Retry" }).click();
  await expect(detail.locator(".res .bdg", { hasText: "Mastodon ✓" })).toBeVisible();
  await expect(page.locator(".hitem", { hasText: m }).locator(".pill.s", { hasText: "Mastodon" })).toBeVisible();
  const sent = await fakes.calls();
  expect(sent.map(c => c.platform)).toEqual(["mastodon"]);
});
