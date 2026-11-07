import { test, expect, openApp, openSettings } from "./fixtures";

test("creates a scoped token that works on /v1, then revokes it", async ({ page, request }) => {
  await openApp(page);
  await openSettings(page, "access");
  const name = "e2e-" + Date.now();
  await page.locator("#tok-name-input").fill(name);
  // Publishing scopes are checked by default; add history reading.
  await expect(page.locator('#tok-scopes input[value="posts:write"]')).toBeChecked();
  await page.locator('#tok-scopes input[value="posts:read"]').check();
  await page.locator("#tok-expiry").selectOption("30");
  await page.locator("#tok-create-btn").click();

  const secret = await page.locator(".tok-secret-input").inputValue();
  expect(secret.length).toBeGreaterThan(20);
  await page.getByRole("button", { name: "Done" }).click();

  const row = page.locator(".tok-row", { hasText: name });
  await expect(row.locator(".tok-scopes-list")).toHaveText("posts:read posts:write media:write");
  await expect(row).toContainText("expires");

  // The secret is a working bearer token with just those scopes.
  const me = await request.get("/v1/me", { headers: { Authorization: "Bearer " + secret } });
  expect(me.status()).toBe(200);
  expect((await me.json()).scopes).toEqual(["posts:read", "posts:write", "media:write"]);
  const denied = await request.get("/v1/tokens", { headers: { Authorization: "Bearer " + secret } });
  expect(denied.status()).toBe(403);

  await row.getByRole("button", { name: "Revoke" }).click();
  await page.locator(".modal.confirm").getByRole("button", { name: "Revoke" }).click();
  await expect(row.locator(".tok-badge")).toHaveText("Revoked");
  const after = await request.get("/v1/me", { headers: { Authorization: "Bearer " + secret } });
  expect(after.status()).toBe(401);
});
