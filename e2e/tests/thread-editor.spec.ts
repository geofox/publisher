import { test, expect, ACCOUNT_ROWS, openApp, harnessIds, marker, type Call } from "./fixtures";
import type { Page } from "@playwright/test";

// The thread editor (internal/web/assets/editor.js): the shared text as a
// stack of cards, and a tab per platform that shows and shapes its thread.

const shared = (page: Page) => page.locator("#editor .ed-shared textarea.ec-text");
// Tabs and rows by network ("bluesky": the account a new post selects
// there) or by label ("Mastodon (long posts)").
let ids: Record<string, string> = {};
test.beforeEach(async ({ request }) => { if (!Object.keys(ids).length) ids = await harnessIds(request); });
const tab = (page: Page, name: string) => page.locator(`#editor .ed-tab[data-tab="${name === "shared" ? "shared" : ids[name]}"]`);
const row = (page: Page, name: string) => page.locator(`#chips .target-row[data-account="${ids[name]}"]`);
const posts = (page: Page) => page.locator("#editor .ed-platform .pp-post");

function chainOf(calls: Call[], platform: Call["platform"], m: string) {
  const all = calls.filter(c => c.platform === platform);
  const i = all.findIndex(c => c.text.includes(m));
  return i < 0 ? [] : all.slice(i);
}

test("splits and merges cards in the shared text", async ({ page }) => {
  const m = marker("cards");
  await openApp(page);
  const first = page.locator("#master");
  await first.fill(`${m} one. two three`);
  // Ctrl+Enter splits at the cursor: before "two".
  await first.evaluate((ta: HTMLTextAreaElement, at) => ta.setSelectionRange(at, at), `${m} one. `.length);
  await first.press("Control+Enter");
  await expect(shared(page)).toHaveCount(2);
  await expect(shared(page).nth(0)).toHaveValue(`${m} one.`);
  await expect(shared(page).nth(1)).toHaveValue("two three");
  await expect(shared(page).nth(1)).toBeFocused();
  await expect(page.locator("#preview .pv-seg")).toHaveCount(2);

  // Backspace at the start of a card joins it to the one above.
  await page.keyboard.press("Backspace");
  await expect(shared(page)).toHaveCount(1);
  await expect(first).toHaveValue(`${m} one. two three`);

  // A typed line of "---" is a break once the line ends.
  await first.press("End");
  await page.keyboard.type("\n---");
  await expect(shared(page)).toHaveCount(1); // "---" alone on the last line is still text
  await page.keyboard.press("Enter");
  await expect(shared(page)).toHaveCount(2);
  await expect(shared(page).nth(0)).toHaveValue(`${m} one. two three`);
  await expect(shared(page).nth(1)).toBeFocused();
  await page.keyboard.type("four");
  await expect(shared(page).nth(1)).toHaveValue("four");

  // The divider's × removes the break too.
  await page.locator("#editor .ed-shared .ec-break-x").click();
  await expect(shared(page)).toHaveCount(1);
  await expect(first).toHaveValue(`${m} one. two three\nfour`);
});

test("a break made on Bluesky's tab splits only Bluesky", async ({ page, fakes }) => {
  const m = marker("own");
  await openApp(page);
  await page.locator("#master").fill(`${m} first sentence here. Second sentence there.`);
  await tab(page, "bluesky").click();
  await expect(page.locator("#editor .pp-mode")).toHaveText("Shared breaks");
  await expect(posts(page)).toHaveCount(1);

  await page.locator('#editor .pp-w', { hasText: /^Second$/ }).click();
  await page.locator("#editor .pp-here").click();
  await expect(posts(page)).toHaveCount(2);
  await expect(page.locator("#editor .pp-mode")).toHaveText("Own breaks");
  await expect(page.locator("#editor .pp-break.manual")).toHaveCount(1);
  await expect(tab(page, "bluesky").locator(".ed-tab-n")).toHaveText("2");

  // ↑ moves it a sentence back: there is none, so a word back (Shift).
  await page.locator("#editor .pp-break .pp-bb[aria-label='move break earlier']").click({ modifiers: ["Shift"] });
  await expect(posts(page).nth(1).locator(".pp-text")).toContainText("here. Second sentence there.");
  await page.locator("#editor .pp-break .pp-bb[aria-label='move break later']").click();
  await expect(posts(page).nth(1).locator(".pp-text")).toHaveText(/^Second sentence there\./);

  // The shared text still feeds Bluesky: an edit there shows on its posts.
  await tab(page, "shared").click();
  await page.locator("#master").press("End");
  await page.keyboard.type(" Third.");
  await tab(page, "bluesky").click();
  await expect(posts(page).nth(1).locator(".pp-text")).toContainText("Second sentence there. Third.");

  await page.locator("#submit").click();
  await expect(page.locator(".pmodal .pm-title")).toHaveText("✓ Posted");
  await expect.poll(async () => (await fakes.calls()).length).toBe(5);
  const calls = await fakes.calls();
  const bsky = chainOf(calls, "bluesky", m);
  expect(bsky.map(c => c.text)).toEqual([`${m} first sentence here. 1/2`, "Second sentence there. Third. 2/2"]);
  for (const p of ["nostr", "mastodon", "threads"] as const) {
    expect(chainOf(calls, p, m).map(c => c.text), p).toEqual([`${m} first sentence here. Second sentence there. Third.`]);
  }
});

test("Reset to shared drops a platform's own breaks", async ({ page }) => {
  await openApp(page);
  await page.locator("#master").fill("Alpha beta. Gamma delta.");
  await tab(page, "mastodon").click();
  await page.locator('#editor .pp-w', { hasText: /^Gamma$/ }).click();
  await page.locator("#editor .pp-here").click();
  await expect(posts(page)).toHaveCount(2);
  await page.getByRole("button", { name: "Reset to shared" }).click();
  await expect(posts(page)).toHaveCount(1);
  await expect(page.locator("#editor .pp-mode")).toHaveText("Shared breaks");
});

test("own wording on Mastodon, then back to the shared text", async ({ page, fakes }) => {
  const m = marker("wording");
  await openApp(page);
  await page.locator("#master").fill(`${m} shared words`);
  await tab(page, "mastodon").click();
  await page.getByRole("button", { name: "Edit wording for Mastodon" }).click();
  await expect(page.locator("#editor .pp-mode")).toHaveText("Own text");
  const own = page.locator("#editor .ed-platform textarea.ec-text");
  await expect(own).toHaveValue(`${m} shared words`);
  await own.fill(`${m} only for Mastodon\n---\nand a second post`);
  await expect(page.locator("#editor .ed-platform textarea.ec-text")).toHaveCount(2);
  await expect(tab(page, "mastodon").locator(".ed-tab-mode")).toHaveText("✎");

  // The shared text goes on without it.
  await tab(page, "shared").click();
  await page.locator("#master").press("End");
  await page.keyboard.type(" more");
  await page.locator("#submit").click();
  await expect(page.locator(".pmodal .pm-title")).toHaveText("✓ Posted");
  await expect.poll(async () => (await fakes.calls()).length).toBe(5);
  const calls = await fakes.calls();
  expect(chainOf(calls, "mastodon", m).map(c => c.text)).toEqual([`${m} only for Mastodon 1/2`, "and a second post 2/2"]);
  expect(chainOf(calls, "bluesky", m).map(c => c.text)).toEqual([`${m} shared words more`]);
});

test("Back to shared text asks first, then mirrors the shared text", async ({ page }) => {
  await openApp(page);
  await page.locator("#master").fill("Shared wording");
  await tab(page, "threads").click();
  await page.getByRole("button", { name: "Edit wording for Threads" }).click();
  await page.locator("#editor .ed-platform textarea.ec-text").fill("Threads wording");
  await page.getByRole("button", { name: "Back to shared text" }).click();
  await page.locator(".modal.confirm").getByRole("button", { name: "Use shared text" }).click();
  await expect(page.locator("#editor .pp-mode")).toHaveText("Shared breaks");
  await expect(posts(page).first().locator(".pp-text")).toHaveText("Shared wording");
});

test("an edit that makes an own-break post too long is split to fit, dashed", async ({ page }) => {
  await openApp(page);
  await page.locator("#master").fill("Short one. Short two.");
  await tab(page, "bluesky").click();
  await page.locator('#editor .pp-w', { hasText: /^Short$/ }).nth(1).click();
  await page.locator("#editor .pp-here").click();
  await expect(posts(page)).toHaveCount(2);

  await tab(page, "shared").click();
  await page.locator("#master").press("End");
  await page.keyboard.insertText(" " + "Filler words to go over the limit. ".repeat(10));
  await tab(page, "bluesky").click();
  await expect(page.locator("#editor .pp-break.manual")).toHaveCount(1);
  await expect(page.locator("#editor .pp-break.auto").first()).toBeVisible();
  for (const c of await page.locator("#editor .pp-count").allTextContents()) {
    const [n, limit] = c.split("/").map(Number);
    expect(n).toBeLessThanOrEqual(limit);
  }
});

test("a draft keeps a platform's own breaks and wording", async ({ page }) => {
  const m = marker("draftlayout");
  await openApp(page);
  await page.locator("#master").fill(`${m} one. Two three.`);
  await tab(page, "bluesky").click();
  await page.locator('#editor .pp-w', { hasText: /^Two$/ }).click();
  await page.locator("#editor .pp-here").click();
  await expect(posts(page)).toHaveCount(2);
  await tab(page, "threads").click();
  await page.getByRole("button", { name: "Edit wording for Threads" }).click();
  await page.locator("#editor .ed-platform textarea.ec-text").fill(`${m} Threads only`);
  await page.locator("#draft-save").click();
  await expect(page.locator("#draft-list .draft-row", { hasText: m })).toBeVisible();

  await page.reload();
  await expect(page.locator("#chips .target-row")).toHaveCount(ACCOUNT_ROWS);
  await page.locator("#draft-list .draft-row", { hasText: m }).click();
  await expect(page.locator("#master")).toHaveValue(`${m} one. Two three.`);
  await tab(page, "bluesky").click();
  await expect(page.locator("#editor .pp-mode")).toHaveText("Own breaks");
  await expect(posts(page)).toHaveCount(2);
  await expect(posts(page).nth(1).locator(".pp-text")).toHaveText(/^Two three\./);
  await tab(page, "threads").click();
  await expect(page.locator("#editor .ed-platform textarea.ec-text")).toHaveValue(`${m} Threads only`);
});

test("a thread posts as its tab shows it", async ({ page, fakes }) => {
  const m = marker("asshown");
  const text = `${m} ` + "Every sentence here is the same length, more or less. ".repeat(12);
  await openApp(page);
  await page.locator("#master").fill(text.trim());
  await tab(page, "bluesky").click();
  await expect(page.locator("#editor .pp-thread:not(.stale)")).toBeVisible();
  const shown = await posts(page).count();
  expect(shown).toBeGreaterThan(1);
  const firstShown = (await posts(page).first().locator(".pp-text").textContent())!.trim();

  await page.locator("#submit").click();
  await expect(page.locator(".pmodal .pm-title")).toHaveText("✓ Posted");
  const chain = await expect.poll(async () => chainOf(await fakes.calls(), "bluesky", m).length).toBe(shown).then(async () => chainOf(await fakes.calls(), "bluesky", m));
  expect(chain[0].text).toBe(firstShown);
});

test("a break dragged onto a word moves there", async ({ page }) => {
  await openApp(page);
  await page.locator("#master").fill("One two three. Four five six.");
  await tab(page, "nostr").click();
  await page.locator('#editor .pp-w', { hasText: /^Four$/ }).click();
  await page.locator("#editor .pp-here").click();
  await expect(posts(page)).toHaveCount(2);
  await page.locator("#editor .pp-break .pp-break-lbl").dragTo(page.locator('#editor .pp-w', { hasText: /^three\.$/ }));
  await expect(posts(page).nth(0).locator(".pp-text")).toHaveText(/^One two 1\/2$/);
  await expect(posts(page).nth(1).locator(".pp-text")).toHaveText(/^three\. Four five six\./);
});

test("unsaved work from the old composer comes back with its breaks", async ({ page }) => {
  await openApp(page);
  // A recovery snapshot as the composer kept it before positions: a "---"
  // line in the text and the image's part number.
  await page.evaluate(() => localStorage.setItem("compose_recovery_v1", JSON.stringify({
    master_text: "Before the line\n---\nafter the line", platforms: ["bluesky", "nostr"],
    overrides: { bluesky: { text: "Own one\n---\nOwn two", langs: ["en"] }, nostr: { pow: 0 } },
    images: [], anchors: {}, number: true,
  })));
  await page.reload();
  await expect(page.locator("#chips .target-row")).toHaveCount(ACCOUNT_ROWS);
  await page.locator("#draft-recovery-restore").click();
  await expect(shared(page)).toHaveCount(2);
  await expect(shared(page).nth(0)).toHaveValue("Before the line");
  await expect(shared(page).nth(1)).toHaveValue("after the line");
  await tab(page, "bluesky").click();
  const own = page.locator("#editor .ed-platform textarea.ec-text");
  await expect(own).toHaveCount(2);
  await expect(own.nth(1)).toHaveValue("Own two");
});

test("Alt+number switches tabs, and Compare lines the threads up", async ({ page }) => {
  await openApp(page);
  await page.locator("#master").fill("Alpha one. " + "Words that take room on a short platform. ".repeat(8));
  await page.keyboard.press("Alt+3"); // Shared, Bluesky, Mastodon…
  await expect(tab(page, "mastodon")).toHaveClass(/\bon\b/);
  await page.keyboard.press("Alt+1");
  await expect(tab(page, "shared")).toHaveClass(/\bon\b/);

  await page.locator("#editor .ed-compare").click();
  const cmp = page.locator(".modal.cmp");
  await expect(cmp.locator(".cmp-head")).toHaveCount(4);
  await expect(cmp.locator(`.cmp-head[data-account="${ids.bluesky}"]`)).toContainText("2 posts");
  await expect(cmp.locator(`.cmp-cell[data-account="${ids.bluesky}"].brk-auto`)).toHaveCount(1);
  await cmp.locator(`.cmp-head[data-account="${ids.nostr}"]`).click();
  await expect(cmp).toHaveCount(0);
  await expect(tab(page, "nostr")).toHaveClass(/\bon\b/);
});

test("History marks where each part of a thread came from", async ({ page }) => {
  const m = marker("ranges");
  await openApp(page);
  await row(page, "mastodon").click();
  await row(page, "threads").click();
  await row(page, "nostr").click();
  await page.locator("#master").fill(`${m} first.\n---\nsecond.`);
  await page.locator("#submit").click();
  await expect(page.locator(".pmodal .pm-title")).toHaveText("✓ Posted");
  await page.locator(".pmodal").getByRole("button", { name: "Done" }).click();
  await page.locator('.tab[data-view="history"]').click();
  await page.locator(".hitem", { hasText: m }).click();
  const detail = page.locator("#hdetail");
  await detail.locator(".seg-row.ranged").nth(1).hover();
  await expect(detail.locator(".dtext.main mark")).toHaveText("second.");
  await detail.locator(".seg-row.ranged").nth(0).hover();
  await expect(detail.locator(".dtext.main mark")).toHaveText(`${m} first.`);
});
