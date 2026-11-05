import fs from "node:fs";
import path from "node:path";
import { test, expect, marker } from "./fixtures";

// The /v1 API end to end: upload an image, post it everywhere, wait for the
// outcome. The harness runs without OIDC, so the reverse proxy is the gate
// and every request has every scope.
test("posts through /v1 with media and waits for the outcome", async ({ request, fakes }) => {
  const m = marker("v1");
  const accounts = (await (await request.get("/v1/accounts")).json()).data as { id: string; type: string; enabled: boolean }[];
  // The connected account of each type: the first listed (the harness also
  // seeds a second Mastodon account, which can't post).
  const enabled = accounts.filter((a, i) => a.enabled && accounts.findIndex(b => b.enabled && b.type === a.type) === i);
  expect(enabled.map(a => a.type).sort()).toEqual(["bluesky", "mastodon", "nostr", "threads"]);

  const up = await request.post("/v1/media?alt=A%20blue%20gradient", {
    headers: { "Content-Type": "image/png" },
    data: fs.readFileSync(path.join(__dirname, "assets", "gradient.png")),
  });
  expect(up.status()).toBe(201);
  const media = await up.json();
  expect(media.status).toBe("ready");

  const body = {
    content: { text: `Posted through the API ${m}`, media: [{ id: media.id }] },
    targets: enabled.map(a => (a.type === "mastodon" ? { account: a.id, options: { visibility: "unlisted" } } : { account: a.id })),
  };
  const res = await request.post("/v1/posts", {
    headers: { "Content-Type": "application/json", "Prefer": "wait=30", "Idempotency-Key": `"${m}"` },
    data: body,
  });
  expect(res.status()).toBe(201);
  const post = await res.json();
  expect(post.status).toBe("published");
  expect(post.targets).toHaveLength(4);

  const calls = await fakes.waitForCalls(m, 4);
  for (const c of calls) {
    expect(c.images?.[0]?.alt).toBe("A blue gradient");
  }

  // The same key and body: the first answer again, and no second post.
  const again = await request.post("/v1/posts", {
    headers: { "Content-Type": "application/json", "Idempotency-Key": `"${m}"` },
    data: body,
  });
  expect(again.headers()["idempotent-replayed"]).toBe("true");
  expect((await again.json()).id).toBe(post.id);
  expect((await fakes.calls()).filter(c => c.text.includes(m))).toHaveLength(4);
});
