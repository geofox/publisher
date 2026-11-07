import { test, expect } from "./fixtures";

// Behind the reverse proxy alone (the main harness): the app suggests
// setting up sign-in until "Not now", Access says why there is nothing to
// manage, and the token form can't grant what needs a signed-in owner.

test("the sign-in banner, until Not now", async ({ page }) => {
  await page.goto("/");
  const banner = page.locator("#signin-banner");
  await expect(banner).toBeVisible();
  await expect(banner).toContainText("Publisher has no sign-in of its own");
  await banner.getByRole("button", { name: "Not now" }).click();
  await expect(banner).toBeHidden();
  await page.reload();
  await expect(page.locator("#compose")).toBeVisible();
  await expect(banner).toBeHidden();
});

test("Access explains proxy mode; admin and accounts:write can't be granted", async ({ page }) => {
  await page.goto("/");
  await page.locator('.tab[data-view="tokens"]').click();
  await expect(page.locator("#access")).toContainText("Publisher has no sign-in of its own here");
  await expect(page.locator('#tok-scopes input[value="admin"]')).toBeDisabled();
  await expect(page.locator('#tok-scopes input[value="accounts:write"]')).toBeDisabled();
  await expect(page.locator('#tok-scopes input[value="posts:write"]')).toBeEnabled();
});
