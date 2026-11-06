import { test as base, expect, type APIRequestContext, type Page } from "@playwright/test";

export type Call = {
  platform: "nostr" | "mastodon" | "bluesky" | "threads";
  /** The id of the account that sent it. */
  account: string;
  action: "post" | "reply" | "repost" | "quote";
  text: string;
  images?: { mime: string; alt?: string; size: number }[];
  reply_to?: string;
  remote_id: string;
};

/** The query naming a platform ("mastodon") or one account ("acc_…"). */
function who(platformOrAccount: string) {
  const key = platformOrAccount.startsWith("acc_") ? "account" : "platform";
  return `${key}=${encodeURIComponent(platformOrAccount)}`;
}

/**
 * Test-control API of the e2e harness's fake platforms. Each control takes a
 * platform (every account of it) or one account's id.
 */
export class Fakes {
  constructor(private readonly request: APIRequestContext) {}
  async reset() { expect((await this.request.post("/__e2e/reset")).ok()).toBeTruthy(); }
  async down(target: string) { expect((await this.request.post(`/__e2e/down?${who(target)}`)).ok()).toBeTruthy(); }
  async up(target: string) { expect((await this.request.post(`/__e2e/up?${who(target)}`)).ok()).toBeTruthy(); }
  /** It refuses posts (an `invalid` error) until up() or reset(). */
  async invalid(target: string) { expect((await this.request.post(`/__e2e/invalid?${who(target)}`)).ok()).toBeTruthy(); }
  /** It takes `after` more calls; the next one never returns. */
  async hold(target: string, after: number) { expect((await this.request.post(`/__e2e/hold?${who(target)}&after=${after}`)).ok()).toBeTruthy(); }
  /** Abandon the engine mid-send and start a new one on the same database, as a restart does. */
  async restart() { expect((await this.request.post("/__e2e/restart")).ok()).toBeTruthy(); }
  /** Calls each platform and each account (by id) received, failed ones included. */
  async attempts(): Promise<Record<string, number>> { return (await this.request.get("/__e2e/attempts")).json(); }
  async calls(): Promise<Call[]> { return (await this.request.get("/__e2e/calls")).json(); }
  /** Calls whose text contains marker, polled until at least `min` arrive. */
  async waitForCalls(marker: string, min: number): Promise<Call[]> {
    let got: Call[] = [];
    await expect.poll(async () => {
      got = (await this.calls()).filter(c => c.text.includes(marker));
      return got.length;
    }).toBeGreaterThanOrEqual(min);
    return got;
  }
}

export type Account = {
  id: string;
  type: string;
  label: string;
  status: "connected" | "disabled" | "not_configured" | "locked" | "needs_reconnect" | "deleted";
  status_reason?: string;
  enabled: boolean;
  default_selected: boolean;
  order: number;
};

/** Every account, oldest first (`order` is the app's order: networks in a fixed order, then oldest first). */
export async function listAccounts(request: APIRequestContext): Promise<Account[]> {
  const r = await request.get("/v1/accounts");
  expect(r.status()).toBe(200);
  return (await r.json()).data;
}

/** The harness's account with this label (see harnessAccounts in e2e/harness/main.go). */
export async function accountLabelled(request: APIRequestContext, label: string): Promise<Account> {
  const a = (await listAccounts(request)).find(x => x.label === label);
  expect(a, `an account labelled ${label}`).toBeTruthy();
  return a!;
}

/**
 * A Mastodon post as POST /v1/resolve answers it, looked up by (and to be
 * answered from) the harness's "Mastodon" account, for specs that answer
 * /v1/resolve themselves.
 */
export async function mastodonSource(request: APIRequestContext) {
  const account = (await accountLabelled(request, "Mastodon")).id;
  return {
    platform: "mastodon", url: "https://mastodon.example/@a/1", text: "source post",
    author: { name: "A", handle: "@a@mastodon.example" }, media: [], created_at: null, account,
    actions: { reply: { allowed: true }, repost: { allowed: true }, quote: { allowed: true } },
  };
}

/** A short unique marker so a test can find its own posts and calls. */
export function marker(label: string) {
  return `${label}-${Date.now().toString(36)}${Math.random().toString(36).slice(2, 6)}`;
}

/**
 * Every test gets a fresh fake-platform state and fails on any browser console
 * error or uncaught exception — which includes Content-Security-Policy
 * violations, so a change that sneaks in an inline script or a cross-origin
 * asset is caught here.
 */
export const test = base.extend<{ fakes: Fakes; consoleErrors: string[] }>({
  fakes: async ({ request }, use) => {
    const f = new Fakes(request);
    await f.reset();
    await use(f);
  },
  consoleErrors: [async ({ page }, use) => {
    const errors: string[] = [];
    page.on("console", m => { if (m.type() === "error") errors.push(m.text()); });
    page.on("pageerror", e => errors.push(String(e)));
    await use(errors);
    expect(errors, "browser console errors").toEqual([]);
  }, { auto: true }],
});

export { expect };

/** The composer's rows: one per harness account (harnessAccounts in e2e/harness/main.go). */
export const ACCOUNT_ROWS = 7;

/** Open the app and wait until boot (config fetch, accounts, compose init) is done. */
export async function openApp(page: Page) {
  await page.goto("/");
  await expect(page.locator("#chips .target-row")).toHaveCount(ACCOUNT_ROWS);
}

/**
 * The harness's account ids by label ("Mastodon (long posts)") and, for the
 * account a new post selects on each network, by network ("mastodon"): the
 * four that specs written for one account per network use.
 */
export async function harnessIds(request: APIRequestContext): Promise<Record<string, string>> {
  const all = await listAccounts(request);
  const out: Record<string, string> = {};
  for (const a of all) out[a.label] = a.id;
  for (const a of all) if (a.default_selected && a.status === "connected" && !(a.type in out)) out[a.type] = a.id;
  return out;
}

/** The composer's target row for the harness account with this label. */
export async function rowOf(page: Page, label: string) {
  const a = await accountLabelled(page.request, label);
  return page.locator(`#chips .target-row[data-account="${a.id}"]`);
}

/** The thread editor's tab for the harness account with this label ("shared" for the shared text). */
export async function tabOf(page: Page, label: string) {
  if (label === "shared") return page.locator(`#editor .ed-tab[data-tab="shared"]`);
  const a = await accountLabelled(page.request, label);
  return page.locator(`#editor .ed-tab[data-tab="${a.id}"]`);
}

export async function switchTab(page: Page, view: "compose" | "history" | "tools" | "verify" | "interact" | "tokens") {
  await page.locator(`.tab[data-view="${view}"]`).click();
  await expect(page.locator(`section#${view}`)).toBeVisible();
}
