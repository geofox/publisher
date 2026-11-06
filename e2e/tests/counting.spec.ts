import fs from "node:fs";
import path from "node:path";
import { test, expect, openApp } from "./fixtures";

// The web app's counter (internal/web/assets/count.js) against the cases the
// Go counter runs too (internal/destination counting_test.go), so the counts
// the composer shows while typing are the ones the server splits by.
const file = path.join(__dirname, "..", "..", "internal", "destination", "testdata", "counting.json");
const { cases } = JSON.parse(fs.readFileSync(file, "utf8")) as {
  cases: { name: string; counting: Record<string, unknown>; text: string; want: number }[];
};

test("the browser counts like the server", async ({ page }) => {
  expect(cases.length).toBeGreaterThan(5);
  await openApp(page);
  const got = await page.evaluate(async (cs) => {
    const { count } = await import("/count.js");
    return cs.map(c => count(c.counting, c.text));
  }, cases);
  cases.forEach((c, i) => expect(got[i], c.name).toBe(c.want));
});
