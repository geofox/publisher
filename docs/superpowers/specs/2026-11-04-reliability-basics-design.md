# Reliability basics (roadmap step 1)

**Status:** Approved direction (roadmap step 1, 2026-11-04) — design for review
**Date:** 2026-11-04
**Area:** Reliability & ops — schema, health, tests, packaging, code layout

## Problem

Publisher works, but five things make it harder to run, change and share than
it should be before an open-source 1.0 and before the Destination-interface
refactor (roadmap step 2):

1. **Schema changes are ad hoc.** `store.migrate()` runs one big
   `CREATE TABLE IF NOT EXISTS` blob and then 13 `addColumnIfMissing` calls.
   Nothing records which version a database is at, so there is no way to do
   anything beyond adding a nullable column (no table rebuilds, no data
   backfills, no renames), no way to detect an *older* binary starting on a
   *newer* database, and no safety net before a change is applied. Step 2
   (accounts/destinations, owner-scoped rows for future multi-user) needs real
   migrations.
2. **Health is liveness only.** `/healthz` returns `ok` if the process is
   up. A container whose SQLite volume went read-only, whose schema is wrong,
   or whose disk is full still reports healthy.
3. **The web app has no automated end-to-end coverage.** ~4,000 lines of
   vanilla JS (composer, preview, drafts, history) are only exercised by hand.
   Every UI regression fixed in the log (#15, `a3a2b43`, `935dbd8`, …) was
   found in production.
4. **The image is amd64-only.** Self-hosters on a Raspberry Pi, an Ampere VPS
   or Apple-silicon dev machines can't run the published image. The Dockerfile
   hardcodes `GOARCH=amd64` and pins the *amd64 child manifest* of
   `mwader/static-ffmpeg` (verified: `sha256:6769881c…` is the amd64 entry of
   the 7.1.1 index `sha256:11a44711…`; the arm64 entry is `sha256:89f3465b…`).
5. **Two files carry too much.** `internal/api/api.go` (1,919 lines, ~45
   handlers across 10 concerns) and `internal/dispatch/dispatch.go` (1,308
   lines: types, platform runners, chains, post, interact, schedule, retry)
   are where step 2 will operate. Splitting them first makes the step-2 diff
   reviewable.

## Goals

- Versioned, transactional, forward-only migrations with a recorded history,
  a downgrade guard, and an automatic pre-migration backup.
- A `/readyz` endpoint that reports whether this instance can do its job,
  plus opt-in upstream reachability checks; the container healthcheck uses it.
- A Playwright end-to-end suite that drives the real web app against the real
  API, store and dispatcher, with only the outside world (platform APIs,
  Blossom) faked. Runs in CI on every PR.
- A multi-arch image (`linux/amd64` + `linux/arm64`) from the same Dockerfile,
  cross-compiled (no QEMU for the Go build), with the linkage guard intact,
  and a Docker build on every PR so packaging breaks are caught before `main`.
- `api.go` and `dispatch.go` split into files by concern, as a pure move.

## Non-goals

- Down migrations. SQLite + a single binary make "roll back" mean "restore the
  pre-migration backup"; that is what the backup is for.
- External (SQL-file) migration tooling such as golang-migrate or goose. A
  ~150-line runner keeps the dependency surface small and lets migrations be
  Go when they need to backfill data.
- Changing any API route or behaviour beyond adding `/readyz` (the `/v1`
  rewrite is step 2).
- Graceful shutdown (SIGTERM draining). Noted as a follow-up below; it is
  separate from these five items and touches every background worker.
- Visual-regression screenshots in the e2e suite (brittle across font
  rendering; can come later).

---

## 1. Versioned migrations

### Shape

```go
// internal/store/migrate.go
type migration struct {
    version int
    name    string
    // up runs inside the migration's transaction.
    up func(ctx context.Context, tx *sql.Tx) error
    // noForeignKeys runs the migration with PRAGMA foreign_keys=OFF, for
    // SQLite's 12-step table-rebuild recipe. PRAGMA foreign_key_check must
    // come back empty before the transaction commits.
    noForeignKeys bool
}
```

Migrations live in one ordered slice in `internal/store/migrations.go`.
Versions are dense integers starting at 1; a unit test asserts they are
strictly increasing with no gaps and unique names. Each migration is either a
SQL string (helper `sqlMigration(v, name, sql)`) or Go code.

History is a table, not `PRAGMA user_version`, so operators can see *when*
each step ran:

```sql
CREATE TABLE IF NOT EXISTS schema_migrations (
  version    INTEGER PRIMARY KEY,
  name       TEXT NOT NULL,
  applied_at TIMESTAMP NOT NULL
);
```

### Runner

`Store.migrate(ctx)` on `Open`:

1. Take one dedicated `*sql.Conn` (PRAGMAs are per-connection; the pool must
   not hand us a different one mid-migration).
2. Create `schema_migrations` if missing; read `MAX(version)`.
3. **Downgrade guard:** if the database is newer than the newest migration
   this binary knows, fail `Open` with `ErrSchemaTooNew` ("database is at
   schema vN, this build knows vM; run a newer image or restore a backup").
   Starting would otherwise silently write rows a newer schema doesn't expect.
4. If nothing is pending, return.
5. **Backup:** if the database already holds data (a recorded version, or
   legacy tables from before this change), write `VACUUM INTO
   '<db>.pre-v<N>.bak'` first, replacing a stale file of the same name. A
   failed backup aborts the upgrade (if the disk can't hold a copy, it likely
   can't hold the migration either). Fresh databases skip this.
6. For each pending migration: optionally `PRAGMA foreign_keys=OFF`, then
   `BEGIN IMMEDIATE` (takes SQLite's write lock up front, so two processes
   starting on one file serialize instead of racing), re-check the version
   isn't already recorded (the other process may have applied it while we
   waited), run `up`, run `foreign_key_check` if FKs were off, insert the
   history row, `COMMIT`, restore `foreign_keys=ON`. Any error rolls back and
   fails `Open` with the version and name in the message. SQLite DDL is
   transactional, so a failed migration leaves no half-applied schema.
7. Log each applied migration (version, name, duration) at `info`.

### Baseline (v1) and existing databases

v1 `baseline` is today's schema exactly: the existing `CREATE TABLE IF NOT
EXISTS` blob followed by the 13 historical column additions, in their
historical order. The column additions keep the "add if missing" check
**inside v1 only**, because an existing database can be at any point of
that history. After v1, every migration is plain run-once DDL, and
`addColumnIfMissing` is no longer part of the store's API.

Consequences, all covered by tests:

| Starting state | Result |
|---|---|
| empty file | v1 creates everything; no backup |
| pre-migration DB, fully up to date | v1 is a no-op apart from the history row; backup `pre-v1` written |
| pre-migration DB from an older release (some columns missing) | v1 adds exactly the missing columns, rows untouched |
| DB at v1 | nothing runs |
| DB at a version newer than the binary | `Open` fails with `ErrSchemaTooNew` |

A golden file (`internal/store/testdata/schema.golden.sql`) holds the
normalized `sqlite_master` of a freshly migrated database. A test compares
against it (`go test ./internal/store -run TestSchemaGolden -update`
rewrites it), so every schema change is visible in review as a diff, and the
schema a fresh install gets is checked to equal the one an upgraded install
gets.

### Rules for future migrations (documented in `migrations.go`)

- Never edit or reorder a released migration; add a new one.
- Prefer additive changes. Destructive steps (drop/rename) use the
  rebuild recipe with `noForeignKeys: true`.
- Data backfills are Go `up` funcs in the same transaction as their DDL.
- Future multi-user/plug-in work: owner-scoped columns and plug-in tables are
  just more migrations. Plug-in-owned migrations, if ever needed, would get
  their own namespace column; not built now.

## 2. `/readyz`

### Semantics

- `/healthz` stays as is: **liveness**, process up, no dependencies.
- `/readyz` is **readiness**: can this instance accept and persist work?
  `200` when every *critical* check passes, `503` otherwise. Non-critical
  failures report `"status":"degraded"` with `200`, so an outage at Bluesky
  never makes Docker or a load balancer pull the instance.

```json
{
  "status": "ok",                       // ok | degraded | unavailable
  "checks": {
    "database":      {"status": "ok", "critical": true,  "duration_ms": 1},
    "data_disk":     {"status": "ok", "critical": false, "duration_ms": 0},
    "video_workdir": {"status": "ok", "critical": false, "duration_ms": 0}
  }
}
```

### Local checks (always run, each with a 2 s timeout, run concurrently)

| Check | Critical | Passes when |
|---|---|---|
| `database` | yes | `PingContext` succeeds and the recorded schema version equals the binary's latest migration |
| `data_disk` | no | the DB directory's filesystem has ≥ 256 MiB free (SQLite fails writes on a full disk long before the process dies) |
| `video_workdir` | no | (only when the video pipeline is enabled) the workdir exists and a temp file can be created and removed |

### External checks (opt-in: `GET /readyz?external=1`)

Probing upstreams on every Docker/LB poll would turn a dozen health probes a
minute into traffic at other people's servers, and would couple our
readiness to theirs. So upstream checks only run when asked, are always
non-critical, and their results are cached for 30 s with concurrent callers
sharing one in-flight probe. Only configured upstreams are listed:

| Check | Probe |
|---|---|
| `blossom` | `HEAD` (falls back to `GET`) on `BLOSSOM_URL`; any HTTP response < 500 is reachable |
| `nostr_relay` | NIP-11 `GET` with `Accept: application/nostr+json` on the bootstrap relay's https equivalent |
| `bluesky` | `GET {PDS}/xrpc/_health` |
| `mastodon` | `GET {base}/api/v1/instance` |
| `threads` | `GET https://graph.threads.net/` reachable (no token sent) |

No credentials are sent by any probe — reachability only. Credential validity
is a step-4 (settings/accounts) concern.

### Exposure

`/readyz` is open like `/healthz` (orchestrators can't log in). The body
holds check names, status and a short generic error class (`timeout`,
`unreachable`, `http 5xx`, `schema mismatch`, `not writable`, `low space`);
paths, hostnames and raw errors go to the log only, logged on state change to
avoid probe spam.

### Healthcheck

`/publisher -healthcheck` now hits `/readyz` (local checks only) and exits 0
on 200. The Dockerfile gains a default `HEALTHCHECK` using it, so
`docker ps` shows health without any compose config; existing compose
healthchecks keep working unchanged.

## 3. Splitting `api.go` and `dispatch.go`

Pure move: no renamed identifiers, no signature or behaviour changes, same
package. Verified mechanically: a script parses every top-level declaration
before and after, prints each with `go/printer`, and the two sorted sets must
be byte-identical (comments included). Then build, vet, the full test suite.

`internal/api/`:

| File | Contents |
|---|---|
| `api.go` | size limits, dependency interfaces, `API` struct, `New`, `Routes` |
| `middleware.go` | security headers, CSP, CSRF guard, auth gates |
| `health.go` | `/healthz` (and `/readyz` in item 2) |
| `config.go` | `/api/config`, `/api/identity`, `/api/translate` |
| `legacy.go` | `/publish`, `/upload-media` (n8n endpoints; step 2 replaces them) |
| `media_handlers.go` | compress, video upload, video job |
| `posts.go` | `/api/post`, list/get/progress/attention, retry, relay-retry, cancel, hide, reschedule, public feed |
| `sync.go` | relay-sync endpoints |
| `verify.go` | `/api/verify` |
| `preview.go` | `/api/thread-preview` |
| `interact.go` | `/api/resolve`, `/api/interact` |
| `assemble.go` | `assembleImages`, `fetchSourceMedia`, `mediaError` |

`internal/dispatch/`:

| File | Contents |
|---|---|
| `dispatch.go` | result/override/image types, adapter interfaces, `PostSpec`, `Dispatcher` struct, small helpers |
| `platform.go` | `runPlatform`, `runAction`, `relayHint` |
| `chain.go` | `runHead`, `runChain`, `resumeSegments`, `resumeChain`, `chainStatus`, segment helpers |
| `post.go` | `Post`, `PostWithID`, link cards, imeta helpers, `dispatchTargets` |
| `interact.go` | `InteractSpec`, `Interact`, `InteractWithID`, reproduction/cap helpers |
| `schedule.go` | `Schedule`, `Fire` |
| `retry.go` | `Retry`, `RetryRelay`, relay status helpers |

Exact placement is decided per declaration in the plan; the table is the intent.

## 4. arm64 image

- Build stage runs on `$BUILDPLATFORM` and cross-compiles with
  `GOOS=$TARGETOS GOARCH=$TARGETARCH` (pure Go, `CGO_ENABLED=0`, so no QEMU).
- ffmpeg/ffprobe come from a named stage
  `FROM --platform=$TARGETPLATFORM mwader/static-ffmpeg:7.1.1@sha256:11a44711…`
  — the **index** digest, so each platform gets its own binaries while the
  pin stays immutable.
- The linkage guard keeps running on the build platform; its pattern
  `ld-(linux|musl)` also matches the aarch64 loaders.
- New guard: the check stage asserts each shipped binary's ELF machine
  matches `$TARGETARCH` (a wrong-arch ffmpeg would otherwise fail only at the
  first video upload).
- `publish.yml` builds `linux/amd64,linux/arm64`; `ci.yml` gains a `docker`
  job that builds both platforms on every PR (no push) and smoke-runs the
  amd64 image: start it, wait for `/readyz` 200.

## 5. Playwright end-to-end suite

### What runs

`e2e/` holds a Node project pinned to `@playwright/test` and a Go harness,
`e2e/harness` (its own `main` package, never in the image). The harness wires
the **real** `api.API` routes, **real** `store` (temp SQLite, real
migrations), **real** `dispatch.Dispatcher` and the embedded web assets, with
fakes only at the network edge:

- fake platform posters (Nostr, Mastodon, Bluesky, Threads) that record calls
  and return deterministic remote ids/URLs; `POST /__e2e/down?platform=X`
  makes one fail until `/__e2e/up`, so failure and retry paths are testable
  (a switch rather than magic text, because Retry re-sends the same text);
- an in-process fake Blossom server for media uploads;
- no relays, no DNS, no internet.

Playwright's `webServer` starts the harness on `127.0.0.1:4173`. The harness
exposes `/__e2e/calls` (what each fake platform received) so tests can assert
on what *would* have been sent, not just on what the UI shows.

### First scenarios

1. App boots, tabs switch, no console errors (CSP violations count as errors).
2. Compose a single post to all four platforms → success shown → History lists
   it with per-platform links → fakes received the text.
3. Long text auto-threads: preview shows N segments; fakes receive N chained
   posts with `1/n` numbering.
4. Manual `---` split produces the expected segments.
5. Attach an image with alt text → upload reaches fake Blossom → fakes receive
   the image and alt.
6. Partial failure (`#fail-mastodon`) → History shows partial → Retry with the
   failure removed from the fake → success.
7. Drafts: save, reload page, open from sidebar, content restored; delete.
8. Schedule a post in the future → shows as scheduled in History → cancel.
9. Mobile viewport (Pixel 7): tests tagged `@mobile` (boot, publish +
   History) also run there.

Chromium only at first (the PWA targets mobile Safari too; WebKit can be added
once the suite is stable). CI uploads the HTML report and traces on failure.

### Why a harness instead of the real binary

The real `cmd/publisher` needs a valid NSEC, a Blossom server and live relays
at startup. Faking every upstream protocol (Nostr websockets, ATProto XRPC,
Mastodon REST, Threads Graph) to run the real binary is a project of its
own. The harness covers everything from the browser down to SQLite and only
replaces adapters, which are exactly what step 2 turns into a pluggable
`Destination` interface. Drift risk (harness wiring vs. `main.go`) is
accepted for now; step 2's app constructor can be shared by both.

## Rollout

One branch, one PR, one commit per item, so it can be reviewed commit by
commit. Order: spec/plan → migrations → file split → `/readyz` → arm64 →
e2e. First start on an existing install applies v1 and writes
`publisher.db.pre-v1.bak` next to the database. Nothing else changes for
users apart from `/readyz` and the new image architectures.

## As built (2026-11-04)

All five items are implemented as specified above. What the work turned up:

- **The old ffmpeg pin was amd64-only**, so a naive arm64 build would have
  shipped x86-64 ffmpeg. The new ELF-machine guard fails such a build
  (verified by building arm64 against the old pin).
- **The e2e suite found three real UI bugs on its first run**, fixed in their
  own commit: inline `style` attributes silently blocked by the CSP (the
  media row layout, two spacers, the video progress bar's initial width);
  and the submit button reading "Schedule" after a successful schedule while
  the cleared form would post immediately.
- **Nostr images carried no alt text**: `buildImetas` never wrote NIP-92
  `alt`. Fixed in this PR (alt from the media record, newlines folded); the
  e2e test that first pinned it with `test.fail()` now passes normally.
- **Over-limit warning vs. auto-threading**: posting text longer than a
  platform's limit shows "over-limit targets may be cut or rejected — Post
  anyway?", yet the server auto-threads it correctly. The warning predates
  auto-threading; it belongs to the step-3 thread editor work.
- **Concurrent first start**: two processes migrating the same file could
  collide on the backup file; caught by a stress test and fixed (temp file +
  rename, discarded if another process migrated meanwhile).

## Follow-ups found while doing this

- **Graceful shutdown.** `main` calls `ListenAndServe` with no signal
  handling; `docker stop` sends SIGTERM and kills after 10 s, which can cut a
  thread chain mid-publish (leaving a `partial` the Retrier then has to
  resume). Proposal: `signal.NotifyContext`, flip `/readyz` to 503, stop
  accepting new work, `srv.Shutdown` with a 25 s budget, then stop workers.
- `docs/superpowers/` is listed in `.gitignore` ("kept out of the public
  repo") yet 22 spec/plan files are tracked. Decide before open-sourcing.
