import { test, expect, type Page, type BrowserContext, type Locator } from "@playwright/test";
import { randomBytes } from "node:crypto";

// Settings → Accounts against the local sign-in harness, which has a master
// key: accounts added in the app (Bluesky with an app password, Mastodon
// and Threads with a token, Nostr generated then shown once, or imported),
// the same account twice refused, credentials the network refuses shown on
// the form, renaming, turning off and deleting (which asks the owner to
// confirm it's them), and a new account in the composer, not picked for new
// posts. The harness's fake networks refuse a credential starting with
// "bad", and a token "<user>:…" belongs to <user>.
//
// The steps share one browser and one server, in order. Names carry this
// run's mark, so a reused server (whose accounts stay) works too.

const PORT = Number(process.env.E2E_LOCAL_PORT || Number(process.env.E2E_PORT || 4173) + 1);
const BASE = `http://localhost:${PORT}`;
const RUN = "r" + Date.now().toString(36);

test.describe.configure({ mode: "serial", retries: 0 });

let context: BrowserContext;
let page: Page;
const errors: string[] = [];

test.beforeAll(async ({ browser }) => {
  context = await browser.newContext({ baseURL: BASE });
  page = await context.newPage();
  page.on("pageerror", e => errors.push(String(e)));
  page.on("console", m => {
    if (m.type() === "error" && !/Failed to load resource: the server responded with a status of 4\d\d/.test(m.text())) errors.push(m.text());
  });
  const cdp = await context.newCDPSession(page);
  await cdp.send("WebAuthn.enable");
  await cdp.send("WebAuthn.addVirtualAuthenticator", {
    options: {
      protocol: "ctap2", transport: "internal", hasResidentKey: true,
      hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true,
    },
  });
  // Signed in with a passkey, from a fresh setup.
  expect((await page.request.post("/__e2e/reset-login")).status()).toBe(204);
  await page.goto(await (await page.request.post("/__e2e/setup-url")).text());
  await page.getByRole("button", { name: "Create a passkey" }).click();
  await expect(page).toHaveURL(BASE + "/");
  // The app is up (its tabs answer) once the composer lists the accounts.
  await expect(page.locator("#chips .target-row").first()).toBeVisible();
});

test.afterAll(async () => {
  expect(errors).toEqual([]);
  await context.close();
});

const panel = () => page.locator("#set-accounts");
const dialog = () => page.locator(".acct-dlg");
const card = (text: string) => panel().locator(".acct-card", { hasText: text });
const status = () => dialog().locator(".acct-dlg-status");

async function openAccounts() {
  await page.locator('.tab[data-view="settings"]').click();
  await page.locator(".set-nav").getByRole("button", { name: "Accounts" }).click();
  await expect(panel().getByRole("button", { name: "Add an account" })).toBeEnabled();
}

// startAdding opens the add dialog at a network's form (picking the way
// of connecting when it has several).
async function startAdding(network: string, way?: string) {
  await panel().getByRole("button", { name: "Add an account" }).click();
  await dialog().getByRole("button", { name: network, exact: true }).click();
  if (way) await dialog().getByRole("button", { name: new RegExp("^" + way) }).click();
  await expect(dialog().locator(".modal-title")).toHaveText(`Add a ${network} account`);
}

const field = (label: string) => dialog().getByLabel(label, { exact: true });

async function submit(name = "Add the account") {
  await dialog().getByRole("button", { name }).click();
}

async function confirmWithPasskey() {
  const d = page.getByRole("dialog", { name: "Confirm it's you" });
  await d.getByRole("button", { name: "Use your passkey" }).click();
  await expect(d).toHaveCount(0);
}

test("add a Bluesky account with an app password; a main password is only warned about", async () => {
  await openAccounts();
  await startAdding("Bluesky");
  // The handle and app password come first; the server is found from the
  // handle unless given (under Advanced).
  await expect(field("Handle, DID or email")).toBeFocused();
  await field("Handle, DID or email").fill(`${RUN}.bsky.e2e.invalid`);
  await field("App password").fill("my main password");
  await expect(dialog().locator(`[data-key="app_password"] .fld-warn`)).toContainText("doesn't look like an app password");
  await field("App password").fill("abcd-efgh-ijkl-mnop");
  await expect(dialog().locator(`[data-key="app_password"] .fld-warn`)).toBeEmpty();
  await dialog().locator(".fld-adv summary").click();
  await field("PDS address (optional)").fill("https://pds.e2e.invalid");
  await submit();
  await expect(dialog()).toHaveCount(0);
  const c = card(`${RUN}.bsky.e2e.invalid`);
  await expect(c.locator(".acct-status")).toHaveText("Connected");
  // Added in the app: deletable, not picked for new posts.
  await expect(c.getByRole("button", { name: /^Delete / })).toBeVisible();
  await expect(c.getByLabel("Picked for new posts")).not.toBeChecked();
  // Its credentials are never shown back: set, with when.
  const acct = (await (await page.request.get("/v1/accounts?limit=100")).json()).data
    .find((a: any) => a.handle === `${RUN}.bsky.e2e.invalid`);
  expect(acct.config).toEqual({ identifier: `${RUN}.bsky.e2e.invalid`, pds_url: "https://pds.e2e.invalid" });
  expect(Object.keys(acct.credentials)).toEqual(["app_password"]);
  expect(acct.credentials.app_password.set).toBe(true);
  expect(JSON.stringify(acct)).not.toContain("abcd-efgh-ijkl-mnop");
});

test("Mastodon: credentials the network refuses stay on the form; the same account twice is refused", async () => {
  await startAdding("Mastodon", "Access token");
  await expect(field("Server address")).toBeFocused();
  await field("Server address").fill(`https://${RUN}.e2e.invalid`);
  await field("Access token").fill("bad-token");
  await submit();
  await expect(status()).toHaveText("Mastodon refused these credentials: Invalid identifier or password");
  await expect(dialog()).toBeVisible();
  // An address that isn't one is said on its field.
  await field("Server address").fill("not an address");
  await field("Access token").fill("alice:one");
  await submit();
  await expect(dialog().locator('.fld[data-key="instance_url"] .fld-err')).not.toBeEmpty();
  await expect(field("Server address")).toBeFocused();
  await field("Server address").fill(`https://${RUN}.e2e.invalid`);
  await submit();
  await expect(dialog()).toHaveCount(0);
  await expect(card(`@alice@${RUN}.e2e.invalid`)).toBeVisible();

  // The same account again (another token of alice's): refused.
  await startAdding("Mastodon", "Access token");
  await field("Server address").fill(`https://${RUN}.e2e.invalid`);
  await field("Access token").fill("alice:two");
  await submit();
  await expect(status()).toContainText("This account is already here");
  await dialog().getByRole("button", { name: "Close" }).click();
  await expect(dialog()).toHaveCount(0);
});

test("Threads with a token", async () => {
  await startAdding("Threads", "Long-lived access token");
  await field("Access token").fill(`t${RUN}:token`);
  await submit();
  await expect(dialog()).toHaveCount(0);
  await expect(card(`t${RUN}`).locator(".acct-status")).toHaveText("Connected");
});

let nsec = "";
test("Nostr: a new key is made, then shown once after confirming it's you", async () => {
  await startAdding("Nostr", "Make a new key");
  await field("Home relay").fill("wss://relay.e2e.invalid");
  await field("Label (optional)").fill(`Fresh ${RUN}`);
  await submit("Make the key and add it");
  await confirmWithPasskey();
  const shown = page.getByRole("dialog", { name: "Keep a copy of the new key" });
  await expect(shown).toBeVisible();
  nsec = await shown.getByLabel("Secret key").inputValue();
  expect(nsec).toMatch(/^nsec1[02-9ac-hj-np-z]{58}$/);
  // Done waits for the owner to say the key is saved; Escape doesn't close.
  await expect(shown.getByRole("button", { name: "Done" })).toBeDisabled();
  await page.keyboard.press("Escape");
  await expect(shown).toBeVisible();
  await shown.getByLabel("I've saved it somewhere safe").check();
  await shown.getByRole("button", { name: "Done" }).click();
  await expect(shown).toHaveCount(0);
  // Shown again on request, after confirming again.
  const c = card(`Fresh ${RUN}`);
  await c.getByRole("button", { name: /^Show key: / }).click();
  await confirmWithPasskey();
  const again = page.getByRole("dialog", { name: /^The key of/ });
  await expect(again.getByLabel("Secret key")).toHaveValue(nsec);
  await again.getByRole("button", { name: "Done" }).click();
  await expect(again).toHaveCount(0);
});

test("Nostr: a key imported as nsec1 or hex; the same key twice is refused", async () => {
  await startAdding("Nostr", "Secret key");
  await field("Home relay").fill("wss://relay.e2e.invalid");
  await field("Secret key").fill(nsec);
  await submit();
  await expect(status()).toContainText("This account is already here");
  const hex = randomBytes(32).toString("hex");
  await field("Secret key").fill(hex);
  await field("Label (optional)").fill(`Imported ${RUN}`);
  await submit();
  await expect(dialog()).toHaveCount(0);
  await expect(card(`Imported ${RUN}`).locator(".acct-status")).toHaveText("Connected");
});

test("rename, turn off, and the composer: a new account is there, not picked", async () => {
  const c = card(`@alice@${RUN}.e2e.invalid`);
  await c.getByRole("button", { name: /^Rename/ }).click();
  const rename = page.getByRole("dialog", { name: /^Rename/ });
  await rename.getByLabel("What you call it here").fill(`Alice ${RUN}`);
  await rename.getByRole("button", { name: "Save" }).click();
  await expect(card(`Alice ${RUN}`)).toBeVisible();

  // In the composer: listed, not selected.
  const id = await card(`Alice ${RUN}`).getAttribute("data-account");
  await page.locator('.tab[data-view="compose"]').click();
  const row = page.locator(`#chips [data-account="${id}"]`);
  await expect(row).toBeVisible();
  await expect(row).toHaveAttribute("aria-checked", "false");

  // Turned off: the composer shows it as off.
  await openAccounts();
  const box = card(`Alice ${RUN}`).getByLabel("On", { exact: true });
  await box.uncheck();
  await expect(card(`Alice ${RUN}`).locator(".acct-status")).toHaveText("Off");
  await expect(card(`Alice ${RUN}`).getByLabel("On", { exact: true })).toBeFocused();
  await card(`Alice ${RUN}`).getByLabel("On", { exact: true }).check();
  await expect(card(`Alice ${RUN}`).locator(".acct-status")).toHaveText("Connected");
});

test("its settings: a new server address needs the credentials again; a new token keeps the account", async () => {
  const c = card(`Alice ${RUN}`);
  await c.getByRole("button", { name: /^Settings of/ }).click();
  await field("Server address").fill(`https://other${RUN}.e2e.invalid`);
  await dialog().getByRole("button", { name: "Save" }).click();
  await expect(status()).toHaveText("A new server address needs the credentials again: type them above.");
  await field("Server address").fill(`https://${RUN}.e2e.invalid`);
  await field("Access token").fill("alice:three");
  await dialog().getByRole("button", { name: "Save" }).click();
  await expect(dialog()).toHaveCount(0);
  // Another user's token is another account.
  await c.getByRole("button", { name: /^Settings of/ }).click();
  await field("Access token").fill("bob:one");
  await dialog().getByRole("button", { name: "Save" }).click();
  await expect(status()).toContainText("These credentials are for another Mastodon account");
  await dialog().getByRole("button", { name: "Cancel" }).click();
});

test("delete asks to confirm it's you; cancelling keeps the account", async () => {
  const c = card(`Alice ${RUN}`);
  await c.getByRole("button", { name: /^Delete/ }).click();
  await page.locator(".modal.confirm").getByRole("button", { name: "Delete" }).click();
  const confirm = page.getByRole("dialog", { name: "Confirm it's you" });
  await expect(confirm).toContainText("To delete this account, confirm it's you.");
  await confirm.getByRole("button", { name: "Cancel" }).click();
  await expect(c).toBeVisible();
  await c.getByRole("button", { name: /^Delete/ }).click();
  await page.locator(".modal.confirm").getByRole("button", { name: "Delete" }).click();
  await confirmWithPasskey();
  await expect(c).toHaveCount(0);
  // The keyboard goes to Add an account; a screen reader hears it went.
  await expect(panel().getByRole("button", { name: "Add an account" })).toBeFocused();
  await expect(page.locator("[data-flash-live]")).toContainText("Deleted");
  // Gone from the composer too.
  await page.locator('.tab[data-view="compose"]').click();
  await expect(page.locator("#chips")).not.toContainText(`Alice ${RUN}`);
});

test("an account from the environment can't be changed or deleted here, only moved into the app", async () => {
  await openAccounts();
  const env = panel().locator(".acct-card", { has: page.locator(".acc-tag", { hasText: "from the environment" }) }).first();
  await expect(env.getByRole("button", { name: /^Delete/ })).toHaveCount(0);
  await env.getByRole("button", { name: /^Settings of/ }).click();
  await expect(dialog()).toContainText("This account comes from the server's environment");
  await expect(dialog().getByRole("button", { name: "Move into the app" })).toBeEnabled();
  await dialog().getByRole("button", { name: "Close", exact: true }).last().click();
});

test("a new server starts with its first steps", async () => {
  // As a server with no account and no master key shows it.
  const p = await context.newPage();
  p.on("pageerror", e => errors.push(String(e)));
  await p.route("**/v1/accounts?*", r => r.fulfill({ json: { data: [], next_cursor: null } }));
  await p.route("**/v1/instance", async r => {
    const j = await (await r.fetch()).json();
    r.fulfill({ json: { ...j, secrets: { status: "none", detail: "no master key" } } });
  });
  await p.goto("/");
  const first = p.locator(".set-first");
  await expect(first).toBeVisible(); // Settings opened by itself
  await expect(p.locator("#set-first-title")).toBeFocused();
  const key = await first.getByLabel("A new master key").inputValue();
  expect(Buffer.from(key, "base64")).toHaveLength(32);
  await expect(first).toContainText(`printf '%s' '${key}' > publisher_master_key`);
  await expect(first.getByRole("button", { name: "Add an account" })).toBeDisabled();
  await first.getByRole("button", { name: "Skip: I'll use the server's environment" }).click();
  await expect(first).toContainText("Skipped: add accounts in the server's environment");
  await expect(first.getByRole("button", { name: "Set one after all" })).toBeFocused();
  // Once only: the next visit opens the composer, which says where to go.
  await p.reload();
  await expect(p.locator("#compose")).toBeVisible();
  await expect(p.locator("#chips")).toContainText("No accounts yet: add one in Settings.");
  await p.locator("#chips").getByRole("button", { name: "Open Settings" }).click();
  await expect(p.locator(".set-first")).toBeVisible();
  await p.close();
});

test("a failed read of the accounts says so, with Try again, not the first steps", async () => {
  const p = await context.newPage();
  p.on("pageerror", e => errors.push(String(e)));
  let fail = true;
  await p.route("**/v1/accounts?*", r => fail
    ? r.fulfill({ status: 503, contentType: "application/problem+json",
      body: JSON.stringify({ title: "Unavailable", status: 503, detail: "the database is busy", code: "unavailable" }) })
    : r.fallback());
  await p.goto("/");
  await expect(p.locator("#chips")).toContainText("Couldn't read your accounts");
  await p.locator('.tab[data-view="settings"]').click();
  await p.locator('.set-nav [data-set="accounts"]').click();
  const box = p.locator("#set-accounts");
  await expect(box).toContainText("Couldn't read your accounts: the database is busy");
  await expect(box.locator(".set-first")).toHaveCount(0);
  fail = false;
  await box.getByRole("button", { name: "Try again" }).click();
  await expect(box.locator(".acct-card").first()).toBeVisible();
  await expect(box.locator(".set-note.bad")).toHaveCount(0);
  await p.close();
});

test("an account from the environment whose variables are gone can be deleted, not moved", async () => {
  const p = await context.newPage();
  p.on("pageerror", e => errors.push(String(e)));
  await p.route("**/v1/accounts?*", async r => {
    const j = await (await r.fetch()).json();
    const base = j.data.find((a: { source: string }) => a.source === "env") || j.data[0];
    j.data.push({ ...base, id: "acc_gone_e2e", type: "threads", source: "env", status: "not_configured",
      status_reason: "THREADS_ACCESS_TOKEN isn't set", label: "Gone threads", handle: "", profile_url: "", health: null });
    await r.fulfill({ json: j });
  });
  await p.goto("/");
  await expect(p.locator("#chips .target-row").first()).toBeVisible();
  await p.locator('.tab[data-view="settings"]').click();
  await p.locator('.set-nav [data-set="accounts"]').click();
  const gone = p.locator('#set-accounts .acct-card[data-account="acc_gone_e2e"]');
  await expect(gone.getByRole("button", { name: /^Delete/ })).toBeVisible();
  await gone.getByRole("button", { name: /^Settings of/ }).click();
  const d = p.locator(".acct-dlg");
  await expect(d).toContainText("doesn't set it any more");
  await expect(d.getByRole("button", { name: "Move into the app" })).toHaveCount(0);
  await expect(d.getByRole("button", { name: "Delete" })).toBeVisible();
  await p.close();
});
