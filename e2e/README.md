# End-to-end tests

Playwright drives the real web app in Chromium against a test harness
(`e2e/harness`) that runs the **production** API routes, middleware,
dispatcher, threading, SQLite store (with real migrations, in a temp dir) and
embedded assets. Only the network edge is fake:

- the destination accounts speak the real drivers' manifests, so splitting,
  media fitting and gating run as in production, but they record what they
  would have sent (and which account sent it) and return deterministic
  ids/URLs. The Bluesky, Mastodon and Nostr fakes also look up posts to
  reply to, quote or repost (see `ResolveSource` in `harness/fakes.go` for
  the URLs they accept); Threads has no lookup, as in production;
- a fake Blossom server, served on the same origin, stores uploads in memory.
- relay sync (scan, pull, push) reaches no relay: every relay is reported
  in step and nothing is copied.

Nothing leaves the machine: no relays, no platform APIs, no DNS.

The accounts are in `harnessAccounts` (`harness/main.go`), stored at
startup as an env-configured deployment stores its own:

| Label | Network | Status | Notes |
|---|---|---|---|
| Nostr, Mastodon, Bluesky, Threads | one each | connected | the composer's four rows |
| Mastodon (long posts) | Mastodon | connected | 1000-character limit |
| Bluesky (alt) | Bluesky | connected | not selected by default |
| Mastodon (old) | Mastodon | not configured | its variables "were removed" |

The composer shows one row per network (the first connected account) until
part A3; `tests/api-accounts.spec.ts` covers several accounts of a network
through `/v1`. A test that changes an account restores it in `afterEach`.

## Run

```bash
cd e2e
npm ci
npx playwright install chromium   # once
npx playwright test               # starts the harness itself (go run ./e2e/harness)
npx playwright test --ui          # interactive
```

Go (per `go.mod`) must be on `PATH`. The harness listens on
`127.0.0.1:4173` (`E2E_PORT` to change). Locally an already-running harness is
reused; in CI it is always started fresh.

## Writing tests

- Import `test`/`expect` from `tests/fixtures.ts`. Every test then:
  - starts with the fakes reset (no recorded calls, every platform up);
  - **fails on any browser console error**, including Content-Security-Policy
    violations — keep it that way.
- `fakes.calls()` returns what each platform received (text, images, reply
  chain, account). `fakes.down("mastodon")` / `fakes.up(...)` simulate an
  outage of every Mastodon account; pass an account id (`acc_…`) instead
  for one account. `listAccounts` and `accountLabelled` find accounts.
- The database persists for the whole run, so tag your content with
  `marker("label")` and look for it, instead of assuming an empty History.
- Tests run one at a time (`workers: 1`) because they share the fakes.
- Add `@mobile` to a test title to also run it on a Pixel 7 viewport.

## Known gaps

- Video (needs ffmpeg) and the Verify flow aren't covered yet. The Interact
  tab's tests still stub `/v1/resolve` in the browser; the API's lookups run
  against the fakes.
