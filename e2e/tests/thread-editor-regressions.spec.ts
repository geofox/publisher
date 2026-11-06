import path from "node:path";
import { test, expect, openApp, marker } from "./fixtures";
import type { Page } from "@playwright/test";

// Regressions found reviewing the thread editor (editor.js).

const shared = (page: Page) => page.locator("#editor .ed-shared textarea.ec-text");
const tab = (page: Page, name: string) => page.locator(`#editor .ed-tab[data-tab="${name}"]`);
const posts = (page: Page) => page.locator("#editor .ed-platform .pp-post");

test("text typed at the end of a card stays in it (split at the end)", async ({ page }) => {
  await openApp(page);
  const first = page.locator("#master");
  await first.fill("hello");
  await first.press("End");
  await first.press("Control+Enter");
  await expect(shared(page)).toHaveCount(2);
  await shared(page).nth(0).click();
  await shared(page).nth(0).press("End");
  await page.keyboard.type(" world");
  await tab(page, "bluesky").click();
  await tab(page, "shared").click();
  await expect(shared(page).nth(0)).toHaveValue("hello world");
  await expect(shared(page).nth(1)).toHaveValue("");
});

test("text typed at the end of a card stays in it (split mid-word)", async ({ page }) => {
  await openApp(page);
  const first = page.locator("#master");
  await first.fill("helloworld");
  await first.evaluate((ta: HTMLTextAreaElement) => ta.setSelectionRange(5, 5));
  await first.press("Control+Enter");
  await expect(shared(page)).toHaveCount(2);
  await shared(page).nth(0).click();
  await shared(page).nth(0).press("End");
  await page.keyboard.type("!");
  await tab(page, "bluesky").click();
  await expect(posts(page).first()).toContainText("hello!");
  await tab(page, "shared").click();
  const vals = await shared(page).evaluateAll(ts => ts.map(t => (t as HTMLTextAreaElement).value));
  expect(vals).toEqual(["hello!", "world"]);
});

test("a draft with CRLF line breaks edits without drift", async ({ page }) => {
  const m = marker("crlf");
  await openApp(page);
  const accts = await (await page.request.get("/v1/accounts")).json();
  const acct = (accts.data || accts).find((a: any) => a.type === "bluesky");
  const text = `${m} line one\r\nline two\r\nline three`;
  const res = await page.request.post("/v1/drafts", { data: { post: { content: { text }, targets: [{ account: acct.id }] } } });
  expect(res.status(), await res.text()).toBeLessThan(300);
  await page.reload();
  await expect(page.locator("#chips .target-row")).toHaveCount(4);
  await page.locator("#draft-list .draft-row", { hasText: m }).click();
  await expect(page.locator("#master")).toHaveValue(`${m} line one\nline two\nline three`); // loaded, as LF
  await page.locator("#master").press("Control+End");
  await page.keyboard.type("X");
  await expect(page.locator("#master")).toHaveValue(`${m} line one\nline two\nline threeX`);
  const master = await page.evaluate(async () => (await import("/state.js" as any)).state.master);
  expect(master.replace(/\r/g, "")).toBe(`${m} line one\nline two\nline threeX`);
});

test("an image moved to a card that starts with a blank line posts with it", async ({ page, fakes }) => {
  const m = marker("mediablank");
  await openApp(page);
  for (const p of ["mastodon", "threads", "nostr"]) await page.locator(`#chips .target-row.p-${p}`).click();
  await page.locator("#master").fill(`${m} one`);
  await page.locator("#master").press("End");
  await page.keyboard.type("\n---\n\ntwo");
  await expect(shared(page)).toHaveCount(2);
  await expect(shared(page).nth(1)).toHaveValue("\ntwo");
  await page.locator("#imgfile").setInputFiles(path.join(__dirname, "assets", "gradient.png"));
  const sel = page.locator("#editor .ed-shared .ec-move").first();
  await expect(sel).toBeVisible();
  await sel.selectOption("1");
  await expect(page.locator('#editor .ed-shared .ecard[data-card="1"] .ec-mitem')).toHaveCount(1);
  await tab(page, "bluesky").click();
  await expect(page.locator("#editor .pp-thread:not(.stale)")).toBeVisible();
  const mediaOn = await posts(page).evaluateAll(ps => ps.map(p => p.querySelector(".pp-media") ? 1 : 0));
  expect(mediaOn).toEqual([0, 1]);
});

test("Alt+digit that types a character in the text is left alone", async ({ page }) => {
  await openApp(page);
  const prevented = await page.locator("#master").evaluate((ta: HTMLTextAreaElement) => {
    ta.focus();
    const e = new KeyboardEvent("keydown", { key: "[", code: "Digit5", altKey: true, bubbles: true, cancelable: true });
    ta.dispatchEvent(e);
    return e.defaultPrevented;
  });
  expect(prevented).toBe(false);
});

test("an upload finishing keeps the text selection", async ({ page }) => {
  await openApp(page);
  const first = page.locator("#master");
  await first.fill("select some of this text");
  await first.evaluate((ta: HTMLTextAreaElement) => ta.setSelectionRange(7, 11));
  await page.locator("#imgfile").setInputFiles(path.join(__dirname, "assets", "gradient.png"));
  await expect(page.locator("#master")).toBeFocused();
  const sel = await page.locator("#master").evaluate((ta: HTMLTextAreaElement) => [ta.selectionStart, ta.selectionEnd]);
  expect(sel).toEqual([7, 11]);
});

test("undo still works after an upload", async ({ page }) => {
  await openApp(page);
  const first = page.locator("#master");
  await first.click();
  await page.keyboard.type("abc");
  await page.locator("#imgfile").setInputFiles(path.join(__dirname, "assets", "gradient.png"));
  await first.click();
  await page.keyboard.press("End");
  await page.keyboard.press("Control+z");
  // The textarea kept its undo history (a rebuilt one has none).
  await expect(page.locator("#master")).not.toHaveValue("abc");
});

test("Enter on a focused button does not break at a picked word", async ({ page }) => {
  await openApp(page);
  await page.locator("#master").fill("Alpha beta. Gamma delta.");
  await tab(page, "mastodon").click();
  await page.locator('#editor .pp-w', { hasText: /^Gamma$/ }).click();
  await expect(page.locator("#editor .pp-here")).toBeVisible();
  // Keyboard user tabs to the Bluesky tab button and presses Enter.
  await tab(page, "bluesky").focus();
  await page.keyboard.press("Enter");
  await expect(tab(page, "bluesky")).toHaveClass(/\bon\b/);
  await tab(page, "mastodon").click();
  await expect(page.locator("#editor .pp-mode")).toHaveText("Shared breaks");
});

test("a translation loaded into the composer drops own breaks", async ({ page }) => {
  await openApp(page);
  await page.locator("#master").fill("Alpha beta. Gamma delta.");
  await tab(page, "mastodon").click();
  await page.locator('#editor .pp-w', { hasText: /^Gamma$/ }).click();
  await page.locator("#editor .pp-here").click();
  await expect(posts(page)).toHaveCount(2);
  await page.evaluate(async () => {
    const c = await import("/compose.js" as any);
    c.loadDraft({ text: "Ein ganz anderer Text der übersetzt wurde", lang: "de" });
  });
  const ok = page.locator(".modal.confirm").getByRole("button", { name: "Replace" });
  if (await ok.count()) await ok.click();
  await tab(page, "mastodon").click();
  await expect(page.locator("#editor .pp-mode")).toHaveText("Shared breaks");
});
