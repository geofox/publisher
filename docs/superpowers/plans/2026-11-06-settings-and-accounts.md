# Settings and accounts for self-hosters (roadmap step 4) — Implementation Plan

> **For agentic workers:** Steps use checkbox (`- [ ]`) syntax for tracking.
> Spec: `docs/superpowers/specs/2026-11-06-settings-and-accounts-design.md`
> (cited below as "spec §n").

**Goal:** any number of accounts per network; sign-in without an OIDC
provider; credentials entered in the web app and encrypted at rest; settings
in the app with env precedence; OAuth connect for Mastodon and Threads; a
self-hosting story that needs one volume, one key and an https address.

**Delivery:** six PRs, A to F, in order, each deployable on its own (spec
§Parts). One commit per task; a small bug found on the way gets its own
commit in the PR where it is found.

**Branch and PR flow.** All work is on `claude/project-thread-24i2fi`. A PR
is opened from it when its part is done and reviewed. While a PR waits for
its merge, the next part is built locally on top of it and pushed only after
the merge (rebased onto the new `main`), so each PR shows only its own part.
If several PRs should be open at once (stacked), that needs extra branches,
which Geoffrey would have to allow.

**Per-PR loop (Geoffrey's workflow).** Build → open the PR → two QA review
agents and one security review agent on the PR's diff → fix every finding
with a regression test → push → hand over. Then the next part starts without
waiting for input. Each PR body starts with the attribution block and ends
with the Claude Code footer; `geofox` is assignee and reviewer; PR activity
is subscribed.

**Verification rule for every task:** `go build ./... && go vet ./... &&
go test -race ./...` green. Before a PR is opened also: `npm test` in
`e2e/` green, `go test ./internal/store -run TestSchemaGolden` green (after
`-update` when the schema changed on purpose), the OpenAPI conformance test
green, the drivers guard test green.

**Conventions (from steps 1–3):**

- Migrations are append-only (`internal/store/migrations.go`); a table
  rebuild uses `noForeignKeys: true`; every migration gets a test that
  applies it to a fixture of the previous version; the store writes
  `.pre-vN.bak` before applying.
- The store, dispatcher, API and the new packages never name a destination
  type in a string literal (the guard test); `internal/drivers` is the one
  place that does.
- Problem details for every `/v1` error (`docs/api/errors.md` lists codes);
  every new route is in `v1Routes()` and the OpenAPI document.
- Secrets never in logs, events, attempts, traces or error messages.

**Migration numbers** are as planned: A v11, B v12, C v13, D v14, E v15.

---

# PR A — Accounts by id; Nostr and Blossom optional

Spec §1 (getBool, Nostr env rules), §2, §4.2, §4.3, §12 (A).

The biggest part: everything that today assumes one account per type. Order
of tasks: contract and drivers first (no behaviour change), then the store,
then the registry, then the engine, then services and API, then the web app
and the harness.

### Task A1: strict booleans

**Files:** `internal/config/config.go`, `internal/config/config_test.go`.

- [ ] `getBool(k, d) (bool, error)`: `1/true/yes/on`, `0/false/no/off` in
  any case, surrounding spaces trimmed; anything else is an error naming the
  variable and the accepted values. `Load` returns it.
- [ ] Call sites: `AUTO_RETRY_ENABLED`, `NOSTR_PRIMARY_FANOUT` (moves to the
  Nostr driver in A3, keeping the strict parse), `OIDC_END_SESSION`.
- [ ] Tests: each accepted spelling, an empty value takes the default,
  `flase` fails `Load` with the variable's name.

**Done when:** `AUTO_RETRY_ENABLED=flase` stops startup with
`AUTO_RETRY_ENABLED: "flase" is not a boolean (use true/false, 1/0, yes/no,
on/off)`.

### Task A2: contract v2 core

**Files:** `internal/destination/destination.go`, `registry.go`, new
`internal/destination/open.go`, `internal/destination/health.go`,
`internal/destination/source.go`, tests.

- [ ] `ContractVersion = 2`. Every built-in manifest sets `Contract: 2`.
- [ ] `Driver` gains `Open(ctx, OpenRequest) (Account, error)` (spec §4.2).
  `OpenRequest{AccountID, Method, Config map[string]any, Secrets
  map[string]string, Tuning, Host AccountHost}`. Open does no network I/O.
- [ ] `Tuning{PublishTimeout, POWTimeout, RelayCacheTTL, VerifyTimeout
  time.Duration; PLCDirectoryURL string}`.
- [ ] `AccountHost` interface: `SaveSecrets(ctx, map[string]string) error`,
  `SaveConfig(ctx, map[string]any) error`, `HTTPClient(NetPolicy)
  *http.Client`, `Logger() *slog.Logger`. `NetPolicy` is a small enum:
  `NetTrusted` (env-configured addresses, today's behaviour) and
  `NetOwner` (owner-typed addresses; the dial-time guard lands in C4, until
  then it behaves like `NetTrusted`, and nothing in A uses it).
- [ ] `EnvSource{FromEnv(getenv func(string) string) ([]EnvAccount,
  error)}`, `EnvAccount{Key, Label, Method, Config, Secrets}` (`Label` is
  the label used when the row is first created; empty → the manifest
  name).
- [ ] `Identity.RemoteID` (json `remote_id`).
- [ ] `Checker`, `CheckRequest{Authenticated bool}`, `Health{OK, Reachable,
  AuthOK *bool, Message, ExpiresAt *time.Time}`.
- [ ] `Refresher{NextRefresh(now) time.Time; Refresh(ctx) error}`.
- [ ] `SourceResolver{ResolveSource(ctx, input) (ResolvedSource, error)}`
  with `ResolvedSource{Ref SourceRef; Preview SourcePreview; Actions
  SourceActions}`. `SourcePreview` and `SourceActions` carry what
  `resolve.Preview` and `resolve.Caps` carry today (author name and handle,
  text, media with alt and video flag, created at, web URL; reply, quote,
  repost each `{allowed, reason}`), so the API's resolve response keeps its
  shape. Errors: `ErrSourceNotFound`, `ErrSourceUnsupported` (wrapping a
  reason).
- [ ] `destinationtest.Host`: an in-memory `AccountHost` for driver tests
  (records saved secrets and config, returns `http.DefaultClient`-like
  clients with the test's transport).
- [ ] Tests: `Register` panics on a contract-1 manifest; JSON round trip of
  `Identity`, `Health`, `ResolvedSource`.

**Done when:** the package builds and its tests pass; drivers don't compile
yet (A3 makes them).

### Task A3: drivers open accounts and read their own env

**Files:** `internal/{nostr,bluesky,mastodon,threads}/driver.go` (+ new
`env.go` each), their tests, `internal/config/config.go`,
`internal/config/config_test.go`.

Each driver gets `Open` and `FromEnv`. `config.Load` stops reading and
validating destination variables; their tests move with them, case for case.

- [ ] **Nostr** `FromEnv`:
  - `NSEC_HEX` set → one account (key `default`). `OWNER_PUBKEY` optional:
    derived when unset; when set it must match (today's error text). 
    `OWNER_PUBKEY` without `NSEC_HEX` is an error ("OWNER_PUBKEY is set but
    NSEC_HEX is not").
  - Config from `NIP65_BOOTSTRAP_RELAY`, `FALLBACK_RELAYS`,
    `PRIMARY_RELAYS`, `NOSTR_PRIMARY_FANOUT` (strict bool),
    `POW_DIFFICULTY_DEFAULT`, `POW_DIFFICULTY_MAX`, `SYNC_RELAYS` (as
    `sync_relays_seed`), with today's defaults unchanged.
  - Secret `secret_key` (hex). `Open` builds `nostr.Publisher` from config,
    secrets and `Tuning` (POW timeout, relay cache TTL, publish timeout).
- [ ] **Bluesky** `FromEnv`: `BLUESKY_APP_PASSWORD` set → one account;
  `BLUESKY_IDENTIFIER` required with it (error naming it); `BLUESKY_PDS_URL`
  as config `pds_url` (today's default handling kept). Secret
  `app_password`, config `identifier`.
- [ ] **Mastodon** `FromEnv`: `MASTODON_TOKEN` set → one account;
  `MASTODON_BASE_URL` required with it (error naming it; today a token
  without a base URL silently fails every send). Config `instance_url`,
  secret `access_token`.
- [ ] **Threads** `FromEnv`: `THREADS_ACCESS_TOKEN` set → one account;
  `THREADS_USER_ID` config `user_id` (default `me`). Secret `access_token`.
- [ ] `Open` for each validates that required config and secrets are
  present (`destination.Error{Class: invalid}` naming the field) and builds
  today's client. No network I/O (the Mastodon instance-limits fetch and the
  Bluesky session stay lazy, as today).
- [ ] `config.Config` keeps bootstrap and tuning fields only; destination
  fields go. `BLOSSOM_URL` stays in config (media), now optional.
- [ ] Tests per driver: unset → no account; complete → one account with the
  expected config/secrets; each partial combination → the named error; Open
  with missing fields → `invalid`; Open never dials (a transport that fails
  the test on any request).

**Done when:** `go test ./internal/...` passes with main.go still building
accounts by hand (A11 switches it).

### Task A4: identity, checks and source resolution on the accounts

**Files:** driver packages (`source.go`, `driver.go`), new `check.go` each;
`internal/resolve/` (shrinks to `Detect`); `internal/health/checks.go`.

- [ ] `Identity.RemoteID`: Bluesky the DID, Mastodon the account URL from
  `verify_credentials`, Threads the user id, Nostr the public key (hex).
- [ ] `Checker` on every account. `Authenticated: false` reproduces today's
  probes: Bluesky `GET <pds>/xrpc/_health`, Mastodon `GET
  <instance>/api/v1/instance`, Threads `GET https://graph.threads.net/`,
  Nostr a WebSocket open on the bootstrap relay (`health.NostrRelay`'s
  logic, moved). `Authenticated: true`: Bluesky `getSession`, Mastodon
  `verify_credentials`, Threads `/me?fields=id`, Nostr none (`AuthOK`
  nil). The Threads account reports `ExpiresAt` from its token state.
- [ ] `SourceResolver` on Bluesky, Mastodon and Nostr accounts: the code of
  `resolve.BlueskyAdapter`, `MastodonAdapter`, `NostrAdapter` moves into the
  driver packages, returning `destination.ResolvedSource`. Mastodon resolves
  through its own instance's search (so `LocalID` belongs to that account's
  instance, as today). Threads has none (today's "Threads posts can't be
  resolved" message comes from the API when the detected type's accounts
  aren't resolvers).
- [ ] `resolve` keeps `Detect(input) string` (exported, no I/O, today's
  rules) and the JSON response shape used by `/v1/resolve`; `Service` and
  the adapters go. Their tests move to the driver packages.
- [ ] Tests: identity remote ids from recorded responses; checks against
  `httptest` servers (unauthenticated sends no credential header; a 401 on
  an authenticated check gives `AuthOK=false`); resolver tests moved.

### Task A5: migration v11 and the store by account

**Files:** `internal/store/migrations.go` (+ `migrations_accounts.go`),
`accounts.go`, `engine.go`, `items.go`, `models.go`, `fanout.go`,
`relays.go` (sync relays), tests, `testdata/schema.golden.sql`, a v10
fixture.

- [ ] **v11** (spec §2.8): `accounts.remote_id TEXT`, `default_selected
  INTEGER NOT NULL DEFAULT 1`, `public_feed INTEGER NOT NULL DEFAULT 1`;
  `nostr_sync_relays (account_id TEXT NOT NULL REFERENCES accounts(id), url
  TEXT NOT NULL, added_at TIMESTAMP NOT NULL, PRIMARY KEY (account_id,
  url))`. Migration test from a v10 fixture with accounts, posts and sync
  relays: columns present, defaults 1, nothing else changed.
- [ ] `store.Account` gains `RemoteID`, `DefaultSelected`, `PublicFeed`.
  `UpdateAccountMeta(ctx, id, AccountMeta)` (label, enabled,
  default_selected, public_feed; nil fields untouched; bumps
  `updated_at`) and `SetAccountIdentity(ctx, id, handle, remoteID)` (no
  `updated_at` bump, so identity refreshes don't look like edits).
- [ ] `SeedEnvAccounts(ctx, owner, []EnvAccount{Type, Key, Label})`:
  - creates a missing `(type, key)` env row with the label;
  - re-enables an existing env row **without touching its label**;
  - disables env rows no longer configured;
  - never touches rows whose `source` isn't `env`;
  - returns every env row (enabled or not).
  Tests: a renamed env account keeps its label across restarts; a second
  restart with one variable removed disables that row only.
- [ ] `ItemKey(postID, accountID, ordinal)`; the fallbacks at
  `models.go:197` and `dispatch/publish.go:244` pass the account id. A test
  shows stored keys of an existing post are read back unchanged (not
  recomputed) after the change.
- [ ] `SavePost` refuses a target with an empty `AccountID` (`ErrNoAccount`);
  `envAccountFor` goes. Every caller is checked: the dispatcher plan (A8),
  the drafts conversion (`v1_drafts_convert.go`, which maps a legacy type to
  the first account of that type, or skips the target with a log line when
  there is none), tests.
- [ ] `Claim`, `SendingItem`, `Unscheduled` gain `AccountID` (the queries
  already join `post_targets`; select `pt.account_id`).
- [ ] Fan-out: `EnqueueFanout(postID string, itemID int64, signedEvent
  string, relays []string)` writes `item_id`; `FanoutJob` gains `ItemID`
  and `AccountID` (`DueFanout` joins `target_items` → `post_targets`; rows
  without an item id, from before v4's backfill, keep `AccountID` empty).
- [ ] Sync relays per account: `SyncRelays(ctx, accountID)`,
  `AddSyncRelay`, `RemoveSyncRelay`, `SeedSyncRelaysIfEmpty(ctx,
  accountID, urls)`, and `AdoptLegacySyncRelays(ctx, accountID)` (copies
  `sync_relays` into the account's list when that list is empty, then
  empties `sync_relays`, in one transaction; idempotent).
- [ ] Golden schema updated.

### Task A6: the account registry

**Files:** new `internal/accounts/registry.go`, `status.go`, `state.go`,
`refresh.go`, tests; `internal/drivers/legacy.go`.

- [ ] `Registry` (spec §2.1): `Load`, `Get`, `List(owner)`, `OfType(owner,
  typ)`, `Acquire(id) (destination.Account, release func(), error)`,
  `Caps(ctx, id, typ)`, `Reopen(ctx, id)`, `Update(id, store.Account)`,
  `OnChange(fn) (unsubscribe)`, `ReportAuthFailure(id, reason)`.
- [ ] **Order**: types in `destination.Types()` order, then `created_at`,
  then id; `List` returns that order and an `Order` index per entry.
- [ ] **Opening**: an `Opener` field (default: `destination.Lookup(type)` →
  `Driver.Open`), so tests and the e2e harness inject fakes. Env accounts
  open with what `FromEnv` returned for their `(type, key)`; an env row
  with no matching env account is `not_configured`; a row whose type isn't
  registered is `not_configured` with reason "driver not installed".
  Database accounts don't exist until C; `Load` treats `source = 'db'` as
  `locked` "needs part C" (unreachable in practice, tested).
- [ ] **Leases**: each entry holds its live object and a reference count.
  `Reopen` builds the new object outside the lock, swaps it in, and closes
  the old one (`io.Closer`) when its last lease is released. `Acquire` on a
  non-connected account returns a typed `*accounts.Unavailable{Status,
  Reason}` error.
- [ ] **Status** (spec §2.2): `connected`, `disabled` (row disabled),
  `not_configured`, `locked`, `needs_reconnect` (set by
  `ReportAuthFailure` or an authenticated Check that fails auth; cleared by
  a successful authenticated Check or a `Reopen`), `deleted` (C).
- [ ] **Caps** returns the open account's capabilities, else the type's
  manifest defaults; `false` only for an unknown type.
- [ ] **Metadata updates** (`Update`) replace the row in place, keep the
  object, and emit a `Change`; identity writes (`SetIdentity`) don't emit
  one.
- [ ] **Host and state** (`state.go`): the `AccountHost` the registry hands
  to `Open` is per account. `SaveConfig` merges into `accounts.config_json`.
  `SaveSecrets` writes through a `SecretBackend`. In part A the only
  backend is `drivers.LegacyState`, which knows the Threads token table
  (the one place allowed to name the type): it stores `access_token` in
  `threads_token` with `seed_hash` = SHA-256 of the env token it derives
  from, and on `Load` returns the persisted token when the seed hash
  matches the current env token (today's `needsSeed` rule), so an operator
  who changes `THREADS_ACCESS_TOKEN` gets the new one. Other types have no
  persisted secrets in A (`SaveSecrets` returns an error that says so).
- [ ] **Refresh loop** (`refresh.go`, spec §4.4): every minute, for each
  connected account that implements `Refresher` and whose `NextRefresh` has
  passed: lease, `Refresh` with a 30 s timeout, release; one account at a
  time, ±10 % jitter on the minute. A failure alerts at most once a day per
  account while the credential still works.
- [ ] Tests (`-race`): lease swap closes the old object only after release;
  concurrent Acquire/Reopen; Update keeps the same object pointer; Caps
  fallback for locked and unknown; order across types and creation times;
  `ReportAuthFailure` then a good Check returns to connected; legacy
  Threads state round trip and seed-hash reset.

### Task A7: Threads token refresh as a Refresher

**Files:** `internal/threads/tokenmanager.go` → `refresh.go`, tests,
`internal/metrics`.

- [ ] The Threads account implements `Refresher`: `NextRefresh` is today's
  rule (token at least 24 h old and within 10 days of expiry → now; else
  that moment), `Refresh` calls `refresh_access_token`, saves the new token
  through `Host.SaveSecrets` and the new expiry through `Host.SaveConfig`
  (`token_expires_at`, `token_refreshed_at`).
- [ ] On `Open` the account reads `token_expires_at`/`token_refreshed_at`
  from its config (absent: unknown; the first authenticated Check or
  refresh fills it).
- [ ] Alerts: the registry's refresh loop sends today's "expires within 7
  days" alert and the failure alert, naming the account.
- [ ] The token-expiry gauge reports the soonest expiry over Threads
  accounts (the registry sets it from `Health.ExpiresAt`).
- [ ] `TokenManager` goes; its tests are ported case for case.

### Task A8: the dispatcher by account id

**Files:** `internal/dispatch/{dispatch,plan,publish,engine,interact,retry,post,fanout,retrier}.go`
and tests.

- [ ] `Dispatcher.Accounts` and `AccountIDs` go; `Dispatcher.Registry
  AccountSource` (interface: `Acquire`, `Caps`, `Get`, `ReportAuthFailure`,
  `List`), satisfied by `*accounts.Registry` and by a test fake.
- [ ] `PostSpec.Targets []TargetSpec{AccountID, Type, Text, Overrides,
  Layout}` replaces `Platforms`, `Overrides`, `Layouts`, `Accounts`
  (`Overrides.Text` goes; `TargetSpec.Text` is what the account posts).
  `InteractSpec` gets `SourceAccount` and `Targets []TargetSpec` (link-quote
  copies), replacing `SourcePlatform`'s account lookup, the `Overrides` map,
  `Fanout` and `Accounts`; `SourcePlatform` stays as the source's type.
  `interaction_json` stores `source_account`; matching a target to the
  interaction (`interact.go:164`) uses the account id, with a fallback to
  the type for stored rows that predate it.
- [ ] Every lookup by type becomes a lookup by account id: `planTarget`,
  `planPostCards` (keyed by account id), `publishItem`, `repost`, recovery
  (`engine.go:343,484,828` use `Caps(ctx, si.AccountID, si.Platform)`),
  retries, `CanLocate`/`Locate`, interactions, link cards (`post.go:35`).
- [ ] `SendsAs`, `errOtherAccount`, `ConnectedIDs` go.
- [ ] Unavailable accounts at send time (spec §2.2): `locked` → the
  attempt fails `transient` "account locked: <reason>" (normal retry);
  `needs_reconnect`, `disabled`, `not_configured` → `auth` with the
  reason. An `auth` failure from the network calls
  `ReportAuthFailure(id)`.
- [ ] `displayName(id, typ)`: "Bluesky (@geoffrey.one)", the type's name
  plus handle or label; the type's name alone when the registry no longer
  has the account. Used by alerts.
- [ ] Tests: two accounts of one type in one post publish independently
  (their own text, layout, caps); a locked account's item is retried and
  goes out after `Reopen`; a disabled one fails `auth` without a network
  call; recovery of a sending item uses the account's caps; the existing
  dispatch tests pass with the fake registry (one account per type).

### Task A9: fan-out and relay retries by account

**Files:** `internal/dispatch/fanout.go`, `retrier.go`, `retry.go`,
`publish.go` (enqueue), tests.

- [ ] `enqueueFanout` passes the item id. The fan-out worker runs always
  (its `enabled` flag goes: whether anything is pending is each Nostr
  account's own `primary_fanout`, already expressed by
  `PublishResult.Pending`).
- [ ] A due job leases the job's account; when that account isn't open (or
  the job has no account), any connected account whose live object is a
  `Redeliverer` of the same type (registry order). With none, the job waits
  (`next_attempt_at` pushed by the backoff, not counted as a failure).
- [ ] The retrier passes the target's id to `RetryTargetRelay` instead of
  target 0 (`retrier.go:127`).
- [ ] Tests: two Nostr accounts, each event redelivered through its own
  account; account locked → redelivered through the other; no Nostr
  account → waits without burning attempts; relay retry hits the right
  target.

### Task A10: services per account (identity, readiness, relay sync, feed)

**Files:** `internal/accounts/identity.go`; delete `internal/identity`;
`internal/health/health.go`; `internal/relaysync/relaysync.go`;
`internal/api/v1_tools.go`, `v1_resources.go`; `internal/feed/feed.go`;
`internal/nostr` (a `RelayLister` interface); tests.

- [ ] **Identity**: `accounts.Identities` caches `Identity` per account for
  10 minutes (6 s timeout per call, failures cached 1 minute), and writes
  handle and remote id through `SetAccountIdentity`. `GET /v1/accounts`
  reads it. A changed `remote_id` on an env account (the operator put
  another account's token in the variable) is logged as a warning and
  stored; history keeps the account id.
- [ ] **Readiness**: `health.Checker.External` becomes a provider
  `func() []health.Check` evaluated per probe (results still cached). The
  provider returns `account:<id>` per enabled account implementing
  `Checker` (unauthenticated), and `blossom` when a Blossom URL is set.
  The separate `nostr_relay` check goes: each Nostr account's check probes
  its own bootstrap relay. (Spec §2.4 kept `nostr_relay`; it would
  duplicate the account check, so it is dropped and the upgrade note
  says so.)
- [ ] **Relay sync**: `relaysync.Sync` takes the home relay and public key
  per call (`Scan(ctx, Account{Home, PubKey}, targets)`, `Apply(…)`). The
  relay handlers (`/v1/accounts/{id}/nostr/relays*`) lease the path's
  account and assert `nostr.RelayLister` (`ResolveWriteRelays`,
  `HomeRelay`, `PubKey`); another type → 404 `not_found`. The sync relay
  list comes from `nostr_sync_relays` for that account. `HomeRelay` and
  `np` leave the API struct. `features.relay_sync` is true when a Nostr
  account is connected.
- [ ] **Public feed**: `feed.Link` gains `account` and `handle`; targets of
  accounts with `public_feed = 0` are left out. Tests for both.

### Task A11: wiring in `cmd/publisher`; Nostr and Blossom optional

**Files:** `cmd/publisher/main.go`, `internal/api/api.go`,
`internal/api/v1_media.go`, `v1_drafts_convert.go`, `v1_interact.go`,
`v1_create.go`, `internal/drivers/drivers.go`, `guard_test.go`.

- [ ] `drivers.EnvAccounts(getenv) ([]EnvAccount-with-type, error)` calls
  `FromEnv` on every registered driver that implements it (error → startup
  error naming the variable). `main.go` seeds them, builds the registry,
  calls `drivers.AdoptLegacy(ctx, st, registry)` (sync relays to the first
  env Nostr account, then `SeedSyncRelaysIfEmpty` from its
  `sync_relays_seed`), loads the registry, starts its refresh loop, then
  `d.Recover` and the workers as today.
- [ ] `main.go` names no destination package or variable after this task;
  the guard test covers `cmd/publisher` and `internal/accounts` (and
  forbids importing a driver package from them, except `internal/drivers`).
- [ ] Media optional: `BLOSSOM_URL` unset → no media pipeline. Set without
  an env Nostr key → startup error "BLOSSOM_URL is set but no Nostr key
  signs uploads: set NSEC_HEX (part D gives the media store its own key)".
  `api.New(mp *media.Pipeline)` (the Nostr publisher argument goes); a nil
  pipeline makes `POST /v1/media`, `:compress` and video jobs answer `503
  unavailable` "no media store is configured"; `ConvertOldDrafts` and
  interaction source-media re-hosting skip media when nil. The dispatcher's
  `Host` is nil without media (a URL-transport account then gets the
  canonical URLs, which only exist when media exists).
- [ ] Startup log: the accounts by type and status instead of the Nostr
  pubkey and Blossom URL.
- [ ] Tests: `main`'s wiring is exercised by a small `run(ctx, getenv)`
  extraction with a test that starts with only `BLUESKY_*` set (no Nostr, no
  Blossom) and answers `/readyz` and `GET /v1/instance` with
  `features.media=false`.

### Task A12: the API by account

**Files:** `internal/api/v1.go`, `v1_create.go`, `v1_interact.go`,
`v1_drafts.go`, `v1_resources.go`, `v1_posts.go`, `v1_tools.go`,
`openapi.json` (or its generator), `docs/api/errors.md`, tests.

- [ ] **Scopes**: add `accounts:write`. `principal.can` no longer treats
  `admin` as every scope: `admin` implies the scopes that existed before
  step 4 (so existing admin tokens keep working) and nothing added since.
  Test: an admin token gets `403` on `PATCH /v1/accounts/{id}`.
- [ ] **Create and preview** (`planCreate`): build `TargetSpec`s per
  account; `duplicate_type` and the "isn't connected on this server"
  refusal go; a target naming the same account twice stays refused
  (`duplicate_account` field error); an account that isn't connected →
  `validation_failed` with `account_disabled` and its status (`disabled`,
  `not_configured`, `locked`, `needs_reconnect`) in the detail. Drafts
  accept such accounts and return a warning.
- [ ] **Replies, quotes, reposts**: the target of the source's type is the
  acting account (exactly one: a second is `one_native_actor`); the source
  is resolved with that account (`SourceResolver`), so a Mastodon `LocalID`
  is on the acting account's instance. Other targets are link-quote copies.
- [ ] **`POST /v1/resolve`** takes optional `account`; default: the first
  connected account of the detected type that is a resolver; the response
  gains `account`. No such account → `422 not_configured` (today's
  message); Threads → today's unsupported message.
- [ ] **Accounts views**: `status`, `status_reason`, `remote_id`, `order`,
  `default_selected`, `public_feed`, `profile` (display name, avatar,
  profile URL from the identity cache) and `capabilities` per account.
  `ETag` on `GET /v1/accounts/{id}`.
- [ ] **`PATCH /v1/accounts/{id}`** (`accounts:write`, `If-Match`
  optional; `*` allowed; stale → `412`): `label` (1–64 chars, trimmed),
  `enabled`, `default_selected`, `public_feed`; unknown fields →
  `validation_failed`; event `account.updated` with the changed field
  names. Goes through `Registry.Update`.
- [ ] `GET /v1/instance`: `features.media`; `features.relay_sync` per A10.
- [ ] OpenAPI document and `errors.md` (`one_native_actor`,
  `duplicate_account`, `unavailable` for media). Conformance test green.
- [ ] Tests: two Mastodon accounts in one create; preview per account with
  different limits; a locked/disabled/not-configured target; interaction
  with two accounts of the source network (second refused) and with an
  explicit acting account; resolve with `account`; PATCH including ETag
  and scope checks.

### Task A13: the web app by account

**Files:** `internal/web/assets/accounts.js`, `state.js`, `compose.js`,
`editor.js`, `count.js`, `plan.js`, `preview.js`, `history.js`,
`drafts.js`, `drafts_recovery.js`, `interact.js`, `tools.js`, `main.js`,
`common.js`, `app.css`.

- [ ] `accounts.js`: `loadAccounts` keeps the list in display order
  (types in the web app's `ORDER` first, other types after in API order;
  within a type the API's `order`); exports `accounts()`, `byId(id)`,
  `ofType(type)`, `connected()`, `defaultTargets()`; `accountFor` goes.
- [ ] `state.js`: `state.platforms` → `state.targets` (a `Set` of account
  ids); `state.ov`, breaks and media positions keyed by account id; the
  option cards chosen by the account's type; `v1Target(id)` and
  `buildSpec` send `account`. Recovery snapshot version 3 keyed by account
  id; a version-2 snapshot (keyed by type) is converted on restore by
  mapping each type to its first connected account; unknown keys dropped.
- [ ] `compose.js`: target rows list every account grouped by network,
  avatar plus handle; non-connected accounts greyed with their reason and
  not selectable; default selection = connected accounts with
  `default_selected`; editor tabs and Compare columns one per selected
  account, labelled avatar plus short handle; the tab row scrolls
  horizontally below 600 px; Alt+1…Alt+9 follow tab order; counting and
  limits per account (`count.js` reads the account's capabilities).
- [ ] `plan.js`: the preview's targets keyed by account id.
- [ ] `history.js`: each target shows its account (handle) and status;
  a filter by account. `drafts.js`: drafts store and restore account ids
  (already the case on `/v1`); a draft naming a deleted account drops it
  with a notice.
- [ ] `interact.js`: with several connected accounts on the source's
  network, a picker chooses the acting account (default: the first);
  `/v1/resolve` gets `account`.
- [ ] `tools.js`: relay tools pick the Nostr account (a select when there
  are several; hidden when none).
- [ ] `main.js`: the composer header uses the first account (display order)
  with a display name.
- [ ] Without a media store (`features.media=false`) the attach button is
  hidden and a hint says why.
- [ ] Unit-free (no JS test runner in the repo): covered by e2e in A14.

### Task A14: the e2e harness and specs

**Files:** `e2e/harness/main.go`, `engine.go`, `fakes.go`;
`e2e/tests/*.spec.ts`; new `e2e/tests/accounts.spec.ts`.

- [ ] The harness builds an `accounts.Registry` with an `Opener` that
  returns recording fakes by account id, and seeds: two Mastodon accounts
  (500 and 1000 character limits), two Bluesky, one Threads, one Nostr, and
  one `not_configured` Mastodon row. `seedSecondAccount` goes. An engine
  restart builds a new registry and dispatcher on the same database.
- [ ] Existing specs updated for account-keyed selectors (tabs and chips
  carry `data-account`).
- [ ] `accounts.spec.ts`: select both Mastodon accounts, edit each tab,
  Compare shows both columns with their own limits; publish posts once per
  account (fake recorder shows two Mastodon sends with different texts);
  retry of one account's failed item; a version-2 recovery snapshot in
  `localStorage` restores into account-keyed state; the not-configured
  account is greyed and unselectable; an interaction picks the acting
  Mastodon account; `PATCH` label shows in the composer after reload;
  `default_selected=false` leaves an account unselected in a new post.

### Task A15: docs, PR and review

- [ ] README: `NSEC_HEX`/`OWNER_PUBKEY`/`BLOSSOM_URL` optional, the
  `OWNER_PUBKEY`-alone error, strict booleans, readiness check names
  `account:<id>`, `MASTODON_BASE_URL` now required with `MASTODON_TOKEN`.
- [ ] Upgrade notes for A (spec §12).
- [ ] Spec "as built" notes for A appended to this plan.
- [ ] Open PR A; 2 QA + 1 security review agents; fix; hand over.

---
# PR B — Sign-in without OIDC

Spec §1 (`PUBLIC_URL`, `AUTH_MODE`, `TRUSTED_PROXIES`, `SETUP_TOKEN_FILE`),
§8, §10.1, §12 (B).

### Task B1: verify before building

- [ ] Authelia (Geoffrey's OIDC provider): does it honour `prompt=login`
  and `max_age=0`, and return `auth_time` in the ID token? Check its
  current documentation and source; record the answer here. If it doesn't,
  OIDC re-authentication falls back to "signed in within the last 5
  minutes" (`auth_time`, else the session's creation time) and the spec's
  §8.4 note is updated.
- [ ] `github.com/go-webauthn/webauthn` current release and API
  (`webauthn.New`, `BeginDiscoverableLogin`, `FinishDiscoverableLogin`,
  `BeginRegistration`, `FinishRegistration`, the `User` interface). Pin the
  version in `go.mod`; record any difference from the spec.

### Task B2: bootstrap variables

**Files:** `internal/config/config.go`, tests.

- [ ] `PUBLIC_URL`: an origin only (no path, query or fragment); `https`,
  or `http` for `localhost`, `127.0.0.1` or `[::1]`; anything else is a
  startup error. When set: added to the allowed hosts; the app base URL
  (CSRF, redirects); the default `OIDC_REDIRECT_URL` base
  (`PUBLIC_URL/auth/callback`) when that variable is unset.
- [ ] `AUTH_MODE`: `oidc | local | proxy`, or empty (decided at startup,
  B3). `oidc` without `OIDC_ISSUER` is an error; `OIDC_ISSUER` with
  `AUTH_MODE=local` or `proxy` is an error (one sign-in system at a time).
- [ ] `TRUSTED_PROXIES`: comma-separated CIDRs or addresses; invalid →
  error naming the entry.
- [ ] `SETUP_TOKEN_FILE`: a path; its directory must exist.
- [ ] Tests for each rule.

### Task B3: migration v12 and the instance's birth version

**Files:** `internal/store/migrations.go`, `migrate.go`, new
`internal/store/localauth.go`, tests, golden schema.

- [ ] v12 (spec §8.2): `webauthn_credentials`, `password_credentials`,
  `login_failures` (+ index on `(addr, at)`), `reauth_nonces`,
  `setup_tokens`, `users.source TEXT NOT NULL DEFAULT 'oidc'`,
  `instance_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`.
- [ ] Birth version: `store.Open` reads the schema version before
  migrating. A new database (version 0) gets
  `instance_meta('created_schema', <latest>)` after migrating; an existing
  one gets `created_schema = <version it had>` inside v12. `AUTH_MODE`
  unset then means: `oidc` with `OIDC_ISSUER`; `local` when
  `created_schema >= 12`; `proxy` otherwise.
- [ ] Store functions: credentials CRUD, `RecordLoginFailure`,
  `LoginFailures(addr, since)`, `PruneLoginFailures` (24 h and 10,000 rows),
  setup tokens (mint invalidates older; consume once), reauth nonces
  (create, consume once, purge expired), `LocalOwnerUser` (the local
  `users` row, `subject = 'local:owner'`, `source = 'local'`).
- [ ] Migration test from a v11 fixture; birth-version test for a new and
  an upgraded database.

### Task B4: client address, throttling, hashing limits

**Files:** new `internal/httpx/clientip.go`, new
`internal/localauth/throttle.go`, `password.go`, tests.

- [ ] `ClientIP(r, trusted []netip.Prefix)`: without trusted proxies, the
  connection's address; with them, walk `X-Forwarded-For` from the right
  and return the first address not in a trusted range (the connection's
  address must itself be trusted for the header to count).
- [ ] Throttle (spec §8.2): checked before any hashing; per address 5
  failures in 15 minutes, then a delay doubling up to 15 minutes; per owner
  a delay after 20 failures in 15 minutes, never a block; passkey
  assertions exempt from the owner delay. Answers `429` with
  `Retry-After`.
- [ ] Password hashing: argon2id t=3, m=64 MiB, p=4, 16-byte salt, 32-byte
  key, PHC string; at most 2 hashes at once (a semaphore; a third caller
  gets `429` immediately); passwords 12–1024 bytes; constant-time compare.
- [ ] Tests: spoofed `X-Forwarded-For` without trusted proxies is ignored;
  rightmost-untrusted with a chain; throttle windows with a fake clock;
  the semaphore under `-race`.

### Task B5: passkeys and password sign-in

**Files:** new `internal/localauth/{webauthn,service}.go`,
`internal/api/auth_local.go`, `internal/api/auth.go`,
`middleware_auth.go`, tests.

- [ ] `localauth.Service`: WebAuthn relying party (id = `PUBLIC_URL` host,
  origin = `PUBLIC_URL`, user verification required, discoverable
  credentials); ceremony state kept in memory for 5 minutes under a random
  id carried by an HttpOnly, Secure, SameSite=Strict cookie (`Path=/auth`);
  the stored credential records its relying party id, and an assertion for
  a credential of another relying party id is refused.
- [ ] Routes (outside `/v1`, same-origin checked): `POST
  /auth/passkey/begin`, `POST /auth/passkey/finish`, `POST /auth/password`
  (`{"password"}`), `POST /auth/logout` (as today). Success creates a
  session exactly as OIDC does (same cookie, TTL, sweep), records
  `login.succeeded`; failure records `login.failed` and a
  `login_failures` row; messages never say which part failed.
- [ ] In `local` mode an unauthenticated page request shows the sign-in
  page (served by the web handler) instead of redirecting to an OIDC
  provider; `/v1` answers `401` as today.
- [ ] Sign count: a lower count than stored is logged as a possible clone
  and the assertion refused (authenticators that always send 0 are
  accepted).
- [ ] Tests with go-webauthn's test helpers or recorded ceremonies:
  register then log in; wrong origin; wrong relying party; replayed
  challenge; password right, wrong, throttled.

### Task B6: setup token, first credential and recovery commands

**Files:** `internal/localauth/setup.go`, `internal/api/setup.go`,
`cmd/publisher/cli.go`, tests.

- [ ] At startup in `local` mode with no local credential: mint a token
  (32 random bytes, stored hashed, 1 hour, single use), invalidate older
  ones, and log `setup: open /setup#<token> on the address you use for
  Publisher` (`PUBLIC_URL` prefixed when set), or write the URL to
  `SETUP_TOKEN_FILE` (0600) and log only the path.
- [ ] `/setup` (web page): reads the fragment, calls `POST
  /setup/exchange {token}` once, which sets a setup cookie (`Path=/setup`,
  10 minutes, HttpOnly, Secure, SameSite=Strict), and clears the fragment
  (`history.replaceState`). `POST /setup/passkey/begin|finish` and `POST
  /setup/password` need that cookie and create the first credential and a
  session. Once any local credential exists every `/setup` route answers
  `410 gone`.
- [ ] `local` mode without `PUBLIC_URL`: the setup page explains how to
  serve Publisher over https (`tailscale serve`, Caddy, Traefik) instead of
  failing on a dropped Secure cookie; the startup log says the same.
- [ ] CLI: `publisher setup-url` (opens `DB_PATH`, mints a token, prints
  the URL); `publisher owner reset-login [--revoke-tokens]` (deletes local
  credentials and sessions, lists tokens holding `admin` or
  `accounts:write` — and later scopes — revoking them with the flag,
  prints a setup URL). Both refuse to run when `AUTH_MODE` resolves to
  something other than `local`.
- [ ] Tests: token single use and expiry; a second start invalidates the
  first token; setup closes after the first credential; CLI against a
  temp database.

### Task B7: re-authentication

**Files:** `internal/localauth/reauth.go`, `internal/api/reauth.go`,
`internal/auth/auth.go` (OIDC `prompt`/`max_age`, `auth_time`), tests.

- [ ] Operations (closed list): `signin.add`, `signin.remove`,
  `password.set`, `token.create_privileged`; later parts add
  `account.reveal`, `account.delete`, `instance.secrets`, `export`,
  `import`.
- [ ] `POST /auth/reauth {operation, passkey assertion | password}` →
  `{nonce, expires_at}` (single use, 5 minutes, bound to the session and
  operation).
- [ ] OIDC: `GET /auth/reauth/oidc?operation=…` starts a login with
  `prompt=login&max_age=0`; the callback checks `auth_time` is after the
  start (B1's fallback otherwise), marks the session, and redirects back;
  the page then calls `POST /auth/reauth/claim {operation}` for the nonce.
- [ ] Middleware `requireReauth(op)` reads `Publisher-Reauth`; missing or
  invalid → `403 reauth_required` with the operation in the problem.
  Tokens can't re-authenticate: an operation that needs it answers `403
  session_required` to a token.
- [ ] Applied in B to: creating a token that holds `admin` or
  `accounts:write`; adding or removing a sign-in method; setting the
  password.
- [ ] Proxy mode: these operations answer `403 sign_in_required`.
- [ ] Tests: nonce single use, wrong operation, expiry, another session's
  nonce; OIDC claim with a stale `auth_time`.

### Task B8: managing sign-in methods

**Files:** `internal/api/v1_me.go`, OpenAPI, tests.

- [ ] `GET /v1/me/sign-in` → passkeys (id, name, created, last used),
  password set or not, the mode. Session only.
- [ ] `POST /auth/passkey/register/begin|finish` (session +
  `signin.add`); `PATCH /v1/me/passkeys/{id}` (rename);
  `DELETE /v1/me/passkeys/{id}` (`signin.remove`); `PUT /v1/me/password`
  (`password.set`; changing an existing password also needs the current
  one); `DELETE /v1/me/password` (`signin.remove`). Removing the last
  local sign-in method → `409 last_sign_in_method`.
- [ ] `GET /v1/me/sign-in-events`: the last 50 successes and failures
  (time, address, method), from events and `login_failures`.
- [ ] `GET /v1/instance` reports `auth: oidc | local | proxy` and, in proxy
  mode, `sign_in_recommended: true`.

### Task B9: the web app

**Files:** `internal/web/assets/signin.js` (new), `setup.js` (new),
`access.js` (new), `reauth.js` (new), `tokens.js`, `main.js`,
`index.html`, `app.css`.

- [ ] Sign-in screen (local mode): "Sign in with a passkey" (discoverable,
  no username), and a password field when a password is set.
- [ ] Setup screen (B6), with the https explanation when needed.
- [ ] `reauth(op)`: a dialog asking for a passkey (or password, or a fresh
  OIDC login) and returning a nonce; `v1()` retries a `reauth_required`
  answer once through it.
- [ ] Access panel (in Tools until D's Settings tab): passkeys (add,
  rename, remove), password (set, change, remove), recent sign-in events.
- [ ] Proxy-mode banner "Set up sign-in" linking the README section.

### Task B10: e2e and docs

- [ ] The harness gains a local-auth mode (`E2E_AUTH=local`, `PUBLIC_URL=
  http://localhost:<port>`; Chromium treats localhost as a secure context
  and keeps Secure cookies there). A Playwright project runs
  `signin.spec.ts` with a CDP virtual authenticator: setup token → passkey
  → signed in; sign out and back in; add a password; re-authenticate to
  create an admin token; throttling after five wrong passwords; setup
  closed afterwards.
- [ ] README: sign-in modes, `PUBLIC_URL`, `TRUSTED_PROXIES`, setup and
  recovery commands, upgrade notes for B (OIDC unchanged; OIDC-off stays
  proxy until `AUTH_MODE=local`).
- [ ] As-built notes; open PR B; 2 QA + 1 security reviews; fix; hand over.

---

# PR C — Secret store, credentials and the account API

Spec §3, §4.1, §4.4 (Threads token move), §5, §10.2, §12 (C).

### Task C1: verify PDS discovery

- [ ] For a custom-domain handle, a `bsky.social` handle and a `did:web`
  account: confirm the chain handle → DID (DNS `_atproto` TXT, then
  `https://<handle>/.well-known/atproto-did`) → DID document (PLC or
  `did:web`) → `#atproto_pds`, and that `createSession` on the discovered
  PDS accepts the identifier forms (handle, DID, email). Record here.

### Task C2: the keyring

**Files:** new `internal/secrets/{keyring,seal}.go`, tests;
`cmd/publisher/cli.go` (`keygen`); `internal/config`.

- [ ] `PUBLISHER_MASTER_KEY[_FILE]`, `PUBLISHER_MASTER_KEY_PREVIOUS[_FILE]`
  (spec §3.1): 32 bytes, base64 in any of the four alphabets/padding
  forms; both forms of one set, or a malformed value, is a startup error;
  a `_FILE` is read once, trimmed, and must not be world-readable (warning,
  not error, since Docker secrets are 0444 by default — record which).
- [ ] HKDF-SHA-256 derivation of the encryption key and the 8-byte key id;
  `Seal(plaintext, AAD fields) (string, error)` and `Open`; format
  `v1.<kid>.<base64url(nonce‖ciphertext‖tag)>`; AAD per spec §3.2.
- [ ] `publisher keygen` prints a new key and a compose snippet.
- [ ] Tests: round trip; every AAD field change fails to open; unknown key
  id; tampered ciphertext; previous key opens, current seals; base64
  variants; known-answer test for the derivation.

### Task C3: migration v13 and the secrets store

**Files:** `internal/store/migrations.go`, new `internal/store/secrets.go`,
`internal/store/scrub.go`, `store.go` (pragma), tests, golden schema.

- [ ] v13: `secrets` (spec §3.2) with `idx_secrets_key`;
  `accounts.deleted_at TIMESTAMP`, `accounts.auth_method TEXT`; partial
  unique index `(owner_id, type, remote_id) WHERE deleted_at IS NULL AND
  remote_id IS NOT NULL`.
- [ ] `PRAGMA secure_delete = ON` on every connection (`store.Open` DSN).
- [ ] Store functions take a `*sql.Tx` so account rows and their secrets
  commit together: `PutSecret`, `Secrets(owner, scope, ref)`,
  `DeleteSecrets(owner, scope, ref[, names])`, `SecretKeyIDs()`,
  `ResealAll(tx, fn)` for rotation.
- [ ] Scrubber (spec §3.2): `secret_key`, `client_secret`, `app_secret`,
  `code_verifier`, `refresh_token` keys; `access_token=`,
  `client_secret=`, `password=` in query strings and form bodies; upstream
  `"code":` values left alone. Tests with each shape.
- [ ] Migration test from a v12 fixture.

### Task C4: outbound requests to owner-set addresses

**Files:** `internal/netguard/{dialer,policy}.go`, `internal/config`
(`PRIVATE_DESTINATIONS`), `internal/accounts/state.go` (host client),
tests.

- [ ] `netguard.Policy{PrivateCIDRs []netip.Prefix, PrivateHosts
  []string}` from `PRIVATE_DESTINATIONS`.
- [ ] `netguard.Dialer(policy)`: a `net.Dialer` whose `Control` checks the
  connected address: never loopback, link-local (incl. `169.254.169.254`),
  unspecified, multicast, or the server's own listen addresses; private
  ranges (RFC 1918, ULA, CGNAT) only when allowed by host name or CIDR.
  Used by a transport with no proxy from the environment, redirects capped
  at 5 (each redirect dials again, so it is checked again).
- [ ] `netguard.OwnerURL(raw)`: https (or http for an allowed private
  host), no userinfo, query or fragment, normalized origin (lower-case
  host, default port dropped, IDN to ASCII).
- [ ] `AccountHost.HTTPClient(NetOwner)` returns the guarded client;
  `NetTrusted` stays as today for env-configured addresses.
- [ ] Tests: each blocked range; DNS rebinding (a resolver returning a
  public then a private address: the dial is refused); a redirect to a
  blocked address; allowlisted private host works.

### Task C5: field specs and auth methods in the contract

**Files:** `internal/destination/fields.go`, manifests in each driver,
tests.

- [ ] `AuthMethod`, `FlowKind`, `FieldSpec` (spec §4.1) plus
  `Revealable bool` on secret fields (only the Nostr `secret_key`);
  `EnumValue{Value, Label}`; `Manifest.Auth`, `Config`, `InstanceConfig`.
- [ ] `ValidateFields(specs, values) (normalized map[string]any,
  []FieldError)`: types, required, pattern (RE2, whole value), enum,
  `Min`/`Max`, `MaxLength` (default 4096 for strings, 64 KiB for text),
  unknown keys refused, URL fields through `netguard.OwnerURL`.
- [ ] Manifests per spec §5.7. The Nostr config defaults offered for a new
  account are the env values when set, else a neutral list of public relays
  (never this instance's own relay as a default for others).
- [ ] Contract test: no `Secret` field in `Config`; every `Audience`
  field is a URL; every method's fields validate their own defaults.

### Task C6: database accounts in the registry; key status; rotation

**Files:** `internal/accounts/{registry,state,keys}.go`,
`cmd/publisher/main.go`, `internal/health`, `internal/api/v1_resources.go`,
tests.

- [ ] Startup order: open store → keyring → rotation (if a previous key
  is set: reseal every value sealed with it in `secrets` — and in
  `oauth_flows` from E on — in one transaction; any failure changes
  nothing and logs) → registry load.
- [ ] Key status: `none` (no key, no database secrets), `ok`, `missing` (a
  database secret exists, no key), `mismatch` (secrets under a key id that
  is neither current nor previous). Accounts whose secrets don't open are
  `locked` with the reason; writes of any secret are refused
  (`409 master_key_required` / `master_key_mismatch`); `/readyz` gets a
  non-critical `secrets` check; `/v1/instance` gets `secrets: {status}`;
  startup logs an error naming the cause.
- [ ] Database accounts open with their config and decrypted secrets; the
  `SecretBackend` for them is the secrets store (scope `account`, ref =
  account id, audience = the account's audience field's origin).
- [ ] Env accounts with a key present: `SaveSecrets` goes to `secrets`.
  The Threads token in `threads_token` moves into `secrets` at startup
  once a key exists: overwrite the row's token with zeros, delete it,
  `PRAGMA wal_checkpoint(TRUNCATE)`; logged with a note to take a fresh
  backup.
- [ ] Tests: each key status; rotation success and rollback; locked
  account's due item waits (A8's behaviour) and goes out after the right
  key is set and the registry reloads; Threads token move.

### Task C7: Bluesky PDS discovery

**Files:** `internal/bluesky/discover.go`, tests.

- [ ] Handle → DID: DNS TXT `_atproto.<handle>` (`did=…`), else `GET
  https://<handle>/.well-known/atproto-did` through the host's guarded
  client; DID → document: `PLC_DIRECTORY_URL/<did>` or `did:web`
  (`https://<host>/.well-known/did.json`, guarded); `#atproto_pds` service
  endpoint, validated with `OwnerURL`. An identifier that is a DID skips
  the first step; an email skips discovery and uses the `pds_url` field
  (required then).
- [ ] Tests against fake DNS and `httptest` servers, including a document
  pointing the PDS at a private address (refused).

### Task C8: the account write API

**Files:** `internal/api/v1_accounts.go` (new), `v1.go` (scopes and
routes), `internal/accounts/write.go`, OpenAPI, `errors.md`, tests.

- [ ] Scopes `settings:read`, `settings:write`, `instance:admin` added
  (used from D); `admin` implies none of the step-4 scopes (A12's rule
  extended). Token creation with `accounts:write`, `settings:write` or
  `instance:admin` needs `token.create_privileged` re-authentication.
- [ ] `POST /v1/accounts` (spec §5.2), with `Idempotency-Key`; steps in
  order; rejected credentials → `422 credentials_rejected` (at most 200
  characters of the network's parsed message, control characters
  removed); unreachable → `502 upstream_failed`; `409 duplicate_account`;
  the row and secrets in one transaction; `default_selected = 0`; event
  `account.created`. The Bluesky password-shape warning in the response.
- [ ] `GET` additions (spec §5.3): `auth_method`, `credentials` (set and
  updated at, never values), `config`, `health`, `locked_fields`;
  `?include=deleted`.
- [ ] `PATCH` additions (spec §5.4): `config`, `credentials` for database
  accounts (`409 locked_by_env` for env accounts); re-validated, reopened,
  re-identified; `409 identity_changed`; audience change needs the
  credentials (`422 credentials_required`).
- [ ] `DELETE` (spec §5.5, `account.delete` re-authentication): soft
  delete, secrets deleted, scheduled targets canceled with the reason in
  one transaction; registry status `deleted`; env accounts refused.
- [ ] `POST /v1/accounts/{id}:check` (authenticated Check, one per 10 s
  per account), `:reveal` (session, `account.reveal`, revealable secrets
  only, `Cache-Control: no-store`), `:adopt` (session, master key; spec
  §5.6; seeding skips a `(type, env_key)` whose row is now `db`; a
  startup warning names variables still set for an adopted account).
- [ ] Proxy mode: every credential write, adopt, reveal and connect answers
  `403 sign_in_required`; labels and flags still work.
- [ ] Rate limit: 10 creates or credential changes per minute per owner
  (in-memory token bucket), `429` with `Retry-After`.
- [ ] Events record principal, token id and changed field names only.
- [ ] Tests: each route, scope and mode refusal; idempotent create replay;
  the duplicate race through the unique index; audience change without and
  with credentials; secrets never in any response, event or log (a test
  greps captured logs and events for the plaintext).

### Task C9: the Settings tab, Accounts section

**Files:** `internal/web/assets/settings.js` (new), `fields.js` (new: a
form renderer from `FieldSpec`s), `accounts.js`, `access.js`,
`index.html`, `main.js`, `app.css`.

- [ ] A Settings tab with Accounts and Access sections (Access moves from
  Tools). D adds the other sections.
- [ ] Accounts: list grouped by network with status, health, handle,
  flags; add account (pick network → method → form from the manifest);
  Nostr "generate a key" then reveal once with "I've saved it"; check;
  adopt; enable/disable; default selection; public feed; rename; delete
  (re-authentication); change credentials.
- [ ] Banners: master key missing or mismatched (with the fix), proxy mode
  ("sign in to manage credentials").
- [ ] First-run step 3 (spec §8.5: add a first account).

### Task C10: e2e, docs, PR

- [ ] The harness runs with a master key; its fake drivers declare the
  real manifests' auth methods; the fake `Open`/`Identity` accept
  credentials by a simple rule (a password `bad` is rejected) and return a
  remote id derived from the identifier.
- [ ] `account-settings.spec.ts`: add Bluesky by app password, Mastodon by
  token, Nostr by generating a key (revealed once); duplicate refused;
  rejected credentials shown on the field; rename, disable, delete (with
  re-authentication); the new account is in the composer but not selected
  by default.
- [ ] README: the master key (Docker secret file preferred), adding
  accounts, `PRIVATE_DESTINATIONS`, what encryption protects (spec §3.4),
  upgrade notes for C.
- [ ] As-built notes; open PR C; 2 QA + 1 security reviews; fix; hand over.

---

# PR D — Settings, account sets, media stores

Spec §1 (precedence), §7, §8.5 steps 2 and 4, §12 (D).

### Task D1: migration v14

- [ ] `settings`, `accounts.defaults_json`, `account_sets` (spec §7.1);
  migration test from a v13 fixture; golden schema.

### Task D2: the settings service

**Files:** new `internal/settings/{defs,service,patch}.go`, tests.

- [ ] Definitions (spec §7.2) in code: key, scope (`owner`/`instance`),
  type, default, env variable, secret, audience, validation, and the
  driver `InstanceConfig` fields as `<type>.<key>` (read from manifests,
  no type names in the package).
- [ ] Precedence: non-empty env, then saved, then default; each value
  carries its `source`; env-set keys are locked. Env values are parsed with
  the same rules as config today (invalid → startup error naming the
  variable).
- [ ] `Current()` returns an immutable snapshot (atomic pointer);
  `OnChange(fn)`; writes go through one mutex and one transaction.
- [ ] Merge patch (RFC 7396) over the nested view; `null` reverts or
  clears; validation per definition; secrets through the secrets store
  (scope `setting`), audience-bound (spec §3.3: a webhook URL is itself a
  secret, and its token is bound to the URL's origin).
- [ ] Tests: precedence, locks, merge patch edge cases, secret settings
  never in snapshots' JSON, audience change without the token.

### Task D3: consumers apply settings live

**Files:** `cmd/publisher/main.go`, `internal/dispatch` (policy and
grace via a settings reader), `internal/notify`, `internal/feed`,
`internal/translate`, `internal/api` (languages, feed token), tests.

- [ ] The translator, feed webhook, alert webhook, retry policy, schedule
  grace, user languages and public feed token read `settings.Current()` at
  use or rebuild on change; nothing in flight is interrupted.
- [ ] `publishing.auto_retry` off: no new automatic retries; items waiting
  for a retry aren't claimed (claim filter) and resume when turned on.
- [ ] Tests: a change applies to the next claim or request; turning retries
  off and on with a waiting item.

### Task D4: settings and account-set API; per-account defaults

**Files:** `internal/api/v1_settings.go`, `v1_account_sets.go`,
`v1_accounts.go`, `v1_create.go`, OpenAPI, `errors.md`, tests.

- [ ] `GET`/`PATCH /v1/settings` (spec §7.4), `If-Match` optional,
  `412 precondition_failed`, `409 locked_by_env`;
  `POST /v1/settings/{key}:copy-from-env`.
- [ ] `GET`/`PATCH /v1/instance/settings` (`instance:admin`; secret writes
  need `instance.secrets` re-authentication; proxy mode refuses secret
  writes).
- [ ] `POST /v1/settings/{group}:test` (`alerts`, `feed`) and
  `/v1/instance/settings/media:test`: `{ok, error: class}`, one per 10 s.
- [ ] `/v1/account-sets[/{id}]` with ETags; a set naming a deleted
  account drops it on read.
- [ ] `PATCH /v1/accounts/{id}` accepts `defaults` (visibility, content
  warning, languages, the type's options), validated against the account's
  capabilities and option specs; precedence target → shared content →
  account default; explicit `""`/`[]` clears; preview lists applied
  defaults; create warnings say "default applied: …".
- [ ] Tests for each.

### Task D5: the media store abstraction and Blossom with its own key

**Files:** `internal/media/{store,blossom}.go`, `internal/api/v1_media.go`,
`internal/dispatch` (Host), tests.

- [ ] `media.Store` (spec §7.6): `Put`, `PutFile`, `Fetch`, `Owns`,
  `Public`; metadata stripping stays in the pipeline in front of every
  store (video posters included).
- [ ] Blossom signs with `media.blossom_key` (generated or pasted; shown
  as an npub), adding the BUD-11 server tag. Env deployments keep signing
  with the env Nostr key until a key is set (no change for Geoffrey).
- [ ] `media.store` switch applies to new uploads only; old URLs are
  fetched where they live.

### Task D6: the local disk store

**Files:** `internal/media/local.go`, `internal/api/media_serve.go`,
`internal/config` (`MEDIA_DIR`), tests.

- [ ] Objects under `MEDIA_DIR` (default `<data dir>/media`, 0700; files
  0600) named by SHA-256; served at `GET /media/<sha256>.<ext>` (strict
  name regex; extension must match the stored type; no path built from
  the URL).
- [ ] Served type allowlist (sniffed JPEG, PNG, GIF, WebP, AVIF, MP4,
  WebM, QuickTime); others as `application/octet-stream` with
  `Content-Disposition: attachment`; `nosniff`, `Content-Security-Policy:
  default-src 'none'; sandbox`, immutable caching, range requests.
- [ ] Free-space floor (1 GiB or 5 % of the volume) and optional quota
  (`media.local_quota`); `507`-mapped `insufficient_storage` problem.
- [ ] `media.public` (needs `PUBLIC_URL`): turning it on runs the
  self-test (fetch a probe through `PUBLIC_URL` with the guarded client,
  require 200 and the exact bytes, no redirect); failure keeps it off with
  the class.
- [ ] URL-transport accounts (Threads, Nostr) with media and no public
  store: `422 media_unsupported` at create/preview.
- [ ] Tests: path traversal attempts, wrong extension, a served SVG/HTML
  goes out as an attachment, floor and quota, self-test pass and fail
  (redirect, 401, wrong bytes).

### Task D7: the Settings tab, all sections

**Files:** `settings.js`, `fields.js`, `compose.js` (account sets),
`app.css`.

- [ ] Sections (spec §7.5): Composing (languages, translation, account
  sets and default set, per-account defaults), Media, Publishing,
  Integrations (feed, alerts, instance configs), System (version,
  readiness, the allowlisted bootstrap/tuning values, key status).
  Every field shows its source; locked fields say which variable sets
  them and offer "copy into the app".
- [ ] Composer: an account-set picker next to the target rows; the default
  set (if any) replaces `default_selected` as the starting selection.
- [ ] First-run steps 2 (master key snippet when none) and 4 (media store).

### Task D8: e2e, docs, PR

- [ ] `settings.spec.ts`: an env-locked setting shows its variable and
  copy-from-env; change languages and see them in the composer; an
  account set selects its accounts; account defaults applied in the
  preview; local media upload served from `/media/…` with the headers;
  public self-test failure shown.
- [ ] README: settings tiers table, local media and the proxy rule that
  exposes only `/media/`, Nostr home-IP warning, upgrade notes for D.
- [ ] As-built notes; open PR D; 2 QA + 1 security reviews; fix; hand over.

---

# PR E — OAuth connect

Spec §4.3 (`Connector`), §6.1–6.3, §12 (E).

### Task E1: verify Threads OAuth

- [ ] From Meta's current documentation: the authorization window host and
  parameters, the code exchange endpoint (short-lived token and user id),
  the long-lived exchange (`th_exchange_token`), redirect URI rules (https,
  exact match, localhost), and whether replying to the account's own
  threads needs `threads_manage_replies`. Record here; update spec §6.3
  only where it differs.

### Task E2: migration v15 and the flow manager

**Files:** `internal/store/migrations.go`, new `internal/connect/flows.go`,
tests.

- [ ] v15: `oauth_flows` and `oauth_clients` (spec §6.1–6.2); migration
  test; golden schema. Rotation (C6) includes `oauth_flows.data_sealed`.
- [ ] Flow manager: state = 32 random bytes, stored hashed, single use,
  10 minutes; PKCE verifier (S256) and driver data sealed; flows bound to
  owner and user; expired flows purged hourly; fixed error codes (spec
  §6.1); 10 starts per minute per owner.

### Task E3: connect routes

**Files:** `internal/api/v1_connect.go`, `internal/api/oauth_callback.go`,
OpenAPI, tests.

- [ ] `POST /v1/accounts:connect`, `POST /v1/accounts/{id}:reconnect`,
  `GET /v1/connect-flows/{id}`, `POST /v1/connect-flows/{id}:choose`.
  Refused without a usable master key and in proxy mode.
- [ ] `GET /oauth/callback/{type}`: completes by state; a session cookie,
  when present, must be the flow's user (`session_mismatch`); renders a
  small page (strict CSP, no upstream text) and links back to
  `/#settings/accounts`.
- [ ] Redirect URI: `PUBLIC_URL/oauth/callback/<type>`, else the starting
  request's origin (which passed the same-origin check).
- [ ] Reconnect must return the same `remote_id` (`identity_changed`).
- [ ] Tests: replayed state, expired flow, wrong session, denied consent,
  choose among candidates.

### Task E4: Mastodon connect

**Files:** `internal/mastodon/connect.go`, tests.

- [ ] Discovery, issuer and endpoint origin checks, PKCE when listed,
  pre-4.3 fallback paths (spec §6.2); app registration cached in
  `oauth_clients` per (issuer, redirect URI) with the secret in `secrets`;
  `invalid_client` re-registers once; `iss` checked when present; token
  exchange; `verify_credentials` identity.
- [ ] Tests against a fake instance: 4.3+ with metadata and PKCE;
  pre-4.3 (metadata 404); issuer mismatch; an endpoint on another origin;
  `iss` mismatch; `invalid_client` then success.

### Task E5: Threads connect with the owner's Meta app

**Files:** `internal/threads/connect.go`, manifest `InstanceConfig`
(`app_id`, `app_secret`), tests.

- [ ] Authorize, exchange, long-lived exchange per E1; identity; the
  account's token state (`token_expires_at`) set so A7's refresher takes
  over; the 90-day grant date shown when Meta reports it.
- [ ] Tests against a fake Graph API, including a token in a query string
  never appearing in logs or errors.

### Task E6: the web app

- [ ] "Connect" buttons per method (opens the authorize URL in a new tab;
  the starting tab polls the flow); candidate picker; reconnect for
  `needs_reconnect` accounts; Settings → Integrations → Threads app showing
  the redirect URI to register and Meta's guide.

### Task E7: e2e, docs, PR

- [ ] The harness serves a fake Mastodon instance and a fake Graph API;
  `connect.spec.ts` connects both and reconnects one.
- [ ] README: connecting Mastodon; creating the Meta app for Threads.
- [ ] As-built notes; open PR E; 2 QA + 1 security reviews; fix; hand over.

---

# PR F — Documentation, export and import

Spec §9, §12 (F).

### Task F1: export

**Files:** `internal/export/{export,wordlist}.go`, `internal/api/v1_export.go`,
tests.

- [ ] Passphrase: 24 words from a 2048-word list with a compatible licence
  (the BIP-39 English list; licence checked and recorded); shown once.
- [ ] File: JSON header (format, version, pinned argon2id parameters,
  salt, nonce) and AES-256-GCM payload with the header as AAD; contents per
  spec §9. Session + `export` re-authentication; `Cache-Control: no-store`.
- [ ] Tests: round trip; header tampering fails; parameters pinned.

### Task F2: import

- [ ] Two steps: `POST /v1/import` returns a summary (every endpoint it
  would set) and a confirmation id; `POST /v1/import:confirm` applies it.
  Refuses other KDF parameters, an owner with database accounts, and
  accounts matching an env account's `remote_id`; re-identifies each
  imported account before marking it connected. Session + `import`
  re-authentication.
- [ ] Tests for each refusal and a full round trip between two databases.

### Task F3: documentation

- [ ] README rewritten around the minimal compose file (one volume, the
  master key as a Docker secret, `PUBLIC_URL`), adding accounts per
  network, the configuration table by tier, backups (database and key
  separately, keys across rotations), upgrading from env-only, the
  security model, `PRIVATE_DESTINATIONS`, `TRUSTED_PROXIES`, local media
  and proxy rules.
- [ ] The env defaults that point at this instance's own relay
  (`NIP65_BOOTSTRAP_RELAY`, `FALLBACK_RELAYS`, `PRIMARY_RELAYS`) become
  neutral for new installs; the upgrade note tells existing deployments
  that relied on the default to set the variable. **Confirm with
  Geoffrey before changing** (it changes his relay if he relies on the
  default).
- [ ] As-built notes; open PR F; 2 QA + 1 security reviews; fix; hand over.
- [ ] Release notes draft for a `vX.Y.Z` tag.

---

## Risks and how the plan handles them

| Risk | Handling |
|---|---|
| Part A touches the engine, API and web app at once | Tasks keep the build green one at a time; the engine change (A8) lands behind the same tests as today plus multi-account cases; e2e runs the whole composer with two accounts per type. |
| A schema or key mistake loses posts | Every migration writes a backup and is tested from a fixture; a missing or wrong key locks accounts and their posts wait (A8, C6), never fail permanently. |
| Lockout after enabling local sign-in | Existing databases default to proxy mode (B3); `setup-url` and `reset-login` recover from the container. |
| Credentials sent somewhere else | Audience binding (C3, C8, D2) and the dial-time network guard (C4). |
| A long-lived PR branch drifts from `main` | Parts are pushed only after the previous merge, rebased onto `main`. |
