import { test as base, expect as baseExpect, request, type Page, type BrowserContext, type Locator } from "@playwright/test";
import path from "node:path";
import { test, expect, openApp } from "./fixtures";

// Settings → Composing, Media, Publishing, Integrations and System, and
// account sets in the composer, against the local sign-in harness (signed
// in with a passkey; its environment locks SCHEDULE_GRACE): a locked
// setting copied into the app, languages, account sets and the set new
// posts start with, an account's defaults in a new post, media kept on the
// server's disk (served with their own policy, and checked reachable at
// the public address before being served publicly), a feed token made and
// shown once, and what the server started with. What a spec changes is
// put back after, so a reused server starts the same.

const PORT = Number(process.env.E2E_LOCAL_PORT || Number(process.env.E2E_PORT || 4173) + 1);
const BASE = `http://localhost:${PORT}`;
const RUN = "r" + Date.now().toString(36);

base.describe("settings, signed in", () => {
  base.describe.configure({ mode: "serial", retries: 0 });

  let context: BrowserContext;
  let page: Page;
  const errors: string[] = [];

  base.beforeAll(async ({ browser }) => {
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
    baseExpect((await page.request.post("/__e2e/reset-login")).status()).toBe(204);
    await page.goto(await (await page.request.post("/__e2e/setup-url")).text());
    await page.getByRole("button", { name: "Create a passkey" }).click();
    await baseExpect(page).toHaveURL(BASE + "/");
    await baseExpect(page.locator("#chips .target-row").first()).toBeVisible();
  });

  // api calls /v1 from the page (its session, and the Origin the CSRF
  // guard wants).
  const api = (method: string, url: string, body?: unknown) => page.evaluate(async ({ method, url, body }) => {
    const r = await fetch(url, { method, credentials: "same-origin",
      headers: body === undefined ? {} : { "Content-Type": "application/json" },
      body: body === undefined ? undefined : JSON.stringify(body) });
    return { status: r.status, body: r.status === 204 ? null : await r.json().catch(() => null), headers: Object.fromEntries(r.headers) };
  }, { method, url, body });

  base.afterAll(async () => {
    // What the specs changed, put back.
    const sets = await api("GET", "/v1/account-sets");
    for (const s of (sets.body && sets.body.data) || []) if (String(s.name).includes(RUN)) await api("DELETE", "/v1/account-sets/" + s.id);
    await api("PATCH", "/v1/instance/settings", { media: { store: null, public: null, blossom_url: null } });
    await api("PATCH", "/v1/settings", { composing: { languages: ["en", "fr"] }, feed: { token: null }, publishing: { max_attempts: null },
      alerts: { webhook_user: null } });
    const accts = await api("GET", "/v1/accounts?limit=100");
    for (const a of (accts.body && accts.body.data) || []) if (a.defaults && Object.keys(a.defaults).length) await api("PATCH", "/v1/accounts/" + a.id, { defaults: {} });
    baseExpect(errors).toEqual([]);
    await context.close();
  });

  async function section(name: string) {
    await page.locator('.tab[data-view="settings"]').click();
    await page.locator(".set-nav").getByRole("button", { name, exact: true }).click();
    const panel = page.locator("#set-" + name.toLowerCase());
    await baseExpect(panel).toBeVisible();
    await baseExpect(panel.locator(".set-reading")).toHaveCount(0);
    return panel;
  }

  // A form's status line; a section of a panel by its heading.
  const status = (where: Locator) => where.locator("form > .set-status");
  const sect = (panel: Locator, title: string) =>
    panel.locator("section.set-sect", { has: page.getByRole("heading", { name: title, exact: true }) });

  base("a setting the environment sets is locked, and can be copied into the app", async () => {
    const panel = await section("Publishing");
    const grace = panel.getByLabel("Late scheduled posts", { exact: true });
    await baseExpect(grace).toBeDisabled();
    await baseExpect(grace).toHaveValue("3h");
    const row = panel.locator('.fld[data-key="publishing.schedule_grace"]');
    const src = row.locator(".fld-src");
    await baseExpect(src).toContainText("Set by SCHEDULE_GRACE in the server's environment");
    // A reused server has it copied already (a value saved under the
    // environment's can't be cleared while the variable is set).
    if (!/also saved in the app/.test((await src.textContent()) || "")) {
      await row.getByRole("button", { name: "Copy late scheduled posts into the app", exact: true }).click();
      await baseExpect(status(panel)).toContainText("You can now remove SCHEDULE_GRACE from the server's environment");
    }
    // Copied, it says so for good, with nothing more to copy.
    await baseExpect(src).toContainText("and also saved in the app: remove SCHEDULE_GRACE from the environment to use what the app keeps.");
    await baseExpect(row.getByRole("button", { name: /into the app/ })).toHaveCount(0);
    await baseExpect(grace).toBeDisabled();
    // A plain one is saved, and says so.
    const tries = panel.getByLabel("Tries before giving up", { exact: true });
    await tries.fill("4");
    await panel.getByRole("button", { name: "Save" }).click();
    await baseExpect(status(panel)).toHaveText("Saved.");
    await baseExpect(panel.locator('.fld[data-key="publishing.max_attempts"] .fld-src')).toContainText("Saved in the app");
    // A value the server refuses says why, on its field.
    await tries.fill("99");
    await panel.getByRole("button", { name: "Save" }).click();
    await baseExpect(panel.locator('.fld[data-key="publishing.max_attempts"] .fld-err')).not.toBeEmpty();
    await tries.fill("6");
    await panel.getByRole("button", { name: "Save" }).click();
    await baseExpect(status(panel)).toHaveText("Saved.");
  });

  // The Mastodon account's card in the composer, opened.
  async function mastodonCard() {
    const accts = await api("GET", "/v1/accounts?limit=100");
    const id = accts.body.data.find((a: { type: string; handle?: string }) => a.type === "mastodon" && a.handle === "@me@one.e2e.invalid").id;
    await page.locator('.tab[data-view="compose"]').click();
    const card = page.locator(`#cards .ocard[data-account="${id}"]`);
    const body = card.locator(".ocard-body");
    if (await body.isHidden()) await card.locator(".ocard-summary").click();
    await baseExpect(body).toBeVisible();
    return { id, card };
  }

  base("languages saved in the app are the composer's at once", async () => {
    const form = sect(await section("Composing"), "Languages");
    const langs = form.getByLabel(/^Languages you write in/);
    await langs.fill("de\nen");
    await form.getByRole("button", { name: "Save" }).click();
    await baseExpect(status(form)).toHaveText("Saved.");
    const inst = await api("GET", "/v1/instance");
    baseExpect(inst.body.user_languages).toEqual(["de", "en"]);
    // A new post nobody has touched starts from them, without a reload.
    let { card } = await mastodonCard();
    const lang = card.getByLabel("lang (ISO 639-1)");
    await baseExpect(lang).toHaveValue("de");
    await baseExpect(lang.locator("option")).toHaveText(["de", "en"]);

    await section("Composing");
    await langs.fill("en\nfr");
    await form.getByRole("button", { name: "Save" }).click();
    await baseExpect(status(form)).toHaveText("Saved.");
    ({ card } = await mastodonCard());
    await baseExpect(card.getByLabel("lang (ISO 639-1)")).toHaveValue("en");
  });

  base("an account set picks its accounts in the composer, and the default one starts new posts", async () => {
    const panel = await section("Composing");
    await panel.getByRole("button", { name: "New set" }).click();
    const dlg = page.locator(".acct-dlg");
    await dlg.getByLabel("Name").fill("Work " + RUN);
    await dlg.getByLabel("Mastodon @me@one.e2e.invalid", { exact: true }).check();
    await dlg.getByLabel("Bluesky me.bsky.e2e.invalid", { exact: true }).check();
    await dlg.getByLabel("Start new posts with this set").check();
    await dlg.getByRole("button", { name: "Add" }).click();
    await baseExpect(dlg).toHaveCount(0);
    const item = panel.locator(".set-item", { hasText: "Work " + RUN });
    await baseExpect(item).toContainText("new posts");
    // A second set, and a name taken whatever its case.
    await panel.getByRole("button", { name: "New set" }).click();
    await dlg.getByLabel("Name").fill("WORK " + RUN.toUpperCase());
    // Threads alone, or beside one an account spec added.
    await dlg.getByLabel(/^Threads( me_e2e)?$/).check();
    await dlg.getByRole("button", { name: "Add" }).click();
    await baseExpect(dlg.locator(".fld-err").first()).not.toBeEmpty();
    await dlg.getByLabel("Name").fill("Threads only " + RUN);
    await dlg.getByRole("button", { name: "Add" }).click();
    await baseExpect(dlg).toHaveCount(0);

    // A new post nobody has touched takes the default set at once, and
    // starts with it after a reload; the other set picks its own.
    const on = page.locator('#chips .target-row[aria-checked="true"]');
    await page.locator('.tab[data-view="compose"]').click();
    await baseExpect(on).toHaveCount(2);
    await baseExpect(page.locator("#chips .ts-set", { hasText: "Work " + RUN })).toHaveAttribute("aria-pressed", "true");
    await page.goto("/");
    const rows = page.locator("#chips .target-row");
    await baseExpect(rows.first()).toBeVisible();
    await baseExpect(on).toHaveCount(2);
    await baseExpect(page.locator("#chips .ts-set", { hasText: "Work " + RUN })).toHaveAttribute("aria-pressed", "true");
    await page.locator("#chips .ts-set", { hasText: "Threads only " + RUN }).click();
    await baseExpect(on).toHaveCount(1);
    await baseExpect(on).toHaveAttribute("data-type", "threads");
    await baseExpect(page.locator("#chips .ts-set", { hasText: "Threads only " + RUN })).toBeFocused();
  });

  base("an account's defaults start a new post on it", async () => {
    const panel = await section("Composing");
    const change = page.getByRole("button", { name: "Change Mastodon @me@one.e2e.invalid's defaults", exact: true });
    const item = panel.locator(".set-item").filter({ has: change });
    await item.getByRole("button", { name: "Change" }).click();
    const dlg = page.locator(".acct-dlg");
    await dlg.getByLabel("Content warning (optional)").fill("spoilers " + RUN);
    await dlg.getByLabel("Visibility").selectOption("unlisted");
    await dlg.getByRole("button", { name: "Save" }).click();
    await baseExpect(dlg).toHaveCount(0);
    await baseExpect(item).toContainText("content warning “spoilers " + RUN + "”");
    await baseExpect(item).toContainText("visibility unlisted");

    // The post on screen (nobody touched it) takes them without a reload.
    await page.locator('.tab[data-view="compose"]').click();
    await page.locator("#chips .ts-set", { hasText: "Work " + RUN }).click();
    const { id, card } = await mastodonCard();
    await baseExpect(card.getByLabel("CW")).toHaveValue("spoilers " + RUN);
    await baseExpect(card.getByLabel("visibility")).toHaveValue("unlisted");
    const previewOf = (want: (t: { options?: Record<string, unknown>; content?: Record<string, unknown> }) => boolean) =>
      page.waitForRequest(r => {
        if (!r.url().endsWith("/v1/posts:preview") || r.method() !== "POST") return false;
        const b = JSON.parse(r.postData() || "{}");
        const t = (b.targets || []).find((t: { account: string }) => t.account === id);
        return !!t && (r.postData() || "").includes("hello " + RUN) && want(t);
      });
    let preview = previewOf(() => true);
    await page.locator("#master").fill("hello " + RUN);
    const masto = JSON.parse((await preview).postData() || "{}").targets.find((t: { account: string }) => t.account === id);
    baseExpect(masto.options.visibility).toBe("unlisted");
    baseExpect(masto.content.content_warning).toBe("spoilers " + RUN);

    // Cleared on the post, a default stays cleared: the post sends the
    // empty value, so the server's default doesn't fill it in again.
    preview = previewOf(t => !!t.options && t.options.visibility === "" && !!t.content && t.content.content_warning === "");
    await card.getByLabel("CW").fill("");
    await card.getByLabel("visibility").selectOption("");
    const res = await (await (await preview).response())!.json();
    const applied = res.targets.find((t: { account: string }) => t.account === id).defaults_applied;
    baseExpect(applied.content_warning).toBeUndefined();
    baseExpect((applied.options || {}).visibility).toBeUndefined();
  });

  base("media kept on this server's disk are served with their own policy, and publicly only once reachable", async () => {
    const panel = await section("Media");
    await panel.getByLabel("Where media go").selectOption("local");
    await baseExpect(panel.getByLabel("Serve media publicly")).toBeVisible();
    await baseExpect(panel.getByLabel("Blossom server", { exact: true })).toBeHidden();
    await panel.getByRole("button", { name: "Save" }).click();
    await baseExpect(status(panel)).toHaveText("Saved.");
    await panel.getByRole("button", { name: "Test the media store" }).click();
    await baseExpect(status(panel)).toHaveText("The media store worked.");
    // Public: the self-test fetches a probe at PUBLIC_URL/media/ first.
    // Refused (faked here: a sign-in in front answered), the field says
    // why and the setting stays off.
    await panel.getByLabel("Serve media publicly").check();
    await baseExpect(panel.locator(".set-note.warn")).toContainText("IP address");
    const why = "PUBLIC_URL/media/ answered with a redirect (302) to https://sso.example/login: let /media/ through without sign-in";
    await page.route("**/v1/instance/settings", r => r.request().method() !== "PATCH" ? r.continue() :
      r.fulfill({ status: 422, contentType: "application/problem+json", body: JSON.stringify({
        type: "about:blank", title: "Validation failed", status: 422, code: "validation_failed",
        errors: [{ pointer: "/media/public", code: "not_reachable", detail: why }] }) }));
    await panel.getByRole("button", { name: "Save" }).click();
    await baseExpect(panel.locator('.fld[data-key="media.public"] .fld-err')).toHaveText(why);
    await page.unroute("**/v1/instance/settings");
    baseExpect((await api("GET", "/v1/instance/settings")).body.media.public.value).toBe(false);
    await panel.getByRole("button", { name: "Save" }).click();
    await baseExpect(status(panel)).toHaveText("Saved.");
    await baseExpect(panel.getByLabel("Serve media publicly")).toBeChecked();

    // An image attached now is kept here and served without sign-in.
    await page.locator('.tab[data-view="compose"]').click();
    await baseExpect(page.locator("#addimg")).toBeVisible();
    const uploaded = page.waitForResponse(r => r.url().endsWith("/v1/media") && r.request().method() === "POST");
    await page.locator("#imgfile").setInputFiles(path.join(__dirname, "assets", "gradient.png"));
    const med = await (await uploaded).json();
    baseExpect(med.url).toMatch(new RegExp("^" + BASE + "/media/[0-9a-f]{64}\\.(png|jpg|webp)$"));
    await baseExpect(page.locator("#images img")).toHaveCount(1);
    const anon = await request.newContext();
    const r = await anon.get(med.url);
    baseExpect(r.status()).toBe(200);
    baseExpect(r.headers()["content-type"]).toMatch(/^image\/(png|jpeg|webp)$/);
    baseExpect(r.headers()["content-security-policy"]).toBe("default-src 'none'; sandbox");
    baseExpect(r.headers()["x-content-type-options"]).toBe("nosniff");
    baseExpect((await anon.get(BASE + "/media/" + "0".repeat(64) + ".png")).status()).toBe(404);
    baseExpect((await anon.get(BASE + "/media/x/y")).status()).toBe(404);
    await anon.dispose();
  });

  const confirmIt = async () => {
    const dlg = page.getByRole("dialog", { name: "Confirm it's you" });
    await dlg.getByRole("button", { name: "Use your passkey" }).click();
    await baseExpect(dlg).toBeHidden();
  };

  base("a Blossom server needs its address; making its key keeps what was typed", async () => {
    const panel = await section("Media");
    const where = panel.getByLabel("Where media go");
    const url = panel.getByLabel("Blossom server", { exact: true });
    await where.selectOption("blossom");
    await baseExpect(url).toBeVisible();
    await url.fill("");
    await panel.getByRole("button", { name: "Save" }).click();
    await baseExpect(panel.locator('.fld[data-key="media.blossom_url"] .fld-err')).not.toBeEmpty();
    baseExpect((await api("GET", "/v1/instance/settings")).body.media.store.value).toBe("local");

    // A key made (it asks who you are first) leaves the unsaved choice and
    // address as they were typed.
    await url.fill("https://blossom.e2e.invalid");
    await panel.getByRole("button", { name: "Make a Blossom signing key", exact: true }).click();
    await confirmIt();
    await baseExpect(panel.getByLabel("Its public key", { exact: true })).toHaveValue(/^npub1/);
    await baseExpect(where).toHaveValue("blossom");
    await baseExpect(url).toHaveValue("https://blossom.e2e.invalid");
    await baseExpect(panel.locator('.fld[data-key="media.blossom_key"] .fld-state')).toContainText("Set");
    await panel.getByRole("button", { name: "Save" }).click();
    await baseExpect(status(panel)).toHaveText("Saved.");
    const saved = (await api("GET", "/v1/instance/settings")).body.media;
    baseExpect([saved.store.value, saved.blossom_url.value]).toEqual(["blossom", "https://blossom.e2e.invalid"]);

    // Cleared again (it asks again: each confirmation is good once).
    await panel.getByRole("button", { name: "Clear Blossom signing key", exact: true }).click();
    await page.locator(".modal").getByRole("button", { name: "Clear", exact: true }).click();
    await confirmIt();
    await baseExpect(panel.locator('.fld[data-key="media.blossom_key"] .fld-state')).toContainText("Not set.");
    await baseExpect(panel.getByLabel("Its public key", { exact: true })).toHaveCount(0);
    // The keyboard goes to the field, to type one in.
    await baseExpect(panel.getByRole("textbox", { name: /^Blossom signing key/ })).toBeFocused();
  });

  base("a feed token is made and shown once; what the server started with is listed", async () => {
    const panel = await section("Integrations");
    const row = panel.locator('.fld[data-key="feed.token"]');
    await row.getByRole("button", { name: "Make a public feed token", exact: true }).click();
    const shown = row.getByLabel("The new public feed token", { exact: true });
    await baseExpect(shown).toHaveValue(/^[A-Za-z0-9_-]{40,}$/);
    const token = await shown.inputValue();
    await baseExpect(row.locator(".fld-state")).toContainText("Set");
    // It stays until the owner says they copied it: another form saved
    // (what was typed and not saved in this one stays too), another
    // section shown.
    const hook = panel.getByRole("textbox", { name: "Feed webhook (optional)", exact: true });
    await hook.fill("https://hook.e2e.invalid/feed");
    const alerts = sect(panel, "Alerts");
    await alerts.getByLabel("Alert webhook user", { exact: true }).fill("bot-" + RUN);
    await alerts.getByRole("button", { name: "Save" }).click();
    await baseExpect(status(alerts)).toHaveText("Saved.");
    await baseExpect(shown).toHaveValue(token);
    await baseExpect(hook).toHaveValue("https://hook.e2e.invalid/feed");
    await section("System");
    await section("Integrations");
    await baseExpect(shown).toHaveValue(token);
    await hook.fill("");
    await row.getByRole("button", { name: "Done, I copied it" }).click();
    await baseExpect(shown).toHaveCount(0);
    await baseExpect(row.getByRole("button", { name: "Make a new public feed token", exact: true })).toBeFocused();
    // Read again, it isn't shown.
    await section("System");
    await section("Integrations");
    await baseExpect(row.locator(".set-note input")).toHaveCount(0);

    const sys = await section("System");
    await baseExpect(sys.locator(".sys-facts")).toContainText("Version");
    await baseExpect(sys.locator(".sys-env")).toContainText(String(PORT));
    const secret = sys.locator(".sys-env tr", { has: page.locator("code", { hasText: /^E2E_SECRET$/ }) });
    await baseExpect(secret).toContainText("set (not shown)");
    baseExpect(await page.content()).not.toContain("e2e-secret-not-for-the-page");
    baseExpect(JSON.stringify((await api("GET", "/v1/instance/system")).body)).not.toContain("e2e-secret-not-for-the-page");
  });
});

// Behind the reverse proxy alone, the server's settings aren't shown.
test("behind the reverse proxy alone, the server's settings say to set up sign-in @mobile", async ({ page, consoleErrors }) => {
  await openApp(page);
  await page.locator('.tab[data-view="settings"]').click();
  await page.locator(".set-nav").getByRole("button", { name: "Media", exact: true }).click();
  await expect(page.locator("#set-media .set-note.warn")).toContainText("set up sign-in first");
  // The app's secrets show locked, saying why, rather than refused on Save.
  await page.locator(".set-nav").getByRole("button", { name: "Integrations", exact: true }).click();
  const token = page.locator('#set-integrations .fld[data-key="feed.token"]');
  await expect(token.locator(".fld-state")).toContainText("Behind the reverse proxy alone, secrets can't be set in the app: set up sign-in first.");
  await expect(token.getByRole("textbox", { name: /^Public feed token/ })).toBeDisabled();
  await expect(token.getByRole("button", { name: /^Make a/ })).toHaveCount(0);
  await page.locator(".set-nav").getByRole("button", { name: "Publishing", exact: true }).click();
  await expect(page.locator("#set-publishing").getByLabel("Retry failed posts by themselves")).toBeVisible();
  // The server's settings answered 403 (sign_in_required), as they should.
  consoleErrors.splice(0, consoleErrors.length, ...consoleErrors.filter(e => !/status of 403/.test(e)));
});
