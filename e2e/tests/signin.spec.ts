import { test, expect, type Page, type BrowserContext, type CDPSession } from "@playwright/test";

// Local sign-in (AUTH_MODE=local) against the second harness, at
// http://localhost:<port> (a secure context), with a virtual passkey
// authenticator. The steps share one browser and one server, in order, so
// they never retry on their own: setup (and its dead ends) → passkey →
// signed in; a password; confirming for tokens; another browser's
// session; managing passkeys; sign out and in; wrong passwords throttle
// the password but not the passkey; another address; setup is closed.
// The run starts from "sign-in isn't set up" (the harness's reset-login),
// so a reused server works too.

const PORT = Number(process.env.E2E_LOCAL_PORT || Number(process.env.E2E_PORT || 4173) + 1);
const BASE = `http://localhost:${PORT}`;
const PASSWORD = "a long enough passphrase";
const PASSWORD2 = "another long passphrase";

test.describe.configure({ mode: "serial", retries: 0 });

let context: BrowserContext;
let page: Page;
let cdp: CDPSession;
let authenticatorId = "";
const errors: string[] = [];

// Watch a page for errors: script errors, and console errors other than
// the failed requests the steps cause on purpose (a 4xx answer logs one).
function watch(p: Page) {
  p.on("pageerror", e => errors.push(String(e)));
  p.on("console", m => {
    if (m.type() === "error" && !/Failed to load resource: the server responded with a status of 4\d\d/.test(m.text())) errors.push(m.text());
  });
}

test.beforeAll(async ({ browser }) => {
  context = await browser.newContext({ baseURL: BASE });
  page = await context.newPage();
  watch(page);
  expect((await page.request.post("/__e2e/reset-login")).status()).toBe(204);
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

async function setupLink() {
  return (await page.request.post("/__e2e/setup-url")).text();
}

const confirmDialog = () => page.getByRole("dialog", { name: "Confirm it's you" });
const access = () => page.locator("#access");

async function openAccess() {
  await page.locator('.tab[data-view="tokens"]').click();
  await expect(access().getByText("where you're signed in")).toBeVisible();
}

test("before setup, the app shows how to set up sign-in", async () => {
  await page.goto("/");
  await expect(page.getByRole("heading", { name: "Sign in" })).toBeVisible();
  await expect(page.getByText("Sign-in isn't set up yet.")).toBeVisible();
  // The API wants a session.
  expect((await page.request.get("/v1/accounts")).status()).toBe(401);
  // The setup page without a link says to open one.
  await page.goto("/setup");
  await expect(page.getByText("Open the setup link from the server's log")).toBeVisible();
});

test("a setup link opened at another address is handed to Publisher's address, unused", async ({ browser }) => {
  const link = await setupLink();
  const token = link.split("#")[1];
  const other = await browser.newContext({ baseURL: `http://127.0.0.1:${PORT}` });
  const p = await other.newPage();
  watch(p);
  await p.goto("/setup#" + token);
  await expect(p.locator("#auth-warn")).toContainText(`Set up sign-in at ${BASE}`);
  await expect(p.locator("#auth-warn").getByRole("link", { name: "Open the setup there" })).toHaveAttribute("href", link);
  await other.close();
  // The link still works where it belongs.
  await page.goto(link);
  await expect(page).toHaveURL(`${BASE}/setup`);
  await expect(page.getByRole("button", { name: "Create a passkey" })).toBeVisible();
});

test("a newer link replaces an opened one; opening it in the same tab starts over", async () => {
  const a = await setupLink();
  await page.goto(a);
  await expect(page).toHaveURL(`${BASE}/setup`);
  await expect(page.getByRole("button", { name: "Create a passkey" })).toBeVisible();
  const b = await setupLink(); // `publisher setup-url` again: a is dead
  await page.getByRole("button", { name: "Create a passkey" }).click();
  await expect(page.getByText("a newer setup link replaced it")).toBeVisible();
  expect(await credentials()).toHaveLength(0);
  // The newer link, opened in this tab: only the fragment changes.
  await page.goto(b);
  await expect(page).toHaveURL(`${BASE}/setup`);
  await expect(page.getByRole("button", { name: "Create a passkey" })).toBeVisible();
});

test("setup with a passkey signs the owner in", async () => {
  const link = await setupLink();
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
  await openAccess();
  await expect(access().getByText("Test laptop")).toBeVisible();
  await expect(access().getByText("this browser")).toBeVisible();
  await expect(access().getByText("Signed in with setup")).toBeVisible();
  // The only way to sign in can't be removed: said up front.
  await expect(access().getByRole("button", { name: /Remove Test laptop.*last way to sign in/ })).toBeDisabled();

  await access().getByRole("button", { name: "Set a password" }).click();
  await page.getByLabel("New password (at least 12 characters)").fill(PASSWORD);
  await page.getByLabel("The new password again").fill(PASSWORD);
  await page.getByRole("dialog").getByRole("button", { name: "Set", exact: true }).click();
  // Re-authentication with the passkey.
  await expect(confirmDialog()).toContainText("set your password");
  await confirmDialog().getByRole("button", { name: "Use your passkey" }).click();
  await expect(confirmDialog()).toBeHidden();
  await expect(access().getByText("Set a password", { exact: true })).toBeVisible(); // in the activity list
  await expect(access().getByRole("button", { name: "Change" })).toBeVisible();
  // Focus came back to the button the dialog was opened from (redrawn).
  await expect(access().getByRole("button", { name: "Change" })).toBeFocused();
  await expect(access().getByRole("button", { name: /Remove Test laptop/ })).toBeEnabled();
});

test("every token asks to confirm; the password works for it; Escape and Cancel cancel", async () => {
  await page.locator("#tok-name-input").fill("deploy bot");
  await page.locator('#tok-scopes input[value="admin"]').check();
  await page.locator("#tok-create-btn").click();
  await expect(confirmDialog()).toContainText("create a token");
  await confirmDialog().getByLabel("Your password").fill("not the password");
  await confirmDialog().getByRole("button", { name: "Confirm with your password" }).click();
  await expect(confirmDialog()).toContainText("That isn't your password.");
  await confirmDialog().getByLabel("Your password").fill(PASSWORD);
  await confirmDialog().getByRole("button", { name: "Confirm with your password" }).click();
  await expect(page.locator(".tok-secret-input")).toHaveValue(/.{20,}/);
  await page.getByRole("button", { name: "Done" }).click();
  await expect(page.locator("#tokens-list")).toContainText("deploy bot");
  // A posting token asks too (it outlives the session); Escape cancels.
  await page.locator("#tok-name-input").fill("second bot");
  await page.locator("#tok-create-btn").click();
  await expect(confirmDialog()).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(confirmDialog()).toBeHidden();
  await expect(page.locator(".toast", { hasText: "Not created: you didn't confirm it's you." })).toBeVisible();
  // Cancel too.
  await page.locator("#tok-create-btn").click();
  await confirmDialog().getByRole("button", { name: "Cancel" }).click();
  await expect(page.locator(".toast", { hasText: "Not created: you didn't confirm it's you." }).last()).toBeVisible();
  await expect(page.locator("#tokens-list")).not.toContainText("second bot");
  // Focus is back on Create once the request is over.
  await expect(page.locator("#tok-create-btn")).toBeFocused();
});

test("another browser's session can be signed out; changing the password takes the current one", async ({ browser }) => {
  const other = await browser.newContext({ baseURL: BASE });
  const p2 = await other.newPage();
  watch(p2);
  await p2.goto("/");
  await p2.getByLabel("Password").fill(PASSWORD);
  await p2.getByRole("button", { name: "Sign in with your password" }).click();
  await expect(p2.locator("#compose")).toBeVisible();

  await openAccess();
  const rows = access().getByRole("button", { name: /^Sign out/ });
  await expect(rows).toHaveCount(2);
  await access().getByRole("button", { name: /^Sign out (?!of this browser)/ }).click();
  await expect(rows).toHaveCount(1);
  expect((await p2.request.get("/v1/accounts")).status()).toBe(401);
  await other.close();

  // No "confirm it's you": the current password is the proof.
  await access().getByRole("button", { name: "Change" }).click();
  await page.getByLabel("Current password").fill(PASSWORD);
  await page.getByLabel("New password (at least 12 characters)").fill(PASSWORD2);
  await page.getByLabel("The new password again").fill(PASSWORD2);
  await page.getByRole("button", { name: "Change", exact: true }).last().click();
  await expect(page.locator(".toast", { hasText: "Password changed" })).toBeVisible();
  await expect(confirmDialog()).toHaveCount(0);
});

test("passkeys can be added, renamed and removed; the last way to sign in can't be", async () => {
  // The device lost its passkey (cleared): add another, confirming with the
  // password.
  await cdp.send("WebAuthn.clearCredentials", { authenticatorId });
  await access().getByRole("button", { name: "Add a passkey" }).click();
  const name = page.getByLabel("Name it (to tell it apart later)");
  await expect(name).not.toHaveValue("Test laptop");
  await name.fill("Work key");
  await page.getByRole("dialog").getByRole("button", { name: "Create", exact: true }).click();
  await confirmDialog().getByLabel("Your password").fill(PASSWORD2);
  await confirmDialog().getByRole("button", { name: "Confirm with your password" }).click();
  await expect(page.locator(".toast", { hasText: "Passkey added" })).toBeVisible();
  await expect(access().getByText("Work key")).toBeVisible();
  expect(await credentials()).toHaveLength(1);

  await access().getByRole("button", { name: "Rename Work key" }).click();
  await page.getByLabel("Name", { exact: true }).fill("Phone");
  await page.getByRole("dialog").getByRole("button", { name: "Save" }).click();
  await expect(access().getByText("Phone", { exact: true })).toBeVisible();
  await expect(access().getByRole("button", { name: "Rename Phone" })).toBeFocused();

  // Remove the lost one, confirming with the new passkey.
  await access().getByRole("button", { name: /Remove Test laptop/ }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Remove" }).click();
  await confirmDialog().getByRole("button", { name: "Use your passkey" }).click();
  await expect(page.locator(".toast", { hasText: "Passkey removed" })).toBeVisible();
  await expect(access().getByText("Test laptop")).toHaveCount(0);

  // Remove the password: the passkey is then the last way in.
  await access().getByRole("button", { name: /Remove the password/ }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Remove" }).click();
  await confirmDialog().getByRole("button", { name: "Use your passkey" }).click();
  await expect(page.locator(".toast", { hasText: "Password removed" })).toBeVisible();
  await expect(access().getByRole("button", { name: /Remove Phone.*last way to sign in/ })).toBeDisabled();

  // Set it again for what follows.
  await access().getByRole("button", { name: "Set a password" }).click();
  await page.getByLabel("New password (at least 12 characters)").fill(PASSWORD);
  await page.getByLabel("The new password again").fill(PASSWORD);
  await page.getByRole("dialog").getByRole("button", { name: "Set", exact: true }).click();
  await confirmDialog().getByRole("button", { name: "Use your passkey" }).click();
  await expect(page.locator(".toast", { hasText: "Password set" })).toBeVisible();
});

test("sign out, then in with the password", async () => {
  await access().getByRole("button", { name: "Sign out of this browser" }).click();
  await expect(page.getByRole("heading", { name: "Sign in" })).toBeVisible();
  expect((await page.request.get("/v1/accounts")).status()).toBe(401);
  await page.getByLabel("Password").fill(PASSWORD);
  await page.getByRole("button", { name: "Sign in with your password" }).click();
  await expect(page.locator("#compose")).toBeVisible();
});

test("wrong passwords are throttled, and a passkey still signs in", async () => {
  await openAccess();
  await access().getByRole("button", { name: "Sign out of this browser" }).click();
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
  await openAccess();
  await expect(page.locator("#access .acc-events")).toContainText("Failed sign-in with the password");
});

test("a page reached at another address says where to sign in", async ({ browser }) => {
  const other = await browser.newContext({ baseURL: `http://127.0.0.1:${PORT}` });
  const p = await other.newPage();
  watch(p);
  await p.goto("/");
  await expect(p.locator("#auth-warn")).toContainText(`Sign in at ${BASE}`);
  await expect(p.locator("#auth-warn").getByRole("link", { name: "Go there" })).toHaveAttribute("href", `${BASE}/`);
  // Setup is done: a setup link says so there too.
  await p.goto("/setup#sometoken");
  await expect(p.getByRole("heading", { name: "Setup is done" })).toBeVisible();
  await other.close();
});
