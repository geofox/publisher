# End-to-end tests

Playwright drives the real web app in Chromium against a test harness
(`e2e/harness`) that runs the **production** API routes, middleware,
dispatcher, threading, SQLite store (with real migrations, in a temp dir) and
embedded assets. Only the network edge is fake:

- the four destination accounts (Nostr, Mastodon, Bluesky, Threads) speak the
  real drivers' manifests, so splitting, media fitting and gating run as in
  production, but they record what they would have sent and return
  deterministic ids/URLs;
- a fake Blossom server, served on the same origin, stores uploads in memory.

Nothing leaves the machine: no relays, no platform APIs, no DNS.

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
  chain). `fakes.down("mastodon")` / `fakes.up(...)` simulate an outage.
- The database persists for the whole run, so tag your content with
  `marker("label")` and look for it, instead of assuming an empty History.
- Tests run one at a time (`workers: 1`) because they share the fakes.
- Add `@mobile` to a test title to also run it on a Pixel 7 viewport.

## Known gaps

- Video (needs ffmpeg) and the Interact/Verify flows (need live resolvers)
  aren't covered yet.
