import path from "node:path";
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

test("posts a saved draft with its image, and the draft is used up", async ({ page, fakes }) => {
  const m = marker("draftimg");
  await openApp(page);
  await page.locator("#master").fill(`Draft with a picture ${m}`);
  await page.locator("#imgfile").setInputFiles(path.join(__dirname, "assets", "gradient.png"));
  await page.locator("#images .thumb").getByPlaceholder("alt text").fill("A saved gradient");
  await page.locator("#draft-save").click();
  await expect(page.locator("#draft-list .draft-row", { hasText: m })).toBeVisible();

  // Reopened from the server, the image is a stored attachment, not a file.
  await page.reload();
  await expect(page.locator("#chips .target-row")).toHaveCount(4);
  await page.locator("#draft-list .draft-row", { hasText: m }).click();
  await expect(page.locator("#images .thumb")).toHaveCount(1);
  await expect(page.locator("#images .thumb").getByPlaceholder("alt text")).toHaveValue("A saved gradient");
  await page.locator("#submit").click();
  await expect(page.locator(".pmodal .pm-title")).toHaveText("✓ Posted");

  const calls = await fakes.waitForCalls(m, 4);
  for (const c of calls) {
    expect(c.images, c.platform).toHaveLength(1);
    expect(c.images![0].alt, c.platform).toBe("A saved gradient");
  }
  await page.locator(".pmodal").getByRole("button", { name: "Done" }).click();
  await expect(page.locator("#draft-list .draft-row", { hasText: m })).toHaveCount(0);
});

test("a draft with the same image twice posts both", async ({ page, fakes }) => {
  const m = marker("draftdup");
  const file = path.join(__dirname, "assets", "gradient.png");
  await openApp(page);
  await page.locator("#master").fill(`Twice the gradient ${m}`);
  await page.locator("#imgfile").setInputFiles(file);
  await page.locator("#imgfile").setInputFiles(file);
  await expect(page.locator("#images .thumb")).toHaveCount(2);
  await page.locator("#draft-save").click();
  await expect(page.locator("#draft-list .draft-row", { hasText: m })).toBeVisible();
  await page.locator("#submit").click();
  await expect(page.locator(".pmodal .pm-title")).toHaveText("✓ Posted");
  const calls = await fakes.waitForCalls(m, 4);
  for (const c of calls) expect(c.images, c.platform).toHaveLength(2);
});

test("a draft keeps its targets, numbering and tags", async ({ page }) => {
  const m = marker("draftset");
  await openApp(page);
  await page.locator("#master").fill(`Settings ${m}\n---\nsecond`);
  await page.locator("#chips .target-row.p-threads").click();
  await page.locator("#threadnum").uncheck();
  const tag = page.locator("#draft-tags-input input");
  await tag.fill("Work");
  await tag.press("Enter");
  await page.locator("#draft-save").click();
  await expect(page.locator("#draft-list .draft-row", { hasText: m })).toBeVisible();

  // Stored as a POST /v1/posts body.
  const list = await (await page.request.get(`/v1/drafts?q=${m}&tag=work`)).json();
  expect(list.data).toHaveLength(1);
  const d = await (await page.request.get(`/v1/drafts/${list.data[0].id}`)).json();
  expect(d.post.content.text).toBe(`Settings ${m}\n---\nsecond`);
  expect(d.post.targets).toHaveLength(3);
  expect(d.post.threading).toEqual({ numbering: false });

  await page.reload();
  await expect(page.locator("#chips .target-row")).toHaveCount(4);
  await expect(page.locator("#threadnum")).toBeChecked();
  await page.locator("#draft-list .draft-row", { hasText: m }).click();
  await expect(page.locator("#master")).toHaveValue(`Settings ${m}\n---\nsecond`);
  await expect(page.locator("#threadnum")).not.toBeChecked();
  await expect(page.locator("#chips .target-row.p-threads")).toHaveClass(/\boff\b/);
  await expect(page.locator("#chips .target-row.p-bluesky")).not.toHaveClass(/\boff\b/);
  await expect(page.locator("#draft-tags-input .tag-chip")).toHaveText(["#work ✕"]);
});

test("saving over a draft changed in another tab asks first", async ({ page, context }) => {
  const m = marker("draftconf");
  await openApp(page);
  await page.locator("#master").fill(`Version one ${m}`);
  await page.locator("#draft-save").click();
  await expect(page.locator("#draft-list .draft-row", { hasText: m })).toBeVisible();

  // Another tab opens the same draft, then this tab saves a change.
  const other = await context.newPage();
  await openApp(other);
  await other.locator("#draft-list .draft-row", { hasText: m }).click();
  await expect(other.locator("#master")).toHaveValue(`Version one ${m}`);
  await page.locator("#master").fill(`Version two ${m}`);
  await page.locator("#draft-save").click();
  await expect(page.locator("#draft-status")).toHaveText("saved just now");

  // The other tab's save is stale: it asks, and overwrites on request.
  await other.locator("#master").fill(`Version three ${m}`);
  await other.locator("#draft-save").click();
  const modal = other.locator(".modal.confirm");
  await expect(modal).toContainText("This draft changed in another tab");
  await modal.getByRole("button", { name: "Overwrite" }).click();
  await expect(other.locator("#draft-status")).toHaveText("saved just now");
  const list = await (await page.request.get(`/v1/drafts?q=${m}`)).json();
  expect(list.data).toHaveLength(1);
  expect(list.data[0].preview).toBe(`Version three ${m}`);
  await other.close();
});

test("a scheduled draft reopens scheduled, and New clears it", async ({ page }) => {
  const m = marker("draftsched");
  await openApp(page);
  await page.locator("#master").fill(`Later ${m}`);
  const later = await page.evaluate(() => {
    const d = new Date(Date.now() + 2 * 24 * 3600 * 1000);
    const pad = (n: number) => String(n).padStart(2, "0");
    return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
  });
  await page.locator("#schedat").fill(later);
  await page.locator("#draft-save").click();
  await expect(page.locator("#draft-list .draft-row", { hasText: m })).toBeVisible();

  await page.reload();
  await expect(page.locator("#chips .target-row")).toHaveCount(4);
  await page.locator("#draft-list .draft-row", { hasText: m }).click();
  await expect(page.locator("#schedat")).toHaveValue(later);
  await expect(page.locator("#submit")).toHaveText("Schedule");
  await expect(page.locator("#schedclear")).toBeVisible();

  await page.locator("#draft-new").click();
  await expect(page.locator("#schedat")).toHaveValue("");
  await expect(page.locator("#submit")).toHaveText("Post");
  await expect(page.locator("#master")).toHaveValue("");
});
