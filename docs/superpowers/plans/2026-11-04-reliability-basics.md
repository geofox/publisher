# Reliability basics (roadmap step 1) — Implementation Plan

> **For agentic workers:** Steps use checkbox (`- [ ]`) syntax for tracking.
> Spec: `docs/superpowers/specs/2026-11-04-reliability-basics-design.md`.

**Goal:** Versioned SQLite migrations, a `/readyz` readiness endpoint, a
Playwright e2e suite, a multi-arch (amd64 + arm64) image, and `api.go` /
`dispatch.go` split by concern — without changing any existing behaviour.

**Delivery:** one branch, one PR, one commit per task group below, in this
order (later groups build on earlier ones: `/readyz` reads the schema
version; the split happens before `/readyz` adds `health.go`; e2e exercises
everything).

**Verification rule for every task:** `go build ./... && go vet ./... &&
go test ./...` green before commit.

---

## Task 1: Migration runner + baseline

**Files:** create `internal/store/migrate.go`, `internal/store/migrations.go`,
`internal/store/migrate_test.go`, `internal/store/testdata/schema.golden.sql`;
modify `internal/store/store.go`.

- [ ] Move the `schema` const and the 13 column additions into migration v1
  `baseline` (column additions keep their historical order; the
  add-if-missing helper becomes private to v1 as `addLegacyColumn`).
- [ ] Runner: dedicated `*sql.Conn`; `schema_migrations` table; downgrade
  guard `ErrSchemaTooNew`; `VACUUM INTO <db>.pre-v<N>.bak` when the DB holds
  data and something is pending; per-migration `BEGIN IMMEDIATE` with
  re-check, optional `foreign_keys=OFF` + `foreign_key_check`, history row,
  `COMMIT`; info log per migration.
- [ ] `Store.SchemaVersion(ctx)`, `Store.Ping(ctx)`, `LatestSchemaVersion()`.
- [ ] Tests:
  - migrations list: versions dense from 1, names unique;
  - fresh DB → latest version, golden schema matches (`-update` flag);
  - legacy DB (pre-migration schema with no added columns, plus rows) →
    upgraded, rows intact, backup file exists and is a valid DB;
  - legacy DB with a subset of columns already added → works;
  - reopen → no-op, no new backup;
  - too-new DB → `ErrSchemaTooNew`, DB untouched;
  - failing migration (injected list) → rolled back, version not recorded,
    error names version + name;
  - `noForeignKeys` rebuild migration works; one that leaves a dangling FK
    fails `foreign_key_check` and rolls back;
  - two concurrent `Open`s on one file both succeed, each version recorded once.
- [ ] Verify by hand: old binary's fresh-DB schema == new binary's fresh-DB
  schema (dump `sqlite_master` from both).

## Task 2: Split `api.go` and `dispatch.go`

**Files:** `internal/api/{api,middleware,health,config,legacy,media_handlers,posts,sync,verify,preview,interact,assemble}.go`;
`internal/dispatch/{dispatch,platform,chain,post,interact,schedule,retry}.go`.

- [ ] Write `scratch/declcheck` (not committed): parse a package dir, print
  every top-level decl (with doc comment) via `go/printer`, sort, hash.
- [ ] Record the decl set for both packages before the move.
- [ ] Move declarations; fix imports with `goimports`-equivalent (`go run
  golang.org/x/tools/cmd/goimports` or manual).
- [ ] Decl sets identical after the move; build/vet/test green.

## Task 3: `/readyz`

**Files:** create `internal/health/health.go`, `internal/health/health_test.go`,
`internal/health/disk_*.go`; modify `internal/api/health.go`,
`internal/api/middleware.go` (open path), `internal/api/api.go` (route +
field), `cmd/publisher/main.go` (wire checks, `-healthcheck` → `/readyz`),
`Dockerfile` (`HEALTHCHECK`), `README.md`.

- [ ] `health.Checker`: named checks with `Critical`, per-check timeout,
  concurrent run, `Report{Status, Checks}`; error classes not raw errors;
  state-change logging.
- [ ] External checks: separate set, run only with `?external=1`, cached 30 s,
  single in-flight probe (mutex + shared result).
- [ ] Checks: `database` (ping + schema version), `data_disk` (statfs ≥
  256 MiB; stub on unsupported OSes), `video_workdir` (create/remove temp file);
  external `blossom`, `nostr_relay`, `bluesky`, `mastodon`, `threads`.
- [ ] Handler: GET/HEAD, JSON, 200/503, `Cache-Control: no-store`; open in gates.
- [ ] Tests: all-ok 200; critical fail 503; non-critical fail 200 degraded;
  timeout classified; external not run without param; external cached;
  gate stays open with auth enabled; schema mismatch fails `database`.

## Task 4: arm64 image

**Files:** `Dockerfile`, `.github/workflows/publish.yml`,
`.github/workflows/ci.yml`, `README.md`.

- [ ] `FROM --platform=$BUILDPLATFORM` build stage, `TARGETOS/TARGETARCH`.
- [ ] Named ffmpeg stage pinned to the index digest
  `sha256:11a44711684c0b9f754c047dcd64235b8b52deab251bd0e0a86f22faa160749c`.
- [ ] ELF machine guard in the check stage.
- [ ] `HEALTHCHECK` instruction.
- [ ] publish: `platforms: linux/amd64,linux/arm64`.
- [ ] CI `docker` job: buildx both platforms (no push, gha cache), load amd64,
  run with throwaway keys, wait for `/readyz` 200.
- [ ] Verify locally: build both platforms; check ELF arch of all three binaries
  in each image; run the amd64 image and hit `/readyz`.

## Task 5: Playwright e2e

**Files:** create `e2e/package.json`, `e2e/package-lock.json`,
`e2e/playwright.config.ts`, `e2e/tests/*.spec.ts`, `e2e/harness/main.go`,
`e2e/harness/fakes.go`, `e2e/README.md`; modify `.github/workflows/ci.yml`,
`.gitignore`.

- [ ] Harness: real store/dispatcher/api/web with fake posters + fake Blossom;
  `#fail-<platform>` toggles; `/__e2e/calls` and `/__e2e/reset`; listens on
  `$PORT`; temp DB dir removed on exit.
- [ ] Playwright config: `webServer` runs `go run ./e2e/harness`, waits for
  `/readyz`; Chromium desktop + Pixel 7 projects; traces on first retry;
  `forbidOnly` in CI.
- [ ] Shared fixture failing a test on console errors / CSP violations.
- [ ] Scenarios 1–9 from the spec (drop or adjust any that the UI can't
  support deterministically, and say so in the PR).
- [ ] CI `e2e` job: setup-go, setup-node, `npm ci`, `npx playwright install
  --with-deps chromium`, run, upload report on failure.
- [ ] Run the suite locally three times in a row to check for flakes.
