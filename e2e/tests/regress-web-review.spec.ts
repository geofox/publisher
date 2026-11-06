import { test, expect, ACCOUNT_ROWS, openApp, marker } from "./fixtures";
import type { Page } from "@playwright/test";

async function saveDraft(page: Page, text: string) {
  await page.locator("#master").fill(text);
  await page.locator("#draft-save").click();
  await expect(page.locator("#draft-list .draft-row", { hasText: text })).toBeVisible();
  await expect(page.locator("#draft-status")).toHaveText(/saved/);
}

test("edits discarded with New are not offered back for recovery", async ({ page }) => {
  const m = marker("rev1");
  page.on("dialog", d => d.accept());
  await openApp(page);
  await saveDraft(page, `orig ${m}`);
  await page.locator("#master").fill(`edited ${m}`);
  await page.waitForTimeout(600);
  await page.locator("#draft-new").click(); // confirm "discard" accepted
  await expect(page.locator("#master")).toHaveValue("");
  await page.reload();
  await expect(page.locator("#chips .target-row")).toHaveCount(ACCOUNT_ROWS);
  await page.waitForTimeout(300);
  await expect(page.locator("#draft-recovery-banner"), "discarded edits must not be offered back").toBeHidden();
});

test("a deleted draft's edits are not offered back for recovery", async ({ page }) => {
  const m = marker("rev2");
  page.on("dialog", d => d.accept());
  await openApp(page);
  await saveDraft(page, `orig ${m}`);
  await page.locator("#master").fill(`edited ${m}`);
  await page.waitForTimeout(600);
  await page.locator("#draft-delete").click();
  await expect(page.locator("#draft-list .draft-row", { hasText: m })).toHaveCount(0);
  await page.reload();
  await expect(page.locator("#chips .target-row")).toHaveCount(ACCOUNT_ROWS);
  await page.waitForTimeout(300);
  await expect(page.locator("#draft-recovery-banner"), "a deleted draft must not be offered back").toBeHidden();
});

test("restored edits of an open draft count as unsaved, even after Replace", async ({ page }) => {
  const m = marker("rev3");
  const dialogs: string[] = [];
  page.on("dialog", d => { dialogs.push(d.message()); d.accept(); });
  await openApp(page);
  await saveDraft(page, `orig ${m}`);
  await page.locator("#master").fill(`edited ${m}`);
  await page.waitForTimeout(600);
  await page.reload();
  await expect(page.locator("#chips .target-row")).toHaveCount(ACCOUNT_ROWS);
  await expect(page.locator("#draft-recovery-banner")).toBeVisible();
  await page.locator("#master").fill("typed meanwhile");
  await page.waitForTimeout(400);
  await page.locator("#draft-recovery-restore").click();
  await page.locator(".modal.confirm .danger").click(); // Replace
  await expect(page.locator("#master")).toHaveValue(`edited ${m}`);
  const dirty = await page.evaluate(async () => (await import("/state.js" as any)).state.dirty);
  await page.waitForTimeout(600);
  const rec = await page.evaluate(() => localStorage.getItem("compose_recovery_v1"));
  // Opening the saved draft from the list must ask before discarding the restored edits.
  await page.locator("#draft-list .draft-row", { hasText: m }).click();
  await page.waitForTimeout(500);
  const master = await page.locator("#master").inputValue();
  console.log(JSON.stringify({ dirty, recKept: !!rec, dialogs, master }));
  expect(dirty, "restored unsaved edits must count as unsaved").toBe(true);
});
