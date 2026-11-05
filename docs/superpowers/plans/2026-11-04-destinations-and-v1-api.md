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

> **As built (2026-11-05): split in four.** PR 3a is the foundations and
> the small resources; 3b reading and managing posts plus the event log;
> 3c creating posts (`POST /v1/posts`, `:preview`, `Prefer: wait`) and
> media; 3d drafts and the tools (translate, verify, resolve, relay sync,
> public feed).
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
>
> PR 3b:
>
> - **The event log is a table** (migration v6, `events`), written in the
>   same transaction as each state change (post created, status changes,
>   each item's outcome, each Nostr delivery, reschedule, hide), kept 7
>   days. The SSE streams read it, so a client resumes with
>   `Last-Event-ID` across restarts; an in-process signal wakes streams
>   at once, with a 1 s poll behind it. It is also the future webhook
>   source. The old in-memory progress registry stays for the old routes.
> - **`:cancel` keeps the post** (the old `/cancel` deleted it): planned
>   items and failed ones awaiting an automatic retry become `canceled`,
>   unknown items keep their status but stop being retried, targets left
>   failed or partial are marked given up. A given-up failure is not
>   rewritten (409). Store statuses gain `canceled`.
> - **Store statuses keep the old words** (`success`, `sending`) until
>   PR 4 moves the web app; the API maps them, and `needs_attention` is
>   computed in SQL so the list can filter on it.
> - **`:retry` answers 202** and marks items due; the engine sends them
>   within its poll interval. Relay retries run in the background.
> - **Streams end on shutdown** (`API.Stopping`, closed by the server's
>   `RegisterOnShutdown`), so a graceful stop doesn't wait for clients.
> - **Owner checks**: every post read goes through the principal's owner
>   (404 otherwise), ready for more than one owner.
> - **Test harness**: libopenapi-validator's radix tree takes
>   `/v1/posts/pst_1:cancel` for `/v1/posts/{id}`; the tests give it a
>   lookup that finds nothing so its template regexes match instead.
> - **Review fixes**: a cancel sticks (`posts.canceled_at`: an item in
>   flight isn't retried, a retry by hand clears it and revives canceled
>   items); "settled" means no automatic retry is due either, so
>   `post.completed` is sent once, when nothing is left; PATCH checks
>   everything before changing anything; delivery retries are deduped,
>   aimed at the named target and stopped on shutdown; delivery messages
>   are scrubbed; streams are capped (64 open, 1 h each), HEAD returns at
>   once, and a settled post's stream answers 204 to a client that has
>   everything (EventSource then stops).
> - **Deferred from review**: a delivery retry that fails before reaching
>   the relay (no stored event) is only logged, no event; event `status`
>   is the post's status, not `needs_attention`; event ids are global
>   (gaps show other owners' volume once there are several).

> PR 3c:
>
> - **Notes only.** `POST /v1/posts` takes shared content (text, media by
>   id, content warning, sensitive, languages), targets (account, options,
>   content overrides), `threading.numbering` and `schedule.at`. Explicit
>   per-account items wait for the step-3 editor; replies, quotes, reposts,
>   fan-out and draft consumption move to 3d with resolve and drafts.
> - **One account per type** in a request: the engine still sends through
>   one account per type (keyed by type) until step 4 adds accounts.
>   Targets record the account id (`PostSpec.Accounts`).
> - **Options are checked strictly**, unlike the legacy overrides, which
>   fail open: unknown keys, wrong types, out-of-range ints and a Bluesky
>   reply gate with an unknown word are 422. Every declared option must
>   have a mapping onto the engine's overrides (a test checks), so none is
>   silently dropped.
> - **Media objects** (migration v7, `media_objects`): an image is stored
>   in Blossom at once (201, `ready`); a video is a video job (202,
>   `processing`) that a goroutine finishes when the job ends, and that a
>   GET refreshes too. A processing object whose job is gone (a restart)
>   becomes `failed`. Posts copy the media's fields, so a post keeps the
>   alt text it was sent with.
> - **Async by default**: the post is saved (`Dispatcher.Enqueue`) and the
>   workers send it; `Prefer: wait=N` (≤ 60 s) waits on the event log
>   until the post settles (201) or the wait ends (202). A scheduled post
>   doesn't wait.
> - **Bounds**: 50 000 characters of text, 20 targets, 10 languages, 40
>   media (one video, not mixed with images), 100 thread parts per
>   account.
> - **New error codes**: `unreadable_upload` (400), `upstream_failed`
>   (502, media storage refused the file) and `insufficient_storage` (507).
> - **Review fixes**: a blank per-account text is refused (the engine
>   reads an empty override as "inherit", so it previewed empty and
>   published the shared text); once a post is saved the answer is never a
>   5xx (a retry with its key would post twice); multipart `alt`/`preset`
>   count in any order; the upload's type comes from its content (ftyp
>   brands for QuickTime and HEIC; SVG or markup labelled as an image is
>   refused); storage and ffmpeg errors go to the log, not the caller;
>   uploads hold slots (4 images in memory, 2 videos on disk) and only a
>   video gets the 30-minute deadline; a full disk is `507
>   insufficient_storage`; string options are capped at 500 characters;
>   error pointers name the request's own fields; the video watcher keeps
>   going through a database error.
> - **Deferred from review**: ffprobe/ffmpeg run without
>   `-protocol_whitelist file` (the legacy route too; hardening, not a
>   known exploit); a job whose media object fails to save runs on
>   unreferenced; Threads' own topic-tag rules are left to Threads.
> - Pre-existing: `TestConcurrentWritersDontFailBusy` fails about one run
>   in eight under `-race` on main too (a write waits out the 5 s
>   `busy_timeout` under the race detector's slowdown).

> PR 3d (split again: 3d interactions, 3e the tools):
>
> - **Interactions name the source by URL.** `reply_to`, `quote` or
>   `repost` on `POST /v1/posts` take `{url}`; the server resolves it (the
>   same resolver as `/api/resolve`), so clients never carry platform
>   refs. The action happens on the target account of the source's type,
>   which must be in `targets`; only a quote may have other targets, each
>   a link-quote. A quote's source images are copied to Blossom at create
>   time (best-effort, no videos). Interactions are sent now: `schedule`
>   is refused.
> - **Persist first for interactions too**: `Dispatcher.EnqueueInteraction`
>   plans with `planInteract` (shared with the inline `InteractWithID`)
>   and leaves sending to the workers.
> - **`POST /v1/resolve`** returns the post's author, text, media, created
>   time, your account on its network and per-action availability; it
>   doesn't return the native ref.
> - **Drafts move to PR 4.** A /v1 draft holds the `/v1` post body (media
>   by id), unlike today's composer drafts; moving them now would mean two
>   formats in one table, or converting drafts the current web app still
>   reads. PR 4 moves the web app and converts the drafts once (their
>   media become media objects). Draft consumption (`draft` on create)
>   goes with them.
> - **Review fixes (2 QA + 1 security):** Nostr relay hints in a pasted
>   nevent are dialed only when `wss://` and public (`internal/netguard`),
>   at most 4. Bluesky lookups reuse one session for 15 min, so resolving
>   can't spend the createSession rate limit publishing needs. Copying a
>   quote's images: at most 4 (and what the link-quotes have room for),
>   20 MiB each, in parallel, 30 s in all, videos skipped by type before
>   fetching. A link-quote's copied text is inert: controls and bidi
>   characters dropped, mentions defused with a word joiner, cut at 500
>   characters (web composer mirrors it). Resolve errors: `unrecognized`,
>   `not_supported`, `not_configured`, `unresolvable` (422, fixed text) or
>   502 `upstream_failed` for timeouts/5xx. Reposts refuse content
>   settings and options (the engine ignores them). A native quote with
>   video is refused unless `interactions.quote_video` (Mastodon's quote
>   drops video). Mastodon "can't quote" reasons no longer promise a link.
>   Deferred: hashtags in copied text stay live; a per-token rate limit on
>   `/v1/resolve`; the preview lists copied images only as a warning.

> PR 3e (the tools):
>
> - `POST /v1/translate` (`posts:write`: it spends the DeepL quota),
>   `POST /v1/verify` (`posts:read`; the verdict is always `200`, its
>   `status` says verified, failed or error).
> - **Relay sync under the Nostr account**:
>   `GET/PUT /v1/accounts/{id}/nostr/relays` (`PUT` replaces the
>   secondary list, which keeps its order; the old add/remove pair goes
>   with the old routes) and the custom methods `:scan` and `:sync`
>   (`direction` pull or push, optional `relays` naming targets; a name
>   that isn't a target is a field error rather than silently skipped).
>   `admin` scope. Still one Nostr identity (the env one) until step 4.
> - **`GET /v1/public/feed`** keeps its own token (`PUBLIC_FEED_TOKEN`),
>   documented as a separate security scheme, so a website's server never
>   holds a token that can post. `limit` out of range is `400`, not
>   clamped.
> - `/v1/drafts/{id}:translate` moves with drafts to PR 4.
> - **Review fixes (2 QA + 1 security):** `:scan`/`:sync` run one at a
>   time (`409`), within 5 minutes (context deadline, write deadline
>   extended to answer; unreached relays reported in their rows), so they
>   no longer outlive the 120 s WriteTimeout silently. `partial` added to
>   the sync status enum. A failed NIP-65 lookup is reported
>   (`nip65_error`, and `502` when a named relay may be one of those).
>   Relay URLs are normalized (case, default port, slash) before
>   comparing and storing; loopback and link-local relays are refused
>   (LAN stays allowed). Relay routes accept only the env Nostr account
>   (its key and list are the server's). Verify verdicts no longer echo
>   transport errors (resolved addresses) or panic text, on the old route
>   too. Chunked empty `:scan` bodies work. Feed without a database is
>   `503`.
>   Deferred: a DeepL quota/rate limit per token; per-relay messages stay
>   detailed (admin only, the owner debugging their relays); sync as a
>   background job if 5 minutes proves short.

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

> **As built, split.** PR 4 is four PRs, each leaving the app working:
> 4a (everything around the composer), 4b (the composer), 4c (drafts and
> their conversion), 4d (old routes removed, README migration guide, new
> e2e scenarios).
>
> **4a.** Boot reads `/v1/instance` (languages, translate targets) and then,
> without waiting, `/v1/accounts` (labels, effective limits, profiles; the
> first call can take seconds while profiles are fetched). History, the
> detail pane, translate, verify, relay sync and tokens are on `/v1`.
> - **`:resolve` is new.** `POST /v1/posts/{id}:resolve`
>   `{target, ordinal, outcome: published|not_published, url?}` answers
>   an `unknown` item. `not_published` sends it again, then the rest of
>   the thread. `published` with a URL looks the post up through a new
>   optional driver interface, `destination.Locator` (Bluesky: getPosts,
>   checked against the session DID; Mastodon: search + verify_credentials
>   id), so the next item can reply to it; the URL is required then.
>   Threads can't look posts up: the URL is kept and what follows stays
>   unsent (`partial`) for a `:retry`. `store.ResolveItem` does it in one
>   transaction with events.
> - **History.** Filters map onto `/v1` statuses (`failed` includes
>   `needs_attention`; the attention badge counts `needs_attention`, which is
>   narrower than the old failed+partial count). Each target shows its
>   items (thread parts), error classes in plain words, relay deliveries
>   with a retry each, and "It was posted / It wasn't" for an unknown item.
>   `:retry` and `:resolve` answer at once; the pane follows the post's
>   event stream until `post.completed` (the stream is closed then, or
>   EventSource would reconnect forever). A publishing post gets "stop
>   sending" (`:cancel`); a canceled post stays in the list as `canceled`.
> - **Dropped:** the detail's raw request/response ("verbose") view: `/v1`
>   attempts carry no traces. Error text and class are shown instead.
> - **Tokens** get scopes (posts:write + media:write checked by default, as
>   old tokens had) and an expiry; rows show scopes and expired status.
> - The composer's result modal still reads the `/api/post` shape
>   (`results.js`) until 4b.
> - **Review fixes (4a).** `not_published` resets the item's attempt count,
>   so the engine's duplicate guard (Mastodon past its key hour) doesn't
>   turn it back into `unknown`. `published` without a reference cancels
>   the rest of the thread, and the engine refuses to send a thread part
>   whose root or parent has no remote id (fails `invalid`) instead of
>   posting it detached. A located post that is another part of the same
>   thread is refused (`duplicate`); a token that can't look posts up is
>   `not_permitted`, not a 502. Resolve URLs must be https without user
>   info. Bluesky lookups keep their session unless it was refused (bad
>   URLs no longer cost a sign-in each). The History pane follows a post by
>   re-reading it (stream events wake it, a 2 s poll backs it up) until it
>   stops publishing, or until a retried relay's row changes: the per-post
>   stream replays from the start, so an old `post.completed` can't end the
>   wait. Stale detail loads are dropped. Boot waits up to 1.5 s for the
>   accounts so the composer starts with the effective limits.
> - **Deferred:** a `:retry` of an `unknown` item on a key destination past
>   its window is released as `unknown` again by the duplicate guard (use
>   `:resolve`); the History hides Retry while an item is unknown.

> **4b.** The composer is on `/v1`.
> - **Media.** An image uploads to `POST /v1/media` when attached (again
>   after a compress preset, through `/v1/media:compress`); Post waits
>   for uploads and names media by id, with the alt text in the post, so
>   editing alt needs no call. A video goes to `POST /v1/media` with its
>   preset and is polled with `GET /v1/media/{id}` (stage and progress).
> - **Placement is new on `/v1`:** `content.media[].part` puts a media on
>   a `---` part of the text (clamped per account, as before; refused on
>   replies and quotes).
> - **Drafts in between.** Until drafts move to `/v1` (4c), the old draft
>   routes answer each attachment with a `media_id`: `Store.AdoptMedia`
>   finds or makes a ready media object for the same file (by URL and
>   hash). Only references named by their SHA-256 on an http(s) URL are
>   adopted (old drafts' references came through the client); another one
>   shows "can't be posted: attach it again". A posted draft is deleted by
>   the client (the old route) after the post is saved.
> - **Posting.** `POST /v1/posts` with an `Idempotency-Key`; resubmitting
>   the same body after an answer was lost reuses the key, so it isn't
>   posted twice. Platforms without an account are refused before sending.
>   Options map onto the manifests' options; Mastodon's CW, sensitive and
>   language and Bluesky's languages go as the target's content.
> - **Interactions.** `/v1/resolve` for the Interact tab; replies and
>   quotes post with `reply_to`/`quote`, reposts with `repost`. A reply
>   goes only to the source's network (the other targets are locked; the
>   old app sent copies). The "try anyway" override is gone: `/v1` refuses
>   an action the post doesn't allow, so the action is disabled with the
>   reason.
> - **Preview** uses `/v1/posts:preview` for the focused account, with the
>   text it will post (so a reply's or copy's split shows without
>   resolving the source again) and the uploaded media.
> - **Progress** follows the post (`follow.js`, shared with History): its
>   event stream wakes a re-read until it stops publishing. The result
>   modal and `results.js` are gone.
> - **Review fixes (4b).** Draft media are adopted only from the configured
>   Blossom origin at `/<sha256>[.ext]` (no other host, no query): the
>   engine fetches a media object's URL unguarded, so a client-made
>   reference must never become one. The same file twice in a draft gets
>   two media objects (a post names each once). Replies, quotes and reposts
>   send what was typed (a Nostr id, the Mastodon URL), not the display URL
>   (njump, the origin), which may not resolve again. Post waits for a
>   preset being applied; a draft attachment (image or video) that can't
>   be posted says so. A stale in-flight id is checked against the list of
>   publishing posts (no 404 on boot). Keys fall back to getRandomValues
>   on plain-http installs; the composer's and Interact's retry keys are
>   apart. `missed` reads "Not posted"; the modal says when following
>   gives up. The translate menu is built without innerHTML.
>   Deferred: an index on `media_objects(owner_id, sha256)` (adoption reads
>   scan the owner's media; fine at this scale, goes with 4c's
>   conversion); preview refusals (a video one network refuses) show only
>   the single-post card; relay deliveries of a Nostr thread's later parts
>   aren't in the modal (History has them).

> **4c.** Drafts are on `/v1`.
> - **`/v1/drafts`**: list (`q` in text or title, `tag` repeated = all of
>   them, newest change first, cursors on `updated_at`+id), create, read,
>   `PATCH` (each field sent replaces; `post` as a whole), `DELETE`, and
>   `:translate` (a new draft; needs `posts:write` too, for the DeepL
>   quota). A draft holds a `POST /v1/posts` body as it stands: nothing is
>   required, but what is there is checked against a post's bounds and must
>   name the owner's media and accounts. It is stored without its unset
>   fields, so it posts as it is. The title defaults to the text's first
>   line and follows the text until one is given.
> - **Versions.** Each write bumps `drafts.version`, sent as a strong ETag.
>   `If-Match` on a stale version is `412 precondition_failed` (new code)
>   with the current ETag; a PATCH without it still writes against the
>   version it just read, so a write in between isn't lost silently.
> - **Posting a draft**: `"draft"` on `POST /v1/posts` (and `:preview`)
>   deletes it once the post is saved. Needs `drafts:write` (field error
>   `not_permitted`); `unknown_draft` otherwise. A replayed
>   Idempotency-Key replays the answer and deletes nothing more.
> - **Migration v8** adds `drafts.body_json` and `version` and two
>   indexes (`drafts(owner_id, updated_at, id)`,
>   `media_objects(owner_id, sha256)`). **Conversion at startup**
>   (`API.ConvertOldDrafts`, once per draft, before serving): attachments
>   on this server's Blossom origin become media objects (others are
>   dropped and logged), platforms become the enabled account of each type,
>   overrides become options (only those the manifest declares) and
>   per-account content, `anchors` become `part`, the title is derived
>   again (the old one was cut at 80 bytes). Numbering, schedule and a
>   reply or quote are mapped too, though the old server never stored
>   them, so old drafts come with the defaults. A draft that fails is
>   logged and tried again next start.
>   Old drafts' media rows stay, unused. The old `/api/drafts*` routes are
>   gone.
> - **Web app.** Drafts save as JSON through `/v1/drafts` with `If-Match`;
>   a 412 asks to overwrite. Opening a draft restores targets, per-account
>   settings, media with their alt and part, numbering, schedule, tags, and
>   a reply or quote (resolved again; if the source can't be read, it
>   opens as a new post and says so). Posting sends `draft`. The list is
>   built without innerHTML.
> - Fixed on the way: `DeriveTitle` and `NormalizeTags` cut bytes, which
>   could split a character.
> - **Review fixes (4c).** A posted draft is taken (deleted at the version
>   the request read) before the post is saved and put back if saving
>   fails, so a double click or two tabs make one post, and a draft saved
>   again meanwhile is kept (`draft_changed`). The draft check runs the
>   post's checks too (options against the manifest, blank per-account
>   text, one account per type, `schedule` without `at`). 412s from a race
>   carry the current ETag; If-Match compares strongly. PATCH is not a
>   merge patch (documented). The conversion checks the rest of an old
>   attachment's description (poster only from media storage, known
>   types, sane dimensions). The list reads neither bodies nor whole
>   texts, takes at most 20 `tag`s, and `:translate` checks `posts:write`
>   before reading the draft. Web app: a loaded schedule shows (button
>   "Schedule"); New and Delete clear schedule, numbering, targets and
>   tags; opening builds the draft first and the last click wins; edits
>   made while saving stay unsaved; missing accounts are said; Overwrite
>   goes through the single save.
>   Deferred: a cap on drafts per owner; expected 4xx (412, a gone draft)
>   still log a browser console line.
> - **Deploy note:** migration v8 (additive) and the one-time draft
>   conversion at first start; old drafts' attachments not on the
>   configured `BLOSSOM_URL` origin are dropped from the converted draft
>   (logged with the draft id).

> **4d.** The old routes are gone.
> - **`/publish`, `/upload-media` and `/api/*` answer `410 Gone`**, not
>   404, with a pointer to the README's "Migrating from the pre-1.0 API",
>   so an n8n flow that wasn't moved fails with the fix in its error. They
>   pass the session gate to get there (nothing behind them).
> - **Removed with them:** the old handlers and their tests (the behaviour
>   they pinned is covered by the `/v1` tests; the link-card and media-note
>   preview tests were ported to `/v1/posts:preview`), the in-memory
>   progress registry (`internal/progress`; the event log replaced it in
>   3b), `requireAPIToken`, the old draft store functions (only the reads
>   the v8 conversion needs stay), and `dispatch.NewID`. The API's
>   `Dispatcher` interface is down to the relay retry.
> - **Logging.** The old `/publish` wrote the only per-send log line
>   (`relays_succeeded` and so on, which the README told you to alert on).
>   The engine now writes one per attempt: `engine: item sent` (info) or
>   `engine: item not sent` (warn), with platform, ordinal, attempt,
>   status, latency, remote id, Nostr endpoint counts, and the error class
>   and give-up flag when it failed.
> - **Found: a second account of a type would post as the first.** The
>   engine holds one connected account per type and looked it up by type,
>   so a target naming another stored account of that type (possible once
>   step 4 adds accounts, or with a row written by hand) would have been
>   sent with the connected account's credentials. `Dispatcher.AccountIDs`
>   (type → the stored account the credentials belong to, from
>   `SeedEnvAccounts`) closes it: `POST /v1/posts` and `:preview` refuse
>   such a target (`account_disabled`), and the engine fails one already
>   saved as `auth` without sending, relay retries included.
> - **e2e.** The harness can hold a platform's next call for good, make a
>   platform refuse (`invalid`), and restart the engine in process: the
>   old dispatcher is abandoned mid-send (its held call never returns, as
>   in a killed process) and a new one on the same database recovers what
>   was left sending. Killing the server process would lose the harness's
>   temporary database and fakes. It also seeds a second Mastodon account
>   straight into the database. New scenarios: a submit whose answer was
>   lost, resubmitted from the composer (one post); two racing submits
>   with one key; a restart mid-thread on Mastodon (the part is re-sent
>   with its key and the thread carries on); the same on Bluesky leaves
>   the part `unknown`, and "It wasn't: send it again" in History finishes
>   the thread; an `invalid` refusal given up at once while a transient
>   failure waits for its retry; the second account listed, refused by
>   `/v1`, and not used by the composer.
> - **README.** The `/v1` section is the API (drafts added to its table);
>   the old route sections are replaced by the migration guide: a map
>   from every old route to its `/v1` call, what changed besides paths,
>   and the n8n steps (token, account id, `POST /v1/media`, `POST
>   /v1/posts` with `Idempotency-Key` and `Prefer: wait`, reading the
>   outcome). Security and Logging sections updated.
> - **Review fixes (4d).** The log line scrubs the error (as the store
>   does) and caps it at 300 characters; recovery at startup, a release
>   after a forgotten key and a panic write it too, so the README's alert
>   sees every give-up. The README's n8n steps say `GET /v1/accounts` needs
>   `accounts:read`, and that `Prefer: wait` can still answer 202 and a
>   settled post can be `needs_attention`. `/v1/resolve` suggests only the
>   connected account. A missing type now fails "not configured", not
>   "another account". New Go test for the `/v1/media` video lifecycle
>   (ready and failed), which the old route's tests had covered. The e2e
>   restarts wait until the held call reached the fake, and the invalid
>   test cancels its pending retry in a `finally`.
>   Deferred: `Dispatcher.Post`, `PostWithID`, `Interact`, `Schedule` and
>   `Retry` and the store's `AddSyncRelay`, `RemoveSyncRelay`,
>   `CancelScheduled`, `HidePost` and `CreateAPIToken` are now used only by
>   tests (about 28 engine tests drive `Post`); moving those tests onto
>   `Enqueue` and deleting them is its own change. A held e2e call keeps
>   one engine worker busy until the next restart.
> - **Deploy note:** no migration. Move every n8n flow (or other caller)
>   off `/publish` and `/upload-media` before deploying: they answer 410.

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

- [x] Delete `/publish`, `/upload-media`, `/api/*` handlers and tests that
  only covered them.
- [x] README: `/v1` overview, link to the OpenAPI document, "Migrating from
  the pre-1.0 API" with a `curl` per old call including the n8n `/publish`
  flow.

## Task 4.3: e2e

- [x] Existing scenarios on `/v1`; new: idempotent double submit, restart
  mid-thread, `unknown` item resolved by hand, `invalid` error not retried,
  a second account of one type (seeded by the harness directly in the DB).
- [x] Spec "As built" section with what the work found.
