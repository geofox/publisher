import { test, expect, type Page, type BrowserContext, type Route } from "@playwright/test";
import { createHash } from "node:crypto";

// Connecting accounts by signing in at their network, against the local
// sign-in harness: the real Mastodon and Threads drivers reach the
// harness's fake sign-in servers (e2e/harness/oauth.go) over TLS. The
// network's sign-in page is this spec's: Playwright answers
// https://*.e2e.invalid/oauth/authorize in the new tab by sending it back
// to the harness's callback with a code (or a refusal). Networks send the
// owner back to https://pub.e2e.invalid (the harness's connect address),
// so the route sends the browser to this server's own callback instead.
//
// A Mastodon code is the token it becomes, "<user>:<anything>", whose user
// is the fake account's; a Threads code becomes the long-lived token. A
// sign-in page told to hold just shows itself, as one the owner hasn't
// finished.
//
// The steps share one browser and one server, in order. Names carry this
// run's mark, but the Threads step expects a server without its Threads
// app yet: a fresh one, as the harness starts.
//
// Every page logs when it opens a tab and when it starts a sign-in
// (window.__e2eLog), and opens none while window.__e2eBlockPopups is
// set, as a browser blocking pop-ups does.

const PORT = Number(process.env.E2E_LOCAL_PORT || Number(process.env.E2E_PORT || 4173) + 1);
const BASE = `http://localhost:${PORT}`;
const RUN = "c" + Date.now().toString(36);
const CONNECT_BASE = "https://pub.e2e.invalid";

test.describe.configure({ mode: "serial", retries: 0 });

let context: BrowserContext;
let page: Page;
const errors: string[] = [];
// expectedErrors are console errors a step causes on purpose.
let expectedErrors: RegExp[] = [];

// The sign-in pages: what each was asked, and what the next one answers.
type Authorize = { host: string; params: URLSearchParams };
const asked: Authorize[] = [];
let answer: { code?: string; error?: string; hold?: boolean } = {};

async function signInPage(route: Route) {
  const u = new URL(route.request().url());
  asked.push({ host: u.host, params: u.searchParams });
  if (answer.hold) {
    await route.fulfill({ status: 200, contentType: "text/html", body: "<!doctype html><title>Sign in</title><h1>Sign in at the server</h1>" });
    return;
  }
  const back = new URL(u.searchParams.get("redirect_uri") || "");
  const to = new URL(BASE + back.pathname);
  to.searchParams.set("state", u.searchParams.get("state") || "");
  if (answer.error) to.searchParams.set("error", answer.error);
  else to.searchParams.set("code", answer.code || "");
  await route.fulfill({ status: 302, headers: { location: to.toString() } });
}

test.beforeAll(async ({ browser }) => {
  context = await browser.newContext({ baseURL: BASE });
  await context.addInitScript(() => {
    const w = window as any;
    w.__e2eLog = [];
    const open = window.open.bind(window);
    window.open = (...args: Parameters<typeof window.open>) => { w.__e2eLog.push("open"); return w.__e2eBlockPopups ? null : open(...args); };
    const fetch0 = window.fetch.bind(window);
    window.fetch = (input: RequestInfo | URL, init?: RequestInit) => {
      const u = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (/:(re)?connect$/.test(u)) w.__e2eLog.push("start");
      return fetch0(input, init);
    };
  });
  await context.route(/^https:\/\/[a-z0-9.-]+\.e2e\.invalid\/oauth\/authorize\?/, signInPage);
  page = await context.newPage();
  page.on("pageerror", e => errors.push(String(e)));
  page.on("console", m => {
    if (m.type() === "error" && !/Failed to load resource: the server responded with a status of 4\d\d/.test(m.text())
      && !expectedErrors.some(x => x.test(m.text()))) errors.push(m.text());
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
  await expect(page.locator("#chips .target-row").first()).toBeVisible();
});

test.afterAll(async () => {
  expect(errors).toEqual([]);
  await context.close();
});

const panel = () => page.locator("#set-accounts");
const dialog = () => page.locator(".acct-dlg");
const card = (text: string) => panel().locator(".acct-card", { hasText: text });
const field = (label: string) => dialog().getByLabel(label, { exact: true });

async function openAccounts() {
  await page.locator('.tab[data-view="settings"]').click();
  await page.locator(".set-nav").getByRole("button", { name: "Accounts" }).click();
  await expect(panel().getByRole("button", { name: "Add an account" })).toBeEnabled();
}

async function startAdding(network: string, way: string) {
  await panel().getByRole("button", { name: "Add an account" }).click();
  await dialog().getByRole("button", { name: network, exact: true }).click();
  await dialog().getByRole("button", { name: new RegExp("^" + way) }).click();
  await expect(dialog().locator(".modal-title")).toHaveText(`Add a ${network} account`);
}

// signIn clicks what opens the network's sign-in, and answers the new
// tab, which shows the callback page's heading.
async function signIn(button: string, heading: string, a: typeof answer) {
  answer = a;
  const tab = context.waitForEvent("page");
  await dialog().getByRole("button", { name: button }).click();
  const t = await tab;
  await expect(t.getByRole("heading", { level: 1 })).toHaveText(heading);
  await t.close();
}

async function confirmWithPasskey() {
  const d = page.getByRole("dialog", { name: "Confirm it's you" });
  await d.getByRole("button", { name: "Use your passkey" }).click();
  await expect(d).toHaveCount(0);
}

const signIns = async () => (await (await page.request.get("/__e2e/sign-ins")).json()) as { host: string; path: string; values: Record<string, string[]> }[];
const accountBy = async (handle: string) => ((await (await page.request.get("/v1/accounts?limit=100")).json()).data as any[])
  .find(a => a.handle === handle);
const s256 = (v: string) => createHash("sha256").update(v).digest("base64url");
const log = () => page.evaluate(() => (window as any).__e2eLog as string[]);
const clearLog = () => page.evaluate(() => { (window as any).__e2eLog = []; });

// holdSignIn clicks what opens the network's sign-in, whose page then
// waits, and answers its tab.
async function holdSignIn(button: string) {
  answer = { hold: true };
  const tab = context.waitForEvent("page");
  await dialog().getByRole("button", { name: button }).click();
  const t = await tab;
  await expect(t.getByRole("heading", { level: 1 })).toHaveText("Sign in at the server");
  return t;
}

const MASTO = `m${RUN}.e2e.invalid`;

test("Mastodon 4.3 and later: sign in at the server in a new tab, and this tab finishes by itself", async () => {
  await openAccounts();
  await startAdding("Mastodon", "Sign in at your server");
  await expect(field("Server address")).toBeFocused();
  await field("Server address").fill(`https://${MASTO}`);
  await clearLog();
  await signIn("Sign in at Mastodon", "Connected", { code: `carol:${RUN}` });
  await expect(dialog()).toHaveCount(0);
  // The tab opened before the sign-in was asked for, while the click
  // still counted (a browser blocks one opened after an answer).
  expect(await log()).toEqual(["open", "start"]);
  const c = card(`@carol@${MASTO}`);
  await expect(c.locator(".acct-status")).toHaveText("Connected");
  await expect(c.getByRole("button", { name: /^Settings of / })).toBeFocused();

  // The sign-in asked for the app Publisher registered, with PKCE, and the
  // token request carried the verifier of that challenge.
  const a = asked.at(-1)!;
  expect(a.host).toBe(MASTO);
  expect(a.params.get("redirect_uri")).toBe(`${CONNECT_BASE}/oauth/callback/mastodon`);
  expect(a.params.get("scope")).toBe("read:accounts read:search read:statuses write:media write:statuses");
  expect(a.params.get("code_challenge_method")).toBe("S256");
  const calls = (await signIns()).filter(x => x.host === MASTO);
  expect(calls.map(x => x.path)).toEqual(["/api/v1/apps", "/oauth/token"]);
  expect(calls[0].values.redirect_uris).toEqual([`${CONNECT_BASE}/oauth/callback/mastodon`]);
  expect(a.params.get("client_id")).toBe(calls[1].values.client_id[0]);
  expect(s256(calls[1].values.code_verifier[0])).toBe(a.params.get("code_challenge"));
  const acct = await accountBy(`@carol@${MASTO}`);
  expect(acct.auth_method).toBe("oauth");
  expect(acct.config.instance_url).toBe(`https://${MASTO}`);
  expect(JSON.stringify(acct)).not.toContain(`carol:${RUN}`);
});

test("before 4.3: the standard sign-in paths, without PKCE; the app is registered once per server", async () => {
  const old = `old-${RUN}.e2e.invalid`;
  await startAdding("Mastodon", "Sign in at your server");
  await field("Server address").fill(`https://${old}`);
  await signIn("Sign in at Mastodon", "Connected", { code: `dave:${RUN}` });
  await expect(card(`@dave@${old}`).locator(".acct-status")).toHaveText("Connected");
  const a = asked.at(-1)!;
  expect(a.host).toBe(old);
  expect(a.params.has("code_challenge")).toBe(false);
  const calls = (await signIns()).filter(x => x.host === old);
  expect(calls.map(x => x.path)).toEqual(["/api/v1/apps", "/oauth/token"]);
  expect(calls[1].values.code_verifier).toBeUndefined();
});

test("declined at the server: both tabs say so, and it starts again", async () => {
  await startAdding("Mastodon", "Sign in at your server");
  await field("Server address").fill(`https://${MASTO}`);
  await signIn("Sign in at Mastodon", "Not connected", { error: "access_denied" });
  await expect(dialog().locator(".modal-title")).toHaveText("Not connected");
  await expect(dialog().getByRole("alert")).toHaveText("Mastodon: the sign-in was declined at the network.");
  await dialog().getByRole("button", { name: "Start again" }).click();
  await expect(dialog().locator(".modal-title")).toHaveText("Add a Mastodon account");
  // The app registered for this server is used again.
  await field("Server address").fill(`https://${MASTO}`);
  await signIn("Sign in at Mastodon", "Connected", { code: `erin:${RUN}` });
  await expect(card(`@erin@${MASTO}`).locator(".acct-status")).toHaveText("Connected");
  expect((await signIns()).filter(x => x.host === MASTO && x.path === "/api/v1/apps")).toHaveLength(1);
});

test("what a sign-in needs is checked before a tab opens; an address that isn't a Mastodon server is marked", async () => {
  await startAdding("Mastodon", "Sign in at your server");
  const err = dialog().locator('.fld[data-key="instance_url"] .fld-err');
  await clearLog();
  await dialog().getByRole("button", { name: "Sign in at Mastodon" }).click();
  await expect(err).toHaveText("Needed to sign in.");
  await expect(field("Server address")).toBeFocused();
  await field("Server address").fill("mastodon.example");
  await dialog().getByRole("button", { name: "Sign in at Mastodon" }).click();
  await expect(err).toHaveText("Enter a full address, such as https://example.com.");
  expect(await log()).toEqual([]);

  // Answering, but not as a Mastodon server: its tab closes again.
  expectedErrors = [/status of 502 \(Bad Gateway\)/];
  await field("Server address").fill(`https://none-${RUN}.e2e.invalid`);
  const tab = context.waitForEvent("page");
  await dialog().getByRole("button", { name: "Sign in at Mastodon" }).click();
  const t = await tab;
  await expect(dialog().locator(".acct-dlg-status")).toHaveText("No Mastodon server answered as expected at that address: check it.");
  await expect(err).toHaveText("No Mastodon server answered as expected here.");
  await expect(field("Server address")).toBeFocused();
  await expect.poll(() => t.isClosed()).toBe(true);
  expectedErrors = [];
  await page.keyboard.press("Escape");
  await expect(dialog()).toHaveCount(0);
});

test("a browser that blocks the new tab: the sign-in opens from a link instead", async () => {
  await page.evaluate(() => { (window as any).__e2eBlockPopups = true; });
  try {
    await startAdding("Mastodon", "Sign in at your server");
    await field("Server address").fill(`https://${MASTO}`);
    await dialog().getByRole("button", { name: "Sign in at Mastodon" }).click();
    await expect(dialog().locator(".acct-dlg-lead")).toHaveText(
      "Your browser didn't open a new tab: open the sign-in at Mastodon, allow Publisher, then come back here.");
    const link = dialog().getByRole("link", { name: "Open the sign-in at Mastodon" });
    await expect(link).toBeFocused();
    await expect(link).toHaveAccessibleDescription(/^Your browser didn't open a new tab/);
    answer = { code: `frank:${RUN}` };
    const tab = context.waitForEvent("page");
    await link.click();
    const t = await tab;
    await expect(t.getByRole("heading", { level: 1 })).toHaveText("Connected");
    await t.close();
    await expect(dialog()).toHaveCount(0);
    await expect(card(`@frank@${MASTO}`).locator(".acct-status")).toHaveText("Connected");
  } finally {
    await page.evaluate(() => { (window as any).__e2eBlockPopups = false; });
  }
});

test("the dialog closed before the end: finished in its tab after all, the account shows on the way back", async () => {
  await startAdding("Mastodon", "Sign in at your server");
  await field("Server address").fill(`https://${MASTO}`);
  const t = await holdSignIn("Sign in at Mastodon");
  await expect(dialog().locator(".modal-title")).toHaveText("Sign in at Mastodon");
  await page.keyboard.press("Escape"); // as Close, and ✕
  await expect(dialog()).toHaveCount(0);
  answer = { code: `heidi:${RUN}` };
  await t.goto(t.url());
  await expect(t.getByRole("heading", { level: 1 })).toHaveText("Connected");
  await t.close();
  await expect(card(`@heidi@${MASTO}`)).toHaveCount(0);
  // Back on this page (headless pages get no focus of their own).
  await page.evaluate(() => window.dispatchEvent(new Event("focus")));
  await expect(card(`@heidi@${MASTO}`).locator(".acct-status")).toHaveText("Connected");
  await expect(page.locator(".toast").last()).toHaveText(`Connected Mastodon @heidi@${MASTO}`);
});

test("finished in a browser that isn't signed in to Publisher: the account waits for this tab to confirm it", async ({ browser }) => {
  await startAdding("Mastodon", "Sign in at your server");
  await field("Server address").fill(`https://${MASTO}`);
  await dialog().getByLabel(/^Label/).fill("From the phone");
  const t = await holdSignIn("Sign in at Mastodon");
  // The sign-in link, opened in another browser (a phone, say).
  const other = await browser.newContext();
  try {
    await other.route(/^https:\/\/[a-z0-9.-]+\.e2e\.invalid\/oauth\/authorize\?/, signInPage);
    answer = { code: `grace:${RUN}` };
    const p = await other.newPage();
    await p.goto(t.url());
    await expect(p.getByRole("heading", { level: 1 })).toHaveText("Almost there");
    await expect(p.locator("body")).toContainText("This browser isn't signed in to Publisher: confirm the account in Publisher, in the tab you started from.");
  } finally {
    await other.close();
  }
  expect(await accountBy(`@grace@${MASTO}`)).toBeUndefined();

  // This tab asks, by who it is.
  await expect(dialog().locator(".modal-title")).toHaveText("Confirm the account");
  await expect(dialog().locator(".acct-cand")).toContainText("grace");
  const add = dialog().getByRole("button", { name: "Add it" });
  await expect(add).toBeFocused();
  await add.click();
  await expect(dialog()).toHaveCount(0);
  await expect(card(`@grace@${MASTO}`).locator(".acct-status")).toHaveText("Connected");
  expect((await accountBy(`@grace@${MASTO}`)).label).toBe("From the phone");
  await t.close();
});

test("several accounts offered: the owner picks one", async () => {
  // No fake network offers several: the flow's answers are this test's.
  const carol = await accountBy(`@carol@${MASTO}`);
  let chosen: unknown = null;
  await page.route("**/v1/connect-flows/*", r => r.request().method() === "GET"
    ? r.fulfill({ json: { id: "flw_x", type: "mastodon", status: "choose",
      candidates: [{ handle: "@one@x.example", display_name: "One" }, { handle: "@two@x.example" }] } })
    : r.fallback());
  await page.route("**/v1/connect-flows/*:choose", r => {
    chosen = r.request().postDataJSON();
    return r.fulfill({ json: { id: "flw_x", type: "mastodon", status: "completed", account: carol.id } });
  });
  try {
    await startAdding("Mastodon", "Sign in at your server");
    await field("Server address").fill(`https://${MASTO}`);
    const t = await holdSignIn("Sign in at Mastodon");
    await expect(dialog().locator(".modal-title")).toHaveText("Which Mastodon account?");
    const radios = dialog().getByRole("radio");
    await expect(radios).toHaveCount(2);
    await expect(radios.first()).toBeFocused();
    await expect(dialog().getByRole("group", { name: "Mastodon offered these accounts" })).toContainText("One @one@x.example");
    await dialog().getByLabel("@two@x.example").check();
    await dialog().getByRole("button", { name: "Add this one" }).click();
    await expect(dialog()).toHaveCount(0);
    expect(chosen).toEqual({ candidate: 1 });
    await t.close();
  } finally {
    await page.unroute("**/v1/connect-flows/*:choose");
    await page.unroute("**/v1/connect-flows/*");
  }
});

test("signing in again: the same account renews it; another account is refused", async () => {
  const c = card(`@carol@${MASTO}`);
  // Its settings: saving keeps it signed in; a new server address needs
  // signing in again there.
  await c.getByRole("button", { name: /^Settings of / }).click();
  await expect(dialog().getByRole("button", { name: "Sign in again" })).toBeFocused();
  await field("Server address").fill(`https://elsewhere-${RUN}.e2e.invalid`);
  await dialog().getByRole("button", { name: "Save" }).click();
  await expect(dialog().locator(".acct-dlg-status")).toHaveText("A new server address needs signing in again there: use Sign in again above.");
  await expect(dialog().getByRole("button", { name: "Sign in again" })).toBeFocused();
  await field("Server address").fill(`https://${MASTO}`);
  await dialog().getByRole("button", { name: "Save" }).click();
  await expect(dialog()).toHaveCount(0);
  await expect(page.locator(".toast").last()).toHaveText("Nothing changed");
  await c.getByRole("button", { name: /^Settings of / }).click();
  await expect(dialog().getByRole("button", { name: "Sign in again" })).toBeFocused();
  answer = { code: `carol:${RUN}-again` };
  let tab = context.waitForEvent("page");
  await dialog().getByRole("button", { name: "Sign in again" }).click();
  let t = await tab;
  await expect(t.getByRole("heading", { level: 1 })).toHaveText("Connected");
  await t.close();
  await expect(dialog()).toHaveCount(0);
  await expect(page.locator(".toast").last()).toContainText(`@carol@${MASTO} again`);

  // Shown as needing it (its token was refused, say): Reconnect signs in
  // again; another account than this one is refused.
  await page.route("**/v1/accounts?*", async r => {
    const res = await r.fetch();
    const body = await res.json();
    for (const a of body.data) if (a.handle === `@carol@${MASTO}`) { a.status = "needs_reconnect"; a.status_reason = "sign in again"; }
    await r.fulfill({ response: res, json: body });
  });
  await page.locator(".set-nav").getByRole("button", { name: "Composing" }).click();
  await page.locator(".set-nav").getByRole("button", { name: "Accounts" }).click();
  await expect(c.locator(".acct-status")).toHaveText("Needs reconnecting");
  answer = { code: `mallory:${RUN}` };
  tab = context.waitForEvent("page");
  await c.getByRole("button", { name: /^Reconnect / }).click();
  t = await tab;
  await expect(t.getByRole("heading", { level: 1 })).toHaveText("Not connected");
  await t.close();
  await expect(dialog().getByRole("alert")).toHaveText("Mastodon: the network signed in another account than the one being reconnected.");
  await dialog().locator(".confirm-actions").getByRole("button", { name: "Close" }).click();
  await expect(dialog()).toHaveCount(0);
  await page.unroute("**/v1/accounts?*");
  const acct = await accountBy(`@carol@${MASTO}`);
  expect(acct).toBeTruthy();
  expect(await accountBy(`@mallory@${MASTO}`)).toBeUndefined();
});

test("Threads: the server's Meta app first, in Settings, Integrations; then sign in at Threads", async () => {
  const user = `th${RUN}`;
  await startAdding("Threads", "Sign in at Threads");
  const note = dialog().locator(".set-note");
  await expect(note).toContainText("Unavailable: this needs the server's Threads app first (Settings, Integrations).");
  await expect(dialog().getByRole("button", { name: "Sign in at Threads" })).toBeDisabled();
  await expect(note.getByRole("button", { name: "Open Settings, Integrations" })).toBeFocused();
  await note.getByRole("button", { name: "Open Settings, Integrations" }).click();
  const sect = page.locator("#set-integrations .set-sect", { hasText: "Threads app" });
  await expect(sect).toBeFocused();
  await expect(sect.locator("code", { hasText: "/oauth/callback/threads" })).toHaveText(`${CONNECT_BASE}/oauth/callback/threads`);
  await sect.getByLabel(/^Threads app ID/).fill("1234567890");
  await sect.getByLabel(/^Threads app secret/).fill("app-s3cret");
  await sect.getByRole("button", { name: "Save", exact: true }).click();
  await confirmWithPasskey();
  await expect(sect.locator(".set-status")).toHaveText("Saved.");

  await openAccounts();
  await startAdding("Threads", "Sign in at Threads");
  await expect(dialog().locator(".set-note")).toHaveCount(0);
  await signIn("Sign in at Threads", "Connected", { code: `${user}:${RUN}` });
  await expect(card(user).locator(".acct-status")).toHaveText("Connected");
  const a = asked.at(-1)!;
  expect(a.host).toBe("threads.e2e.invalid");
  expect(a.params.get("client_id")).toBe("1234567890");
  expect(a.params.get("scope")).toBe("threads_basic,threads_content_publish,threads_manage_replies");
  expect(a.params.get("redirect_uri")).toBe(`${CONNECT_BASE}/oauth/callback/threads`);
  expect(a.params.has("client_secret")).toBe(false);
  const calls = (await signIns()).filter(x => x.host === "graph.e2e.invalid").slice(-2);
  expect(calls.map(x => x.path)).toEqual(["/oauth/access_token", "/access_token"]);
  expect(calls[0].values.client_secret).toEqual(["app-s3cret"]);
  expect(calls[0].values.redirect_uri).toEqual([`${CONNECT_BASE}/oauth/callback/threads`]);
  expect(calls[1].values.access_token).toEqual([`${user}:${RUN}~short`]);
  // (The long-lived token's expiry, kept for the refresher, is the
  // driver's own state: its Go tests check it.)
  const acct = await accountBy(user);
  expect(acct.auth_method).toBe("oauth");
  expect(JSON.stringify(acct)).not.toContain(`${user}:${RUN}`);
  const view = await (await page.request.get("/v1/instance/settings")).text();
  expect(view).not.toContain("app-s3cret");

  // Its settings save without signing in again.
  await card(user).getByRole("button", { name: /^Settings of / }).click();
  await dialog().locator(".fld-adv summary").click();
  await dialog().getByLabel(/^Threads user id/).fill("17841400000");
  await dialog().getByRole("button", { name: "Save" }).click();
  await expect(dialog()).toHaveCount(0);
  await expect(page.locator(".toast").last()).toHaveText("Saved");
  const saved = await accountBy(user);
  expect(saved.auth_method).toBe("oauth");
  expect(saved.config.user_id).toBe("17841400000");
});
