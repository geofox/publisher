import { test, expect, openApp, switchTab, marker } from "./fixtures";

test.use({ timezoneId: "America/New_York" });

test("History 'change time' names and reads the device's time zone", async ({ page, request }) => {
  const accounts = (await (await request.get("/v1/accounts")).json()).data as { id: string; type: string; status: string }[];
  const masto = accounts.find(a => a.status === "connected" && a.type === "mastodon")!;
  const m = marker("tz");
  const r = await request.post("/v1/posts", { headers: { "Content-Type": "application/json" },
    data: { content: { text: `${m}` }, targets: [{ account: masto.id }], schedule: { at: "2027-01-10T09:00:00Z" } } });
  expect(r.ok()).toBeTruthy();
  await openApp(page);
  await switchTab(page, "history");
  await page.locator('#hseg button[data-status="scheduled"]').click();
  await page.locator(".hitem", { hasText: m }).click();
  let patched: any = null;
  page.on("request", q => { if (q.method() === "PATCH") patched = q.postDataJSON(); });
  let prompted = "", prefill = "";
  page.once("dialog", d => { prompted = d.message(); prefill = d.defaultValue(); d.accept("2027-01-12T10:00"); });
  await page.locator("#hdetail").getByRole("button", { name: "change time" }).click();
  await expect.poll(() => patched).not.toBeNull();
  expect(prompted).toContain("America/New_York");
  expect(prefill).toBe("2027-01-10T04:00"); // the current time, in New York
  // 10:00 New York in January is 15:00 UTC.
  expect(patched.schedule.at).toBe("2027-01-12T15:00:00.000Z");
});

test("composer schedule names the device's time zone and shows the time as typed", async ({ page }) => {
  await openApp(page);
  await page.locator("#master").fill(marker("tz2"));
  await page.locator("#schedat").fill("2027-01-12T10:00");
  const note = await page.locator("#schednote").textContent();
  console.log("label:", await page.locator("#schedrow").evaluate(e => e.previousElementSibling!.textContent), "note:", note);
  expect(note).toContain("10:00");
  await expect(page.locator("#schedtz")).toContainText("America/New_York");
});
