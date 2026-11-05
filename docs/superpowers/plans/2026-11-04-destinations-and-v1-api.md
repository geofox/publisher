# Destinations and the `/v1` API (roadmap step 2) — Implementation Plan

> **For agentic workers:** Steps use checkbox (`- [ ]`) syntax for tracking.
> Spec: `docs/superpowers/specs/2026-11-04-destinations-and-v1-api-design.md`.

**Goal:** the four platforms behind one Destination contract with a
capability manifest; accounts as rows; a persist-first, exactly-once-where-
possible publish engine; a `/v1` API the web app and scripts share.

**Delivery:** four PRs, in order, each deployable on its own. One commit per
task. Small bugs found on the way get their own commit in the PR where they
are found.

**Verification rule for every task:** `go build ./... && go vet ./... &&
go test ./...` green, and `npm test` in `e2e/` green before each PR is
opened. Docker build green in CI.

---

# PR 1 — Contract and drivers (no schema or API change)

> **As built (2026-11-05).** PR 1 shipped the contract, the four drivers and
> the dispatcher on the contract. Differences from the tasks below:
>
> - **One small migration (v2, `error_class`)** on `post_targets` and
>   `target_attempts` instead of an `[invalid] …` prefix in the error text:
>   cheaper than parsing messages and kept by PR 2. PR 2's migrations are
>   renumbered v3 and v4.
> - **The retrier** gives up, with one alert, on `auth`, `invalid`,
>   `rejected`, and on `unknown_outcome` for destinations that can't drop
>   repeats (`ErrorClass.Retryable`). `rate_limited` is treated as
>   transient; honouring `RetryAfter` waits for the PR 2 engine.
> - **Idempotency:** Mastodon sends a deterministic `Idempotency-Key` per
>   item (hash of post id, target and ordinal). Bluesky `rkey` and Nostr
>   "natural" idempotency need the persisted signed event / container id,
>   so they move to PR 2 with the persist-first engine; both stay
>   `IdempotentNone` until then.
> - **Counting:** `URLLength` / `MentionLocalPartOnly` exist in the contract
>   but no driver declares them yet; the splitter and the composer counter
>   learn them in PR 4, so counts stay conservative.
> - **Not moved yet:** `Resolve`, `Identity` aggregation, `verify` and the
>   readiness checks still use their own per-platform services (PR 2, with
>   accounts). Threads token state stays in `threads_token`. The contract
>   test suite (Task 1.2) is per-driver `httptest` tests for now; the shared
>   `destinationtest` suite comes with the first plug-in-facing work.
>
> **Review fixes (2026-11-05, two QA and one security review):**
>
> - Only the publishing request (`createRecord`, `threads_publish`,
>   `POST /statuses`, reblog) can be `unknown_outcome`, via
>   `destination.Committing`; a timeout on login, upload or status poll is
>   `transient`, and so is a context already done before that request. A
>   504 on the publishing request is `unknown_outcome`.
> - Threads reads the Graph API `error.code` / `is_transient` (throttling →
>   `rate_limited`, 190/permissions → `auth`, 1/2 → `transient`).
> - `Capabilities.IdempotencyKeyTTLSecs` (Mastodon 3600): the retrier stops
>   once an attempt that may have published is older than the key's TTL
>   minus 5 minutes.
> - Composer overrides fail open as before (PoW clamped, unknown reply
>   control dropped); the /v1 API in PR 3 rejects bad options with a 400.
> - Smaller fixes: Mastodon quotes carry CW, sensitive, visibility and
>   language; the instance-limits fetch runs once, outside the lock, on its
>   own budget; a byte-upload video over `media.FetchCap` is `invalid` (and
>   badged in the preview); an empty Nostr note is `invalid`; the immediate
>   failure alert no longer promises a retry the retrier won't make;
>   `Segment.Ref` (a signed Nostr event) is stored but not sent to API
>   clients; the e2e fakes have each driver's real interfaces and `Ref.Data`.
> - **Deferred:** thread tails replying to an external post use our head as
>   root (pre-existing; PR 2 engine); a Nostr segment no relay accepted
>   keeps its remote id so resume skips it (pre-existing; PR 2's
>   `target_items`); `AltMaxLength`, `Video.MaxCount`, `MixImagesVideo` and
>   `Kinds` aren't enforced yet (PR 4 preview work); the guard test should
>   also forbid importing driver packages from core once the legacy
>   endpoints go (PR 4); Graph API error bodies are stored unscrubbed in
>   attempt errors (pre-existing; scrub in PR 2).
> - **Task 1.6 (Nostr and Blossom optional) moves to PR 2**, where accounts
>   make "no Nostr account" a first-class state.
> - **Also in PR 1:** `/api/config` returns each destination's name and
>   capabilities and the composer takes its limits from it (a Mastodon
>   instance's own character limit included); the thread preview uses the
>   dispatcher's planner (`PlanTarget`). A guard test keeps destination
>   names out of `dispatch`, `api` and `store`. Fixed on the way: Nostr
>   content warnings were never sent (NIP-36 tag now); a Bluesky quote whose
>   gate write failed lost its link.

## Task 1.1: `internal/destination` types

**Files:** create `internal/destination/{destination,manifest,errors,counting,registry}.go` + tests.

- [ ] `Driver`, `Account`, `Host`, `PublishRequest`, `PublishResult`,
  `ItemRef`, `ThreadRef`, `SourceRef`, `Delivery`, `Identity`, `Health`,
  `Manifest`, `Capabilities`, `MediaCaps`, `Limit`, `OptionSpec`, `Kind`,
  `ContractVersion = 1`. JSON tags on everything that crosses the boundary.
- [ ] Optional interfaces `Interactor`, `Resolver`, `Redeliverer`.
- [ ] `Error` + `ErrorClass` + helpers `Classify(status int, header
  http.Header, err error) *Error` (net timeouts after write →
  `unknown_outcome`; 401/403 → `auth`; 429 → `rate_limited` with
  `Retry-After`; 400/413/422 → `invalid`; other 4xx → `rejected`; 5xx and
  dial errors → `transient`).
- [ ] `Counting.Count(text) int`: graphemes (uniseg, as today),
  codepoints, utf16, bytes, weighted; `URLLength` via the same URL matcher
  the splitter uses; `MentionLocalPartOnly`. Table tests including emoji
  ZWJ sequences, CJK, URLs with paths, `@user@host`.
- [ ] Registry: `Register`, `Lookup(type)`, `All()` (sorted), duplicate
  registration panics.
- [ ] `OptionSpec` validation: `ValidateOptions(specs, map[string]any)`
  returning per-key errors (unknown key, wrong type, not in enum).

## Task 1.2: Contract test suite

**Files:** `internal/destination/destinationtest/suite.go`.

- [ ] `Run(t, Harness)` where `Harness` provides a driver, an account
  config pointed at a fake server, and hooks to script responses.
- [ ] Cases: publish note; publish with media; thread ref round-trip
  (item 2 replies to item 1 using only `ItemRef.Data`); error class for
  400/401/429(+Retry-After)/500/timeout-after-write; idempotency key is
  sent (`key`) or re-send is a no-op (`natural`); `Capabilities` falls back
  to manifest defaults when the server is unreachable.

## Task 1.3: Splitter and media fitting read capabilities

**Files:** `internal/thread`, `internal/transcode`, `internal/dispatch/mediafit.go`.

- [ ] `thread.SplitPlace` takes `(text, Counting, maxLen, imgParts,
  maxImages, Opts)`; `LimitFor` / `MaxImagesFor` deleted. Callers pass
  account capabilities. Existing split tests rewritten as capability tables
  with identical expectations.
- [ ] `transcode.Profile` built from `MediaCaps` (`ProfileFrom(caps)`);
  `ProfileFor` deleted. `BlueskyThumb` stays a Bluesky-driver constant.
- [ ] Host-side `prepareMedia(caps, imgs)`: fit for `bytes` transport;
  fit + re-host for `url` transport (moves `prepThreadsImgs` and the
  `fitImgs` call out of adapters); gate videos (moves `gateVideo`).

## Task 1.4: Drivers

**Files:** `internal/{bluesky,mastodon,threads,nostr}/driver.go` (+ tests
running the contract suite); delete `internal/dispatch/adapters.go` pieces
as they move.

- [ ] Bluesky: `Publish` (note, reply via `ThreadRef`, quote/reply to
  external `SourceRef`, link card, reply gate / disable quotes options,
  `rkey` from idempotency key as a TID), `Repost`, `Resolve`, `Identity`,
  `Check` (`/xrpc/_health` + session), manifest. Partial status when a gate
  write fails (as today). Verify against a PDS that a duplicate `rkey`
  create is rejected; if not, use `applyWrites` with a `create` that fails on
  existing, and record what we found in the spec's "As built".
- [ ] Mastodon: `Publish` with `Idempotency-Key`, visibility / CW / language
  mapping, quote, reblog, resolve, identity; `Capabilities` from
  `/api/v2/instance` (cached 1 h, falls back to manifest defaults).
- [ ] Threads: two-step publish with the container id returned in
  `ItemRef.Data` *before* the publish call (driver reports it through a
  `Host.State()`-backed checkpoint so a crash between calls keeps it);
  topic tag / reply control options; token refresh state moves from the
  `threads_token` table to `Host.State()` (table kept, read once to migrate).
- [ ] Nostr: signed event built and returned in `ItemRef.Data` before
  relay sends (checkpoint as above); relay outcomes as `Deliveries`;
  `Redeliverer`; repost/quote; resolve; imeta from host media; PoW option.
- [ ] Fix while moving: Bluesky `QuoteBsky` drops the remote id when only a
  gate write failed (`adapters.go` quote path returns an empty result on
  error, unlike `PostBsky`); Repost/Quote results carry no request trace.

## Task 1.5: Dispatcher uses the registry

**Files:** `internal/dispatch/*`, `cmd/publisher/main.go`.

- [ ] `Dispatcher` holds `map[string]destination.Account` keyed by type
  (accounts come in PR 2); `runPlatform` / `runAction` become one
  `publishItem` calling `Account.Publish`; the four poster interfaces go.
- [ ] Map `TargetResult` fields from `PublishResult` so the store and API
  are unchanged in this PR.
- [ ] Retrier skips targets whose last error class is `auth`, `invalid` or
  `rejected`; honours `rate_limited` `RetryAfter`. Persist the class in the
  attempt's `error` prefix for now (`[invalid] …`) — the column comes in
  PR 2.
- [ ] `identity.Service`, `resolve.Service`, readiness checks iterate
  accounts instead of named fields.
- [ ] Guard test: no quoted destination type name outside its driver
  package, `cmd/publisher` and test files.

## Task 1.6: Nostr and Blossom optional

- [ ] `NSEC_HEX`/`OWNER_PUBKEY` optional (both or neither); `BLOSSOM_URL`
  optional. Without Blossom, `url`-transport accounts report unhealthy and
  are skipped with a clear target error; media upload endpoints 503.
- [ ] README configuration table updated.

---

# PR 2 — Accounts and the durable engine

> **As built (2026-11-05): split in two.** PR 2a is the storage (Tasks 2.1
> and 2.2); PR 2b is the engine and graceful shutdown (Tasks 2.3 and 2.4).
> Each is deployable, and the storage change is reviewable on its own.
>
> PR 2a differences from the tasks below:
>
> - **Owner ids are on the data, not yet in every query.** v3 adds
>   `owner_id` (default `own_default`) to posts, drafts, api_tokens and
>   users. Store queries start filtering by owner with the `/v1` API (PR 3),
>   whose auth is what produces an owner; with one owner today the filter
>   would be a constant.
> - **`content_json` on targets waits for PR 3.** The content model
>   (shared content, overrides, options) is defined with the `/v1` request;
>   targets keep `final_text` + `fields_json` until then. Items do get
>   `content_json` (`{text, media}`).
> - **`idempotency_keys` moves to PR 3**, with the API that uses it (its
>   migration is v5 with the tokens change).
> - **The `/api` shape is a view over items** (`targetView`): RemoteID,
>   RemoteURL, Relays, Segments and SignedEventJSON come from the target's
>   items, so the web app and the old routes don't change. A thread's
>   attempts keep `item_id` NULL (one attempt spans items); a single post's
>   point at its item.
> - The migration runner's retry limit for concurrent starters was 3; with
>   four migrations it could run out, so it is now one per migration.
>
> PR 2b differences from Tasks 2.3 and 2.4:
>
> - **"Publish now" drives its post inline.** The post is saved whole first
>   (every item planned, with its key), then the request's goroutine claims
>   and sends that post's items, so `/api/post` and `/api/interact` keep
>   their response shape. (`/publish` signs a Nostr note directly and
>   doesn't use the dispatcher; PR 4 removes it.) Background workers (`Run`,
>   `PUBLISH_WORKERS`, default 4) claim everything else: scheduled posts,
>   retries, items recovered at startup. They skip posts being driven inline.
> - **Progress stays in memory.** The inline drive feeds the existing
>   `progress.Registry`. The `events` table and SSE from the database come
>   with the `/v1` API (PR 3), which is where SSE is redesigned.
> - **Loops.** The Scheduler is gone: a scheduled post's items are due at
>   its time, and a pass marks posts past the grace window `missed`. Item
>   retries are decided when a failure is recorded (`RetryPolicy`). The
>   relay-level Retrier and the Nostr fan-out queue keep their own tickers;
>   both only re-send already-signed events, which relays dedupe.
> - **Unknown outcomes.** An item left `sending` at startup is retried when
>   the destination drops repeats (Mastodon within its key's hour, Nostr
>   always). Otherwise it becomes `unknown`, its target gives up, and an
>   alert says "may have been posted". There is no `:resolve` endpoint yet:
>   that is `/v1` (PR 3); a retry by hand covers it until then.
> - **Exactly-once.** Mastodon sends the item's key, derived as before so
>   migrated items keep theirs. Nostr implements `destination.Preparer`: the
>   signed event is saved to the item before the first send and re-sent on
>   every attempt, so Nostr is now `natural`. The Bluesky `rkey` and the
>   Threads container id are deferred: neither platform's behaviour on a
>   repeat is verified yet, so both stay `none` (an unknown outcome stops
>   for the owner).
> - **Rate limits** wait at least `RetryAfter`, and count against the
>   attempt cap, so a platform that keeps answering 429 still gives up.
> - **A thread's attempts are per item** (each `target_attempts` row has
>   its `item_id`). The target's `attempt_count` is its most-tried item's.
> - **Alerts.** A post's first run alerts as before when it doesn't fully
>   succeed. A retry alerts only when the retries give up, and recovery
>   alerts for each item that may have been posted.
> - **The crash e2e is a Go test.** The harness deletes its database on
>   exit, so the kill-and-restart case is
>   `TestCrashBetweenThreadItemsCompletesWithoutDuplicate`: a fake platform
>   that drops repeated keys, with the process stopped after sending part 2
>   of a thread and before recording it.
> - Threads planned before media placement was recorded (no item names any
>   media) get the old front-fill placement when they are retried.

## Task 2.1: Migration v3 — owners and accounts

- [ ] `owners`, `accounts`; `owner_id` on `posts`, `drafts`, `api_tokens`,
  `users` (default `own_default`). Golden schema updated.
- [ ] Store API takes an owner id everywhere (`store.Owner` context helper
  so handlers don't thread it by hand).
- [ ] Env seeding: upsert `source=env, env_key=default` per configured
  type; disable accounts whose env vars disappeared.

## Task 2.2: Migration v4 — targets by account, items, deliveries

- [ ] Rebuild `post_targets` (`noForeignKeys`), add `account_id`,
  `content_json`; `UNIQUE(post_id, account_id)`.
- [ ] `target_items` from `segments_json` and single targets;
  `item_deliveries` from `target_relays`; `target_attempts.item_id`;
  `nostr_fanout` rows re-pointed at items.
- [ ] `idempotency_keys(owner_id, key, method, path, request_hash, status,
  response_json, resource_id, created_at, expires_at, locked_until)`.
- [ ] Migration tests from four fixture DBs (threaded, single, scheduled,
  nostr partial with fan-out rows) asserting every field lands.

## Task 2.3: Persist-first engine

- [ ] `engine.Create(ctx, PostPlan)`: one transaction writes post, targets,
  planned items with idem keys, media. Plan = validated content + per-target
  items from the splitter.
- [ ] Worker loop: claim (`UPDATE … SET status='sending' … RETURNING`),
  one in-flight item per target, publish, record result + attempt +
  deliveries in one transaction, compute next item due. Configurable
  concurrency (default 4).
- [ ] Scheduler, Retrier and fan-out loops folded in (scheduled post =
  items with `next_attempt_at = schedule.at`; `missed` after the grace
  window as today).
- [ ] Progress events from transitions (`events` table, pruned after 24 h)
  feeding SSE; the in-memory `progress.Registry` goes.
- [ ] Startup recovery: `sending` → `unknown`; idempotent ones re-queued,
  the rest flagged `needs_attention`.
- [ ] Old `/api/post`, `/api/interact`, `/publish`, retry/cancel/reschedule
  handlers call the engine (same request/response shapes).

## Task 2.4: Graceful shutdown

- [ ] `signal.NotifyContext` in `main`; readiness flips to 503; `Shutdown`
  with 25 s; workers stop claiming and finish in-flight items; compose
  example and README get `stop_grace_period: 30s`.
- [ ] e2e: harness kills the server between thread items; after restart the
  thread completes with no duplicate (fake platform counts creates).

---

# PR 3 — `/v1` API

> **As built (2026-11-05): split in three.** PR 3a is the foundations and
> the small resources; 3b posts, media and events; 3c drafts and the tools
> (translate, verify, resolve, relay sync, public feed).
>
> PR 3a differences from the tasks below:
>
> - **`/v1` lives in package `api`** (`internal/api/v1*.go`), not a
>   `internal/api/v1` package: the handlers share the API struct's
>   dependencies and helpers with the old routes until PR 4 removes those,
>   and a second package would have meant exporting most of them.
> - **Routes are declared once**, in `v1Routes()`, with their scope and
>   whether they take `Idempotency-Key`; mounting, auth, 405s and the
>   conformance test all read that list.
> - **The validator is pb33f/libopenapi-validator** (pure Go, OpenAPI 3.1).
>   It is a test dependency only; the server serves the YAML converted to
>   JSON with goccy/go-yaml. Adding it moved golang.org/x/{net,crypto,sys,
>   text,sync} to newer minor versions.
> - **Token creation is not idempotent**: its response holds the secret,
>   and storing that for a replay would keep a usable credential in the
>   database for 24 hours.
> - **Validation errors are 422** (`validation_failed`) with every bad
>   field listed; malformed JSON, unknown fields and wrong types are 400
>   (`invalid_json`).
> - **Account health** is not in 3a: nothing measures it yet. It arrives
>   with the account settings work (step 4).
> - **Deferred to 3b:** `Prefer: wait` and the ETag helpers (nothing in
>   3a uses them), and per-route body limits with a streaming fingerprint
>   for `Idempotency-Key`, which `POST /v1/media` needs (3a buffers up to
>   1 MiB, enough for JSON).
> - **Scopes bind the old machine routes too**: `/publish` needs
>   `posts:write` and `/upload-media` `media:write` (found in review: a
>   read-only token could publish).
> - **Colon custom methods** (`:cancel`, `:retry`, `:preview`) can't be
>   ServeMux patterns (a wildcard must be a whole segment); 3b routes them
>   by hand inside a `{id}` handler.

## Task 3.1: Foundations

- [ ] `internal/api/v1` package: router, problem-details writer, error
  codes catalogue (`docs/api/errors.md`), cursor encoding, id prefixes,
  `Prefer: wait`, ETag helpers.
- [ ] Auth: session or bearer on every route; token scopes + expiry
  (migration v5 adds `scopes`, `expires_at`; existing tokens get
  `posts:write media:write`).
- [ ] Idempotency middleware for creates (draft -08 semantics: replay,
  `422` on payload mismatch, `409` while in flight, 24 h expiry).

## Task 3.2: Resources

- [ ] `instance`, `destinations`, `accounts`.
- [ ] `media` (+ `:compress`, video processing state).
- [ ] `posts`: create (note/article/interactions/fanout/draft consume),
  `:preview`, list, get, `PATCH`, `:cancel`, `:retry`, item `:resolve`,
  events (per post and global, `Last-Event-ID`).
- [ ] `drafts` (+ ETag/If-Match), `translate`, `verify`, `resolve`,
  `tokens`, `me`, Nostr relay sync under the Nostr account, public feed.

## Task 3.3: OpenAPI

- [ ] `docs/api/openapi.yaml` (3.1), served as JSON at
  `/v1/openapi.json`.
- [ ] Conformance test: every registered `/v1` route has an operation;
  every handler test validates request and response against the schema
  (kin-openapi or a small validator; pick in the task, prefer no CGO).

---

# PR 4 — Web app on `/v1`, old routes removed

## Task 4.1: Composer and history on `/v1`

- [ ] Destinations/accounts/capabilities from `/v1`; option panels from
  `Manifest.Options`; account picker instead of `ORDER`.
- [ ] Browser `Counting` implementation (`Intl.Segmenter`) shared by
  counters and the live preview; server preview stays authoritative.
- [ ] Post via `POST /v1/posts` with an `Idempotency-Key` per submit;
  progress via `/v1/posts/{id}/events`.
- [ ] History: items per target, `needs_attention` with confirm/retry for
  `unknown` items, error classes in plain words.
- [ ] Drafts with ETag conflict handling ("changed in another tab:
  reload / overwrite").

## Task 4.2: Remove the old routes

- [ ] Delete `/publish`, `/upload-media`, `/api/*` handlers and tests that
  only covered them.
- [ ] README: `/v1` overview, link to the OpenAPI document, "Migrating from
  the pre-1.0 API" with a `curl` per old call including the n8n `/publish`
  flow.

## Task 4.3: e2e

- [ ] Existing scenarios on `/v1`; new: idempotent double submit, restart
  mid-thread, `unknown` item resolved by hand, `invalid` error not retried,
  a second account of one type (seeded by the harness directly in the DB).
- [ ] Spec "As built" section with what the work found.
