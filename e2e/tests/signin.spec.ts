import { test, expect, type Page, type BrowserContext, type CDPSession } from "@playwright/test";

// Local sign-in (AUTH_MODE=local) against the second harness, at
// http://localhost:<port> (a secure context), with a virtual passkey
// authenticator. The steps share one browser and one server, in order:
// setup → passkey → signed in; a password; re-authentication for an admin
// token; sign out and in; wrong passwords throttle the password but not
// the passkey; setup is closed.

const PORT = Number(process.env.E2E_LOCAL_PORT || Number(process.env.E2E_PORT || 4173) + 1);
const BASE = `http://localhost:${PORT}`;
const PASSWORD = "a long enough passphrase";

test.describe.configure({ mode: "serial" });

let context: BrowserContext;
let page: Page;
let cdp: CDPSession;
let authenticatorId = "";
const errors: string[] = [];

test.beforeAll(async ({ browser }) => {
  context = await browser.newContext({ baseURL: BASE });
  page = await context.newPage();
  page.on("pageerror", e => errors.push(String(e)));
  cdp = await context.newCDPSession(page);
  await cdp.send("WebAuthn.enable");
  ({ authenticatorId } = await cdp.send("WebAuthn.addVirtualAuthenticator", {
    options: {
      protocol: "ctap2", transport: "internal", hasResidentKey: true,
      hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true,
    },
  }));
});

test.afterAll(async () => {
  expect(errors).toEqual([]);
  await context.close();
});

async function credentials() {
  return (await cdp.send("WebAuthn.getCredentials", { authenticatorId })).credentials;
}

test("before setup, the app shows how to set up sign-in", async () => {
  await page.goto("/");
  await expect(page.getByRole("heading", { name: "Sign in" })).toBeVisible();
  await expect(page.getByText("Sign-in isn't set up yet.")).toBeVisible();
  // The API wants a session.
  expect((await page.request.get("/v1/accounts")).status()).toBe(401);
});

test("setup with a passkey signs the owner in", async () => {
  const link = await (await page.request.post("/__e2e/setup-url")).text();
  expect(link).toMatch(new RegExp(`^${BASE}/setup#`));
  await page.goto(link);
  // The token leaves the address bar at once.
  await expect(page).toHaveURL(`${BASE}/setup`);
  await page.getByLabel("Name this passkey").fill("Test laptop");
  await page.getByRole("button", { name: "Create a passkey" }).click();
  await expect(page).toHaveURL(`${BASE}/`);
  await expect(page.locator("#compose")).toBeVisible();
  expect(await credentials()).toHaveLength(1);
  // The same link can't be used again, and setup is closed.
  await page.goto(link);
  await expect(page.getByRole("heading", { name: "Setup is done" })).toBeVisible();
  const gone = await page.request.get("/setup");
  expect(gone.status()).toBe(410);
  expect(await gone.text()).toContain("Setup is done");
});

test("Access lists the passkey and the session; setting a password asks to confirm", async () => {
  await page.goto("/");
  await page.locator('.tab[data-view="tokens"]').click();
  const access = page.locator("#access");
  await expect(access.getByText("Test laptop")).toBeVisible();
  await expect(access.getByText("this browser")).toBeVisible();
  await expect(access.getByText("Signed in with setup")).toBeVisible();

  await access.getByRole("button", { name: "Set a password" }).click();
  await page.getByLabel("New password (at least 12 characters)").fill(PASSWORD);
  await page.getByLabel("The new password again").fill(PASSWORD);
  await page.getByRole("button", { name: "Set", exact: true }).click();
  // Re-authentication with the passkey.
  const dlg = page.getByRole("dialog", { name: "Confirm it's you" });
  await expect(dlg).toContainText("set your password");
  await dlg.getByRole("button", { name: "Use your passkey" }).click();
  await expect(dlg).toBeHidden();
  await expect(access.getByText("Set a password", { exact: true })).toBeVisible(); // in the activity list
  await expect(access.getByRole("button", { name: "Change" })).toBeVisible();
});

test("an admin token needs re-authentication; the password works for it", async () => {
  await page.locator("#tok-name-input").fill("deploy bot");
  await page.locator('#tok-scopes input[value="admin"]').check();
  await page.locator("#tok-create-btn").click();
  const dlg = page.getByRole("dialog", { name: "Confirm it's you" });
  await expect(dlg).toContainText("create a token that can make tokens or change accounts");
  await dlg.getByLabel("Your password").fill("not the password");
  await dlg.getByRole("button", { name: "Confirm with your password" }).click();
  await expect(dlg).toContainText("That isn't your password.");
  await dlg.getByLabel("Your password").fill(PASSWORD);
  await dlg.getByRole("button", { name: "Confirm with your password" }).click();
  await expect(page.locator(".tok-secret-input")).toHaveValue(/.{20,}/);
  await page.getByRole("button", { name: "Done" }).click();
  await expect(page.locator("#tokens-list")).toContainText("deploy bot");
  // Cancelling the dialog cancels the create.
  await page.locator("#tok-name-input").fill("second bot");
  await page.locator('#tok-scopes input[value="admin"]').check();
  await page.locator("#tok-create-btn").click();
  await page.getByRole("dialog", { name: "Confirm it's you" }).getByRole("button", { name: "Cancel" }).click();
  await expect(page.locator(".toast", { hasText: "Not created: you didn't confirm it's you." })).toBeVisible();
  await expect(page.locator("#tokens-list")).not.toContainText("second bot");
});

test("sign out, then in with the password", async () => {
  await page.locator("#access").getByRole("button", { name: "Sign out", exact: true }).first().click();
  await expect(page.getByRole("heading", { name: "Sign in" })).toBeVisible();
  expect((await page.request.get("/v1/accounts")).status()).toBe(401);
  await page.getByLabel("Password").fill(PASSWORD);
  await page.getByRole("button", { name: "Sign in with your password" }).click();
  await expect(page.locator("#compose")).toBeVisible();
});

test("wrong passwords are throttled, and a passkey still signs in", async () => {
  await page.locator('.tab[data-view="tokens"]').click();
  await page.locator("#access").getByRole("button", { name: "Sign out", exact: true }).first().click();
  await expect(page.getByRole("heading", { name: "Sign in" })).toBeVisible();
  const status = page.locator("#auth-status");
  for (let i = 0; i < 5; i++) {
    await page.getByLabel("Password").fill("wrong password " + i);
    await page.getByRole("button", { name: "Sign in with your password" }).click();
    await expect(status).toHaveText("That password didn't work. Try again.");
    await status.evaluate(n => { n.textContent = ""; });
  }
  await page.getByLabel("Password").fill(PASSWORD);
  await page.getByRole("button", { name: "Sign in with your password" }).click();
  await expect(status).toContainText("too many wrong passwords");
  await expect(status).toContainText("Try again in");
  await page.getByRole("button", { name: "Sign in with a passkey" }).click();
  await expect(page.locator("#compose")).toBeVisible();
  // The failures show in Access.
  await page.locator('.tab[data-view="tokens"]').click();
  await expect(page.locator("#access .acc-events")).toContainText("Failed sign-in with the password");
});

test("a page reached at another address says where to sign in", async ({ browser }) => {
  const other = await browser.newContext({ baseURL: `http://127.0.0.1:${PORT}` });
  const p = await other.newPage();
  await p.goto("/");
  await expect(p.locator("#auth-warn")).toContainText(`Sign in at ${BASE}`);
  await other.close();
});
