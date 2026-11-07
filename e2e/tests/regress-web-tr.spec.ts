import { test, expect, openApp, switchTab, marker } from "./fixtures";
test("translating a History post into Compose doesn't inherit the old schedule / visibility / CW", async ({ page, request }) => {
  const accounts = (await (await request.get("/v1/accounts")).json()).data as { id: string; type: string; status: string }[];
  const masto = accounts.find(a => a.status === "connected" && a.type === "mastodon")!;
  const m = marker("tr");
  const r = await request.post("/v1/posts", { headers: { "Content-Type": "application/json", Prefer: "wait=10" },
    data: { content: { text: `Old post ${m}` }, targets: [{ account: masto.id }] } });
  expect(r.ok()).toBeTruthy();
  await page.route("**/v1/instance", async rt => {
    const res = await rt.fetch(); const j = await res.json();
    await rt.fulfill({ response: res, json: { ...j, translate_targets: ["fr"] } });
  });
  await page.route("**/v1/translate", rt => rt.fulfill({ json: { text: `Vieux post ${m}` } }));
  await openApp(page);
  // Work in progress: a scheduled, direct-visibility Mastodon post with a CW.
  await page.locator("#master").fill("private note");
  await page.locator("#cards .ocard.p-mastodon .ocard-summary").click();
  await page.locator("#cards .ocard.p-mastodon input[type=text]").first().fill("CW: family");
  await page.locator("#cards .ocard.p-mastodon select").first().selectOption("direct");
  await page.locator("#schedat").fill("2027-03-01T10:00");
  await switchTab(page, "history");
  await page.locator(".hitem", { hasText: m }).click();
  await page.locator("#hdetail .translate-sel").selectOption("fr");
  await page.locator(".modal .danger").click(); // Replace
  await expect(page.locator("#master")).toHaveValue(`Vieux post ${m}`);
  let body: any = null;
  await page.route("**/v1/posts", async rt => { if (rt.request().method() === "POST") { body = rt.request().postDataJSON(); return rt.fulfill({ status: 400, json: { detail: "intercepted" } }); } return rt.continue(); });
  page.removeAllListeners("console");
  await page.locator("#submit").click();
  await expect.poll(() => body).not.toBeNull();
  const mt = body.targets.find((t: any) => t.account === masto.id);
  console.log(JSON.stringify({ schedule: body.schedule, masto: mt }));
  expect(body.schedule, "old schedule carried over").toBeUndefined();
  // The composer sends what its card holds, empty values too.
  expect(mt.options?.visibility || "", "old visibility carried over").toBe("");
  expect(mt.content?.content_warning || "", "old CW carried over").toBe("");
});
