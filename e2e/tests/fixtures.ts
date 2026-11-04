import { test as base, expect, type APIRequestContext, type Page } from "@playwright/test";

export type Call = {
  platform: "nostr" | "mastodon" | "bluesky" | "threads";
  action: "post" | "reply" | "repost" | "quote";
  text: string;
  images?: { mime: string; alt?: string; size: number }[];
  reply_to?: string;
  remote_id: string;
};

/** Test-control API of the e2e harness's fake platforms. */
export class Fakes {
  constructor(private readonly request: APIRequestContext) {}
  async reset() { expect((await this.request.post("/__e2e/reset")).ok()).toBeTruthy(); }
  async down(platform: string) { expect((await this.request.post(`/__e2e/down?platform=${platform}`)).ok()).toBeTruthy(); }
  async up(platform: string) { expect((await this.request.post(`/__e2e/up?platform=${platform}`)).ok()).toBeTruthy(); }
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

/** Open the app and wait until boot (config fetch + compose init) is done. */
export async function openApp(page: Page) {
  await page.goto("/");
  await expect(page.locator("#chips .target-row")).toHaveCount(4);
}

export async function switchTab(page: Page, view: "compose" | "history" | "tools" | "verify" | "interact") {
  await page.locator(`.tab[data-view="${view}"]`).click();
  await expect(page.locator(`section#${view}`)).toBeVisible();
}
