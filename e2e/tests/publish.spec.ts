import path from "node:path";
import { test, expect, openApp, switchTab, marker } from "./fixtures";

const PLATFORMS = ["nostr", "mastodon", "bluesky", "threads"] as const;

test("publishes one post to all four platforms and lists it in History @mobile", async ({ page, fakes }) => {
  const m = marker("hello");
  await openApp(page);
  await page.locator("#master").fill(`Hello from the e2e suite ${m}`);
  await page.locator("#submit").click();

  const modal = page.locator(".pmodal");
  await expect(modal.locator(".pm-title")).toHaveText("✓ Posted");
  await expect(modal.locator(".trow.lvl1.st-success")).toHaveCount(4);

  const calls = await fakes.waitForCalls(m, 4);
  expect(calls.map(c => c.platform).sort()).toEqual([...PLATFORMS].sort());
  for (const c of calls) {
    expect(c.action).toBe("post");
    expect(c.text).toBe(`Hello from the e2e suite ${m}`);
  }

  await modal.getByRole("button", { name: "Done" }).click();
  await expect(modal).toHaveCount(0);
  await expect(page.locator("#master")).toHaveValue(""); // composer cleared after a commit

  await switchTab(page, "history");
  const item = page.locator(".hitem", { hasText: m });
  await expect(item).toBeVisible();
  await item.click();
  const detail = page.locator("#hdetail");
  await expect(detail.locator(".lbl", { hasText: "result · published" })).toBeVisible();
  for (const p of PLATFORMS) {
    await expect(detail.locator(".res .bdg", { hasText: p })).toBeVisible();
  }
  // Links point at the (fake) remote posts the adapters returned.
  await expect(detail.locator('a.lnk[href^="https://mastodon.e2e.invalid/"]')).toBeVisible();
});

test("auto-threads a long post per platform limits", async ({ page, fakes }) => {
  const m = marker("thread");
  const sentence = "Reliability is a feature, and this sentence exists to take up room. ";
  const text = `${m} ${sentence.repeat(9)}`.trim(); // ~620 chars: > Bluesky 300 and > Mastodon/Threads 500
  await openApp(page);
  await page.locator("#master").fill(text);

  // The live preview (Bluesky by default) shows a multi-post thread.
  await expect(page.locator("#preview .pv-seg").first()).toBeVisible();
  expect(await page.locator("#preview .pv-seg").count()).toBeGreaterThan(1);

  await page.locator("#submit").click();
  // The composer warns that text is over some platforms' single-post limit,
  // even though the server will thread it; confirm to post.
  await page.getByRole("button", { name: "Post anyway" }).click();
  await expect(page.locator(".pmodal .pm-title")).toHaveText("✓ Posted");

  // Wait for all four platforms (Nostr's single post + three chains).
  await expect.poll(async () => new Set((await fakes.calls()).map(c => c.platform)).size).toBe(4);
  const calls = await fakes.calls();
  const by = (p: string) => calls.filter(c => c.platform === p);
  // Nostr has no length limit: one post.
  expect(by("nostr")).toHaveLength(1);
  for (const p of ["bluesky", "mastodon", "threads"]) {
    const chain = by(p);
    expect(chain.length, `${p} segments`).toBeGreaterThan(1);
    // Head is a post, every later segment replies to the previous one.
    expect(chain[0].action).toBe("post");
    for (let i = 1; i < chain.length; i++) {
      expect(chain[i].action).toBe("reply");
    }
    // Numbered 1/n … n/n (the "number thread posts" box is on by default).
    expect(chain[0].text).toContain(`1/${chain.length}`);
    expect(chain[chain.length - 1].text).toContain(`${chain.length}/${chain.length}`);
    // Nothing lost: every word of the original arrives, in order.
    const joined = chain.map(c => c.text.replace(/\s*\d+\/\d+\s*$/, "")).join(" ");
    expect(joined.replace(/\s+/g, " ")).toContain(m);
    expect(joined).toContain("take up room.");
  }
  // Mastodon chains reply to the platform ids the previous segment returned.
  const masto = by("mastodon");
  expect(masto[1].reply_to).toBe(masto[0].remote_id);
});

test("splits on a manual --- marker on every platform", async ({ page, fakes }) => {
  const m = marker("manual");
  await openApp(page);
  await page.locator("#master").fill(`${m} first part\n---\nsecond part`);
  await expect(page.locator("#preview .pv-seg")).toHaveCount(2);
  await page.locator("#submit").click();
  await expect(page.locator(".pmodal .pm-title")).toHaveText("✓ Posted");

  await expect.poll(async () => (await fakes.calls()).length).toBe(8);
  const calls = await fakes.calls();
  for (const p of PLATFORMS) {
    const chain = calls.filter(c => c.platform === p);
    expect(chain, p).toHaveLength(2);
    expect(chain[0].text).toContain(`${m} first part`);
    expect(chain[1].text).toContain("second part");
    expect(chain[1].action).toBe("reply");
  }
});

test("uploads an image with alt text to Blossom and attaches it everywhere", async ({ page, fakes }) => {
  const m = marker("image");
  await openApp(page);
  await page.locator("#master").fill(`Picture post ${m}`);
  await page.locator("#imgfile").setInputFiles(path.join(__dirname, "assets", "gradient.png"));
  const thumb = page.locator("#images .thumb");
  await expect(thumb).toHaveCount(1);
  await thumb.getByPlaceholder("alt text").fill("A blue gradient");
  await page.locator("#submit").click();
  await expect(page.locator(".pmodal .pm-title")).toHaveText("✓ Posted");

  const calls = await fakes.waitForCalls(m, 4);
  for (const c of calls) {
    expect(c.images, c.platform).toHaveLength(1);
    if (c.platform === "nostr") {
      // Nostr gets a NIP-92 imeta tag pointing at the Blossom upload.
      expect(c.images![0].mime).toMatch(/^image\//);
      continue;
    }
    expect(c.images![0].alt, c.platform).toBe("A blue gradient");
    expect(c.images![0].size, c.platform).toBeGreaterThan(0);
  }

  // History shows the archived Blossom media (served same-origin by the fake).
  await page.locator(".pmodal").getByRole("button", { name: "Done" }).click();
  await switchTab(page, "history");
  await page.locator(".hitem", { hasText: m }).click();
  const img = page.locator("#hdetail .dmedia img").first();
  await expect(img).toBeVisible();
  await expect.poll(() => img.evaluate((el: HTMLImageElement) => el.naturalWidth)).toBe(64);
});

test("a failed platform can be retried from History", async ({ page, fakes }) => {
  const m = marker("retry");
  await fakes.down("mastodon");
  await openApp(page);
  await page.locator("#master").fill(`Partial failure ${m}`);
  await page.locator("#submit").click();

  const modal = page.locator(".pmodal");
  await expect(modal.locator(".pm-title")).toHaveText("⚠ Posted with issues");
  await expect(modal.locator(".trow.lvl1.st-failed")).toHaveCount(1);
  await modal.getByText("Open in history ↗").click();

  const detail = page.locator("#hdetail");
  await expect(detail.locator(".lbl", { hasText: "result · partial" })).toBeVisible();
  const row = detail.locator(".res", { has: page.locator(".bdg", { hasText: "mastodon" }) });
  await expect(row.locator(".bad")).toContainText("e2e: mastodon is down");

  await fakes.up("mastodon");
  // The retry answers at once; the detail follows the post's events until
  // the engine has sent it.
  await row.getByRole("button", { name: "Retry" }).click();
  await expect(page.getByText("Mastodon: retrying")).toBeVisible();
  await expect(detail.locator(".lbl", { hasText: "result · published" })).toBeVisible();
  const masto = (await fakes.calls()).filter(c => c.platform === "mastodon");
  expect(masto).toHaveLength(1);
  expect(masto[0].text).toBe(`Partial failure ${m}`);
});

// Nostr gets the alt text in its NIP-92 imeta tag, like the other platforms.
test("Nostr images carry their alt text in the imeta tag", async ({ page, fakes }) => {
  const m = marker("nostr-alt");
  await openApp(page);
  await page.locator("#master").fill(`Alt on nostr ${m}`);
  await page.locator("#imgfile").setInputFiles(path.join(__dirname, "assets", "gradient.png"));
  await page.locator("#images .thumb").getByPlaceholder("alt text").fill("A blue gradient");
  await page.locator("#submit").click();
  await expect(page.locator(".pmodal .pm-title")).toHaveText("✓ Posted");
  const nostr = (await fakes.waitForCalls(m, 4)).find(c => c.platform === "nostr")!;
  expect(nostr.images?.[0]?.alt).toBe("A blue gradient");
});

test("places an image on the thread part its chip names", async ({ page, fakes }) => {
  const m = marker("place");
  await openApp(page);
  await page.locator("#master").fill(`${m} first part\n---\nsecond part`);
  await page.locator("#imgfile").setInputFiles(path.join(__dirname, "assets", "gradient.png"));
  const thumb = page.locator("#images .thumb");
  await thumb.getByPlaceholder("alt text").fill("On part two");
  await thumb.locator("select.place-chip").selectOption("1");
  // The server preview plans it on the second post.
  await expect(page.locator("#preview .pv-seg").nth(1).locator(".pv-media img")).toHaveCount(1);
  await expect(page.locator("#preview .pv-seg").nth(0).locator(".pv-media img")).toHaveCount(0);
  await page.locator("#submit").click();
  await expect(page.locator(".pmodal .pm-title")).toHaveText("✓ Posted");

  await expect.poll(async () => (await fakes.calls()).length).toBe(8);
  const calls = await fakes.calls();
  for (const p of PLATFORMS) {
    const chain = calls.filter(c => c.platform === p);
    expect(chain, p).toHaveLength(2);
    expect(chain[0].images ?? [], p).toHaveLength(0);
    expect(chain[1].images, p).toHaveLength(1);
    expect(chain[1].images![0].alt, p).toBe("On part two");
  }
});
