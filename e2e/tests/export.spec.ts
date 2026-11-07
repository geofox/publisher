import { test, expect, type Page, type BrowserContext } from "@playwright/test";
import { readFileSync } from "node:fs";

// Export and import (Settings → System), against the local sign-in
// harness: the owner exports, the accounts added in the app are deleted
// (a new server, as far as the import is concerned), and the file comes
// back with its passphrase, confirmed. Each step that needs it confirms
// with the passkey.

const PORT = Number(process.env.E2E_LOCAL_PORT || Number(process.env.E2E_PORT || 4173) + 1);
const BASE = `http://localhost:${PORT}`;
const RUN = "x" + Date.now().toString(36);
const HANDLE = `port-${RUN}.example`;

test.describe.configure({ mode: "serial", retries: 0 });

let context: BrowserContext;
let page: Page;
const errors: string[] = [];
let file = "";
let passphrase = "";

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
  expect((await page.request.post("/__e2e/reset-login")).status()).toBe(204);
  await page.goto(await (await page.request.post("/__e2e/setup-url")).text());
  await page.getByRole("button", { name: "Create a passkey" }).click();
  await expect(page).toHaveURL(BASE + "/");
  // Start from a server whose accounts all come from the environment,
  // then add one in the app, and a setting.
  expect((await page.request.post("/__e2e/forget-app-accounts")).status()).toBe(204);
  const add = await page.request.post("/v1/accounts", { data: { type: "bluesky", method: "app_password", label: "Ported " + RUN,
    config: { pds_url: "https://pds.e2e.invalid" }, credentials: { identifier: HANDLE, app_password: "pw-" + RUN } } });
  expect(add.status(), await add.text()).toBe(201);
  const set = await page.request.patch("/v1/settings", { headers: { "If-Match": "*" }, data: { publishing: { max_attempts: 4 } } });
  expect(set.status(), await set.text()).toBe(200);
});

test.afterAll(async () => {
  // The harness is shared with the specs that follow: take away the
  // account and the setting this spec made.
  expect((await page.request.post("/__e2e/forget-app-accounts")).status()).toBe(204);
  const reset = await page.request.patch("/v1/settings", { headers: { "If-Match": "*" }, data: { publishing: { max_attempts: null } } });
  expect(reset.status(), await reset.text()).toBe(200);
  expect(errors).toEqual([]);
  await context.close();
});

const section = () => page.locator("#set-port");

async function openSystem() {
  await page.goto(BASE + "/");
  await page.locator('.tab[data-view="settings"]').click();
  await page.locator(".set-nav").getByRole("button", { name: "System" }).click();
  await expect(section().getByRole("button", { name: "Export…" })).toBeVisible();
}

async function confirmWithPasskey() {
  const d = page.getByRole("dialog", { name: "Confirm it's you" });
  await d.getByRole("button", { name: "Use your passkey" }).click();
  await expect(d).toHaveCount(0);
}

const accountByHandle = async (handle: string) => ((await (await page.request.get("/v1/accounts?limit=100")).json()).data as any[])
  .find(a => a.handle === handle);

test("export: the passphrase shown once, and the file", async () => {
  await openSystem();
  await section().getByRole("button", { name: "Export…" }).click();
  await confirmWithPasskey();
  const d = page.getByRole("dialog", { name: "Your export" });
  await expect(d.locator(".port-words li")).toHaveCount(24);
  passphrase = (await d.locator(".port-words li").allTextContents()).join(" ");
  expect(passphrase).toMatch(/^[a-z]+( [a-z]+){23}$/);
  // Closing before saving the file asks first.
  await d.getByRole("button", { name: "Done" }).click();
  await expect(d.getByRole("alert")).toContainText("You haven't downloaded the file");
  const dl = page.waitForEvent("download");
  await d.getByRole("button", { name: "Download the file" }).click();
  const got = await dl;
  expect(got.suggestedFilename()).toMatch(/^publisher-\d{4}-\d{2}-\d{2}\.export$/);
  file = readFileSync(await got.path(), "utf8");
  expect(file.startsWith('{"format":"publisher-export","version":1,')).toBe(true);
  expect(file).not.toContain("pw-" + RUN);
  await expect(d.getByRole("alert")).toBeHidden();
  await d.getByRole("button", { name: "Done" }).click();
  await expect(d).toHaveCount(0);
});

test("import: refused while the server has accounts added in the app", async () => {
  await expect(section().locator(".port-blocked")).toBeVisible();
  await expect(section().getByRole("button", { name: "Read the file" })).toBeDisabled();
  const refused = await page.request.post("/v1/import", { data: { file, passphrase } });
  expect(refused.status()).toBe(409);
  expect((await refused.json()).code).toBe("invalid_state");
  expect((await page.request.post("/__e2e/forget-app-accounts")).status()).toBe(204);
  expect(await accountByHandle(HANDLE)).toBeUndefined();
  await openSystem();
  await expect(section().locator(".port-blocked")).toBeHidden();
});

test("import: a wrong passphrase, then the summary, then the import", async () => {
  await section().getByLabel("Export file").setInputFiles({ name: "publisher.export", mimeType: "application/octet-stream", buffer: Buffer.from(file) });
  // Words of the list, but not this file's.
  const words = passphrase.split(" ");
  await section().getByLabel("Passphrase").fill([...words.slice(1), words[0]].join(" "));
  await section().getByRole("button", { name: "Read the file" }).click();
  await expect(section().locator("#imp-pass-err")).toContainText("doesn't open this file");
  await expect(section().getByLabel("Passphrase")).toHaveAttribute("aria-invalid", "true");

  // As pasted from a note: other case, a word a line.
  await section().getByLabel("Passphrase").fill(passphrase.toUpperCase().split(" ").join("\n"));
  await section().getByRole("button", { name: "Read the file" }).click();
  const summary = section().locator(".port-summary");
  await expect(summary).toContainText("What this file would do");
  const line = summary.locator(".port-list li", { hasText: "Ported " + RUN });
  await expect(line).toContainText("credentials go to https://pds.e2e.invalid");
  await expect(line).toContainText("Will be added");
  await expect(summary.locator(".port-list li", { hasText: "Tries before giving up" }).locator(".port-where code")).toHaveText("4");
  expect(await accountByHandle(HANDLE)).toBeUndefined();

  await summary.getByRole("button", { name: "Import" }).click();
  await confirmWithPasskey();
  await expect(summary).toContainText("Imported");
  await expect(summary.locator(".port-list li", { hasText: "Ported " + RUN })).toContainText("Connected as " + HANDLE);
  const a = await accountByHandle(HANDLE);
  expect(a && a.source).toBe("db");
  expect(a.label).toBe("Ported " + RUN);
  const s = await (await page.request.get("/v1/settings")).json();
  expect(s.publishing.max_attempts.value).toBe(4);
  // The server has an account added in the app again: no second import.
  await summary.getByRole("button", { name: "Done" }).click();
  await expect(section().locator(".port-blocked")).toBeVisible();
});
