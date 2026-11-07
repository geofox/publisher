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
  await page.locator('.tab[data-view="settings"]').click();
  await page.locator(".set-nav").getByRole("button", { name: "Access" }).click();
  await expect(page.locator("#access")).toContainText("Publisher has no sign-in of its own here");
  await expect(page.locator('#tok-scopes input[value="admin"]')).toBeDisabled();
  await expect(page.locator('#tok-scopes input[value="accounts:write"]')).toBeDisabled();
  await expect(page.locator('#tok-scopes input[value="posts:write"]')).toBeEnabled();
});

test("Accounts behind the proxy alone: renaming and switching only, no credentials", async ({ page }) => {
  await page.goto("/");
  await page.locator('.tab[data-view="settings"]').click();
  await page.locator(".set-nav").getByRole("button", { name: "Accounts" }).click();
  const panel = page.locator("#set-accounts");
  await expect(panel).toContainText("credentials can't be managed in the app");
  await expect(panel.getByRole("button", { name: "Add an account" })).toHaveCount(0);
  await expect(panel.getByRole("button", { name: /^Rename / }).first()).toBeVisible();
  await expect(panel.getByRole("button", { name: /^Delete / })).toHaveCount(0);
  await expect(panel.getByRole("button", { name: /^Show key: / })).toHaveCount(0);
  // An account from the environment shows its settings, read-only, and can't be moved into the app here.
  await panel.getByRole("button", { name: /^Settings of / }).first().click();
  const dlg = page.locator(".acct-dlg");
  await expect(dlg).toBeVisible();
  await expect(dlg.getByRole("button", { name: "Move into the app" })).toHaveCount(0);
  await dlg.locator(".confirm-actions").getByRole("button", { name: "Close" }).click();
  await expect(dlg).toBeHidden();
});
