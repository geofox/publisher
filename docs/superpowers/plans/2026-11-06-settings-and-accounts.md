# Settings and accounts for self-hosters (roadmap step 4) — Implementation Plan

> **For agentic workers:** Steps use checkbox (`- [ ]`) syntax for tracking.
> Spec: `docs/superpowers/specs/2026-11-06-settings-and-accounts-design.md`
> (cited below as "spec §n").

**Status:** plan v2, revised after three reviews (fit with the code,
security, completeness and testing), 2026-11-06.

**Goal:** any number of accounts per network; sign-in without an OIDC
provider; credentials entered in the web app and encrypted at rest; settings
in the app with env precedence; OAuth connect for Mastodon and Threads; a
self-hosting story that needs one volume, one key and an https address.

## Review history

The first plan was reviewed for fit with the code, for security, and for
completeness and testing. The main changes in this version:

- **Part A is three PRs** (A1 contract and drivers, A2 engine and API, A3
  web app). One PR touching the drivers, engine, API, web app and harness
  would be 6–10k lines, too large to review well. Part A can't create a
  second account of a type in production anyway (database accounts arrive
  in C), so the multi-account composer can follow the engine without anyone
  missing it. Parts B–F stay one PR each: eight PRs in all.
- **Every commit builds.** New APIs are added next to old ones under new
  names, one "switch" commit moves the callers, and a cleanup commit
  deletes the old code.
- **Reliability fixes:** the Threads token keeps its expiry on upgrade; a
  locked account's posts wait without using up retries; one 401 no longer
  parks an account until restart (only a confirming authenticated check
  does, and a later good check clears it); seeding never overrides the
  owner's `enabled`; pending fan-out rows get their account; nil-media
  traps closed.
- **Security fixes:** a real auth mode replaces "OIDC on or off" in every
  gate; the network guard reaches every driver transport (HTTP and
  WebSocket); redirects never carry credentials across origins; the
  idempotency store never keeps a secret or a capability; logs are scrubbed
  at the handler; OIDC re-authentication refuses a missing `auth_time`;
  ceremonies are bound to their purpose and session; OAuth redirect URIs
  never come from a request header.

**Deviations from the spec** made by this plan are listed in
§"Spec deviations" at the end; the spec is updated with them.

## Delivery

**PRs, in order:** A1, A2, A3, B, C, D, E, F. Each is deployable on its
own. One commit per task unless a task says otherwise; a small bug found on
the way gets its own commit in the PR where it is found.

**Branch and PR flow.** All work is on `claude/project-thread-24i2fi`. A PR
is opened from it when its part is built and reviewed. While a PR waits for
its merge, the next part is built locally on top of it and pushed only after
the merge, rebased onto the new `main`, so each PR shows only its own part.
Stacked PRs open at the same time would need extra branches, which Geoffrey
would have to allow.

**Per-PR loop (Geoffrey's workflow).** Build → open the PR → two QA review
agents and one security review agent on the PR's diff → fix every finding
with a regression test → push → hand over. Then the next part starts without
waiting for input. Each PR body starts with the attribution block and ends
with the Claude Code footer; `geofox` is assignee and reviewer; PR activity
is subscribed.

**Verification rule for every commit:** `go build ./... && go vet ./... &&
go test -race ./...` green (this includes `e2e/harness`, which is in the root
module). Before a PR is opened also: `npm test` in `e2e/` green, the schema
golden test green (`-update` only for intended changes), the OpenAPI
conformance test green, the drivers guard test green.

**Every PR also:**

- adds its user-visible changes to `CHANGELOG.md` under "Unreleased"
  (created in A1), since `main` publishes the `edge` image;
- updates `docs/api/openapi.yaml`, `docs/api/errors.md` and the README
  where it changes them;
- appends "as built" notes and its spec deviations to this plan;
- for a migration: says in the PR body how to rehearse it on a copy of the
  production database (run the new image against a copy with the same env,
  then check `/readyz?external=1`, the composer and History).

**Conventions (from steps 1–3):**

- Migrations are append-only; a table rebuild uses `noForeignKeys: true`;
  every migration is tested from a fixture of the previous version; the
  store writes `.pre-vN.bak` before applying.
- Core packages never name a destination type in a string literal (the
  guard test); `internal/drivers` is the one place that does.
- Problem details for every `/v1` error; every route is in `v1Routes()` and
  the OpenAPI document; one code for "too many requests" everywhere:
  `rate_limited` with `Retry-After`.
- Secrets never in logs, events, attempts, traces, error messages or stored
  idempotency responses.

**Migration numbers:** A2 v11, B v12, C v13, D v14, E v15.

---

# PR A1 — Contract v2 and drivers

Spec §1 (strict booleans, Nostr env rules), §4.2, §4.3. No migration. No
visible change except strict booleans and the clearer env errors.

### Task A1.1: strict booleans, CI race detector, changelog

**Files:** `internal/config/config.go`, `config_test.go`,
`.github/workflows/ci.yml`, `publish.yml`, new `CHANGELOG.md`.

- [x] `getBool(k, d) (bool, error)`: `1/true/yes/on`, `0/false/no/off` in
  any case, spaces trimmed; anything else is an error naming the variable
  and the accepted values. Call sites: `AUTO_RETRY_ENABLED`,
  `NOSTR_PRIMARY_FANOUT`, `OIDC_END_SESSION`.
- [x] `config.Load` collects every invalid variable and returns them all
  at once (`errors.Join`), so one restart fixes them all.
- [x] CI runs `go test -race ./...`.
- [x] `CHANGELOG.md` with an "Unreleased" section.
- [x] Tests: each spelling; empty takes the default; `flase` fails with the
  variable's name; two invalid variables are both reported.

### Task A1.2: contract v2 and `Open` for every driver

One commit, because adding `Open` to `Driver` breaks every driver until
they implement it.

**Files:** `internal/destination/{destination,registry}.go`, new
`internal/destination/{open,health,source}.go`, new
`internal/destination/destinationtest/host.go`, the four driver packages,
`internal/drivers/drivers.go`, tests.

- [x] `ContractVersion = 2`; manifests say `Contract: 2`; `Register`
  panics on contract 1 (test).
- [x] `Driver.Open(ctx, OpenRequest) (Account, error)`;
  `OpenRequest{AccountID, Method, Config map[string]any, Secrets
  map[string]string, Tuning, Host AccountHost}`. No network I/O in `Open`.
- [x] `Tuning{PublishTimeout, POWTimeout, RelayCacheTTL, VerifyTimeout
  time.Duration; PLCDirectoryURL string}`.
- [x] `AccountHost`:
  - `Save(ctx, StateUpdate{Secrets map[string]string; Config
    map[string]any}) error` — one atomic, durable write of new secrets and
    config together (a token refresh writes the token and its expiry in one
    transaction). Replaces the spec's separate `SaveSecrets`/`SaveConfig`.
  - `HTTPClient(NetPolicy) *http.Client` and `Dialer(NetPolicy)
    destination.DialFunc` (for WebSocket connections).
  - `Logger() *slog.Logger`.
  - `NetPolicy`: `NetTrusted` (env-configured addresses) and `NetOwner`
    (owner-typed addresses). The host may tighten a policy, never loosen it
    (C4 makes every database account's transports `NetOwner` whatever the
    driver asks).
- [x] **Drivers take every transport from the host.** go-mastodon's
  `http.Client`, Bluesky's API and upload clients, Threads' client, and the
  Nostr relay pool's WebSocket dialer come from `Host.HTTPClient` /
  `Host.Dialer`. First step: confirm the Nostr library's pool and
  `RelayConnect` accept a custom dialer or HTTP client; if not, wrap them
  (record which here). With `NetTrusted` the host returns today's clients,
  so nothing changes in A1.
- [x] `EnvSource{FromEnv(getenv) ([]EnvAccount, error); EnvVars()
  []string}`; `EnvAccount{Key, Label, Method, Config, Secrets}`.
  `EnvVars` names the variables, so a `not_configured` reason can say
  "set MASTODON_TOKEN and MASTODON_BASE_URL".
- [x] `Identity.RemoteID`; `Checker`, `CheckRequest{Authenticated}`,
  `Health{OK, Reachable, AuthOK *bool, Message, ExpiresAt *time.Time}`;
  `Refresher{NextRefresh(now) time.Time; Refresh(ctx) error}`;
  `SourceResolver{ResolveSource(ctx, input) (ResolvedSource, error)}`
  with `ResolvedSource{Ref SourceRef; Preview SourcePreview; Actions
  SourceActions}` carrying what `resolve.Preview` and `resolve.Caps` carry
  today, so `/v1/resolve` keeps its shape.
- [x] `destination.TypesInOrder()`: registration order. `drivers.Register`
  registers Bluesky, Mastodon, Threads, Nostr (the composer's order today);
  `Types()` stays sorted for existing callers.
- [x] Each driver's `Open` checks required config and secrets (`invalid`
  naming the field) and builds today's client from them. The old
  constructors stay until A2's cleanup.
- [x] `destinationtest.Host`: in-memory `AccountHost` for driver tests;
  its transports fail the test on any request unless the test installs a
  handler.
- [x] Tests: `Open` never dials; missing fields → `invalid`; JSON round
  trips of the new types; registration order.

### Task A1.3: drivers read their own env

**Files:** each driver's new `env.go` and tests,
`internal/drivers/env.go`, `internal/config`, `cmd/publisher/main.go`.

- [x] **Nostr:** `NSEC_HEX` → one account (`default`); `OWNER_PUBKEY`
  optional (derived), must match when set; `OWNER_PUBKEY` without
  `NSEC_HEX` is an error. Config from `NIP65_BOOTSTRAP_RELAY`,
  `FALLBACK_RELAYS`, `PRIMARY_RELAYS`, `NOSTR_PRIMARY_FANOUT` (strict),
  `POW_DIFFICULTY_DEFAULT`, `POW_DIFFICULTY_MAX`, and `SYNC_RELAYS` as
  `sync_relays_seed`; today's defaults unchanged. Secret `secret_key`.
- [x] **Bluesky:** `BLUESKY_APP_PASSWORD` → one account;
  `BLUESKY_IDENTIFIER` required with it; `BLUESKY_PDS_URL` as `pds_url`.
- [x] **Mastodon:** `MASTODON_TOKEN` → one account; `MASTODON_BASE_URL`
  required with it (today a token without a base URL fails every send).
- [x] **Threads:** `THREADS_ACCESS_TOKEN` → one account; `THREADS_USER_ID`
  as `user_id` (default `me`).
- [x] `drivers.EnvAccounts(getenv) ([]TypedEnvAccount, error)` calls every
  registered `EnvSource` and joins all errors. `drivers.EnvMediaSigner(
  accounts)` returns the env Nostr key pair for Blossom upload signing (or
  none), so `main.go` doesn't name the type.
- [x] `config.Config` loses the destination fields; their tests move to
  the drivers, case for case.
- [x] `main.go` builds today's `map[type]Account` from `EnvAccounts` and
  `Driver.Open` with a legacy host (today's clients). `NSEC_HEX` and
  `BLOSSOM_URL` stay required in A1 (A2.6 lifts that). The Threads token
  manager still gets the Threads client (one temporary type assertion in
  `main.go`, removed in A2.3).
- [x] Tests per driver: unset → none; complete → one with the expected
  config and secrets; each partial combination → the named error.

### Task A1.4: identity, checks and source resolution on the accounts

Additive: the old `identity`, `resolve.Service` and probes keep working
until A2.4 switches to these.

**Files:** driver packages (`source.go`, new `check.go`, `identity`
methods), tests.

- [x] `RemoteID`: Bluesky the DID, Mastodon the account URL, Threads the
  user id, Nostr the public key (hex).
- [x] `Check(Authenticated: false)`: Bluesky `GET <pds>/xrpc/_health`,
  Mastodon `GET <instance>/api/v1/instance`, Threads `GET
  https://graph.threads.net/`, Nostr a WebSocket open on the bootstrap
  relay; no credential is sent. `Authenticated: true`: Bluesky
  `getSession`, Mastodon `verify_credentials`, Threads `/me?fields=id`,
  Nostr none (`AuthOK` nil). The Threads account reports `ExpiresAt` from
  its token state.
- [x] `SourceResolver` on Bluesky, Mastodon and Nostr accounts: the
  adapters' code moves into the drivers (Mastodon resolves through its own
  instance). Threads has none.
- [x] Tests: remote ids from recorded responses; unauthenticated checks
  send no credential; a 401 on an authenticated check gives
  `AuthOK=false`; resolver tests moved from `resolve`.

### Task A1.5: docs, PR, review

- [x] README: `MASTODON_BASE_URL` required with `MASTODON_TOKEN`,
  `OWNER_PUBKEY` optional, strict booleans. CHANGELOG.
- [ ] Open PR A1; 2 QA + 1 security reviews; fix; hand over.

### As built (A1)

- **Network access is one value.** `AccountHost.Net(kind)` returns
  `destination.Net{DialContext, Proxy, CheckRedirect}`, with `Transport` and
  `Client` helpers, instead of `HTTPClient` and `Dialer`. Every HTTP client
  and transport the drivers make is built from it: Bluesky's API and upload
  clients, go-mastodon's transport (under the rate-limit and idempotency
  wrappers) and Mastodon's own client, Threads' client, the Nostr account's
  plain HTTP client (its check) and its WebSockets to relays named by
  content. A `Net` without a dialer refuses every connection.
- **The driver says where an address came from; the host decides the
  rules.** The policies are `NetAccount` (the account's own configured
  addresses and its network's fixed API hosts) and `NetContent` (addresses
  from content or other servers, such as relay hints in a pasted nevent),
  not `NetTrusted`/`NetOwner`: a driver can't know who typed an address,
  only the host knows how the account was set up. The env host trusts
  `NetAccount` (`TrustedNet`) and serves `NetContent`, and any kind it
  doesn't know, through `destination.ContentNet(netguard.IsBlockedIP)`:
  public addresses only, checked on the address dialed, no proxy. C4's
  database host guards `NetAccount` too.
- **Transports** speak HTTP/2 with a ping after 30 s of silence (closed
  after 15 s without an answer). go-mastodon's transport and Bluesky's
  upload client were HTTP/1.1 before, and the upload client had no proxy;
  clients that used Go's default transport dial with a 10 s timeout
  instead of 30 s (CHANGELOG).
- **Redirects** (`SafeRedirect`, every `TrustedNet` and `ContentNet`): at
  most 10, never https to http, and a request body is replayed only to the
  first request's host or its subdomains (Go's rule for keeping
  `Authorization`). C4 tightens this for database accounts.
- **Saved state is a patch.** `StateUpdate` merges key by key (absent keys
  kept, `""` or nil deletes); `destination.MergeState` does it, and
  `destinationtest.Host` applies it (`WithState`, `State`). Hosts in A2 and
  C use the same function.
- **The Nostr relay pool can't take the host's network yet.**
  `fiatjaf.com/nostr`'s `Pool.EnsureRelay` always calls `relay.Connect`
  without a client; `Relay.ConnectWithClient` exists. C4 gives the Nostr
  account its own relay map built on `ConnectWithClient`; until then its
  pool's WebSockets dial with the library's defaults. Relays named by
  content already dial through the host's `NetContent`.
- **Closing.** An account holding connections implements `io.Closer`
  (Nostr closes its pool); the registry closes the old account on a lease
  swap (A2.2).
- **Env accounts.** Each driver's `FromEnv` (`env.go`) returns its accounts;
  `drivers.EnvAccounts` joins the drivers' errors, and `main.go` joins
  those with the config's. Credentials are kept exactly as set
  (`EnvSecret`): the Threads token manager knows the env token by its hash,
  and a trimmed token would read as a new one on upgrade. A list variable
  set to spaces is an empty list (as in 1.x). `EnvVars` returns
  `[]EnvVar{Name, Required}`. `NSEC_HEX` must be exactly 64 hex characters
  (`nostr.ParseSecretKey`; errors never contain the value), and a negative
  `POW_DIFFICULTY_*` is a startup error naming the variable.
  `BLUESKY_PDS_URL` is required with `BLUESKY_APP_PASSWORD`, like the
  identifier (a password without it failed every send; C7's discovery can
  relax this). `drivers.OpenEnv` opens them, one per type, on a host that
  refuses `Save` until A2 stores state. `main.go` still type-asserts the
  clients for the identity service, the token manager and readiness; A2.3
  and A2.4 remove that.
- **Config values.** `ConfigString`, `ConfigStrings`, `ConfigInt` (also
  `json.Number`; no text), `ConfigBool` and `ConfigTime` (RFC 3339 or
  `time.Time`); their errors name neither the key (`InvalidField` adds it)
  nor the value.
- **Checks.** `destination.CheckReachable`, `CheckedAuth` and `NetProblem`
  are shared. `AuthOK=false` only for a refusal of the credentials: Bluesky
  401, `AuthenticationRequired`, `AccountTakedown`, `AccountDeactivated`,
  `ExpiredToken`, `InvalidToken` (an expired cached session is replaced once
  first); Mastodon 401, 403 or 422 with Mastodon's JSON error body (a
  proxy's or firewall's page doesn't count); Threads when its error
  classifier says `auth` (Graph codes 190, 102, 10, 200–299). A rate limit
  (429, or Graph codes 4, 17, 32, 613) or an outage leaves it unset, and a
  2xx that isn't JSON is reachable. `Health` documents the rule the
  registry follows: only `AuthOK == true` clears needs_reconnect. Messages
  name the kind of failure, never the request. Threads reports `ExpiresAt`
  from the token manager, which records it on the client
  (`SetTokenExpiry`) until the `Refresher` replaces the manager; while the
  stored expiry is only a seed's provisional one, it reports none.
- **Remote ids.** Mastodon's is the ActivityPub actor id (`uri`, Mastodon
  4.2 and later), else the profile `url`.
- **Source resolution** moved into the drivers with its tests;
  `resolve.AccountAdapter` renames fields for `/v1/resolve`. A network
  without an account resolves to `not_configured`.
- **CI** runs `go test -race -gcflags=all=-d=checkptr=0`: checkptr trips in
  `fiatjaf.com/nostr`'s `writeJSONString` (no data race was found). The
  thread planner's timing test allows ten times longer under the race
  detector.

---

# PR A2 — Engine, store and API by account; Nostr and Blossom optional

Spec §2.1–2.6, §2.8, §4.4 (refresh), §12 (A). Migration v11. The web app
keeps working unchanged (it already sends account ids; API changes are
additive); A3 makes it multi-account.

### Task A2.1: migration v11 and additive store functions

**Files:** `internal/store/migrations.go` (+ `migrations_accounts.go`),
`accounts.go`, `items.go`, `engine.go`, `fanout.go`, new `sync_relays.go`,
`models.go` (Target), new `storetest/` helpers, tests, golden schema, a v10
fixture.

- [ ] **v11:**
  - `accounts.remote_id TEXT`, `default_selected INTEGER NOT NULL
    DEFAULT 1`, `public_feed INTEGER NOT NULL DEFAULT 1`;
  - `UPDATE accounts SET enabled = 1 WHERE source = 'env'` (before step 4
    `enabled = 0` on an env row only ever meant "not configured", which the
    registry now decides);
  - `nostr_sync_relays (account_id, url, added_at, PRIMARY KEY
    (account_id, url))`;
  - repeat v4's `nostr_fanout.item_id` backfill (`EnqueueFanout` never
    wrote it, so every row enqueued since v4 lacks it).
- [ ] `ItemKey` is renamed `LegacyItemKey` at every call site (it stays
  the key of items planned before this change and of the v4 migration). A
  new `AccountItemKey(postID, accountID, ordinal)` is added, used from
  A2.3. A test shows stored keys are read back, never recomputed.
- [ ] `Claim`, `SendingItem`, `Unscheduled` gain `AccountID`.
- [ ] Fan-out: `EnqueueFanout(postID, itemID, signedEvent, relays)` writes
  `item_id` (its one caller passes it); `FanoutJob` gains `ItemID`,
  `AccountID` (`DueFanout` joins `target_items` → `post_targets`; a row
  still without an item id is matched by `post_id` and `ref_json`, else
  has no account).
- [ ] Sync relays per account: `SyncRelays(ctx, accountID)`,
  `AddSyncRelay`, `RemoveSyncRelay`, `SeedSyncRelaysIfEmpty(ctx,
  accountID, urls)`, `AdoptLegacySyncRelays(ctx, accountID)`
  (transactional, idempotent).
- [ ] `store.Account` gains `RemoteID`, `DefaultSelected`, `PublicFeed`;
  `UpdateAccountMeta(ctx, id, AccountMeta)` (nil fields untouched;
  `updated_at` bumped only when a value changes); `SetAccountIdentity(ctx,
  id, handle, remoteID)` (no `updated_at` bump).
- [ ] Targets read by the store carry `AccountPublicFeed`, so
  `feed.Eligible` (feed page and feed webhook alike) can honour it.
- [ ] Test helpers: `storetest.SeedAccount(t, st, typ) string`.
- [ ] Tests: v10 fixture → v11 (columns, defaults, env rows enabled,
  fan-out backfill); `TestLegacyRealisticUpgrade` extended (baseline →
  latest) with a `sending` item with `committing_at`, fan-out rows with
  and without `item_id`, a `threads_token` row and a scheduled post.

### Task A2.2: the account registry

New package, not used yet.

**Files:** new `internal/accounts/{registry,status,state,checks,refresh,identities}.go`,
`internal/drivers/legacy.go`, `internal/threads/refresh.go`, tests.

- [ ] `Registry` (spec §2.1): `Load`, `Get`, `List(owner)`, `OfType`,
  `Acquire(id) (Account, release, error)`, `Caps(ctx, id, typ)`,
  `Reopen`, `Update`, `OnChange`, `ReportAuthFailure(id)`.
- [ ] Inputs injected: the env accounts with their keys (from
  `drivers.EnvAccounts`, or a test list), an `Opener` (default:
  `Driver.Open`), the store, a clock, an alerter.
- [ ] **Order:** `destination.TypesInOrder()`, then `created_at`, then id;
  each entry carries its `Order`.
- [ ] **Status:**
  - `connected`;
  - `disabled` (the owner turned it off);
  - `not_configured` (an env row with no matching env account, reason
    naming `EnvVars`; or a type whose driver isn't installed);
  - `locked` (C; `source = 'db'` rows are `locked` "needs a later
    version" until then);
  - `needs_reconnect` (below);
  - `deleted` (C).
- [ ] **needs_reconnect only when confirmed.** `ReportAuthFailure`
  schedules an authenticated `Check` (at most one per account per 10
  minutes); only `AuthOK=false` sets `needs_reconnect`. A daily
  authenticated check runs for every connected account (and every 15
  minutes for one in `needs_reconnect`); a passing check clears it, with
  no restart. A permission error on one reply therefore never parks the
  account.
- [ ] **Leases:** reference-counted live objects; `Reopen` swaps and
  closes the old object (`io.Closer`) on its last release; `Acquire` on
  an unavailable account returns `*accounts.Unavailable{Status, Reason}`.
- [ ] **Caps:** the open account's, else the type's manifest defaults.
- [ ] **Update** keeps the object and emits a `Change`; identity writes
  don't.
- [ ] **State for env accounts** (`drivers.LegacyState`, the one place
  naming the Threads type): Threads state stays whole in `threads_token`
  (token, `expires_at`, `refreshed_at`, `seed_hash`) and `Host.Save`
  writes all four in one upsert; `Load` hands the token as the secret and
  the expiry and refresh time as config to `Open` when the seed hash
  matches the current env token, else today's provisional rule (expiry =
  now + 11 days) applies. Other types have no persisted state in A
  (`Save` returns "not supported before part C").
- [ ] **Threads refresher** (`internal/threads/refresh.go`): the account
  implements `Refresher` with today's rules (refresh when at least 24 h
  old and within 10 days of expiry); `Refresh` calls the refresh endpoint
  and `Host.Save`s token and expiry together. `TokenManager`'s tests are
  ported case for case.
- [ ] **Refresh loop:** every minute ±10 %, one account at a time, holding
  a lease, 30 s timeout. Alerts: a refresh failure at most once a day per
  account while the credential works; "expires within 7 days" at most
  once a day; for accounts without a `Refresher`, the same expiry alert
  from `Health.ExpiresAt` (spec §4.3). The Threads expiry gauge reports the
  soonest expiry.
- [ ] **Identities** (replaces `internal/identity`): per account, cached
  10 minutes (6 s timeout, failures cached 1 minute), refreshed in the
  background; readers get the stored handle and the last profile
  immediately and never wait on the network. Handle and remote id are
  written with `SetAccountIdentity`; a changed remote id on an env account
  is logged as a warning.
- [ ] Tests (`-race`): lease swap; concurrent Acquire/Reopen; Update keeps
  the object; Caps fallback; order; a 403 on a send followed by a passing
  check stays `connected`; a failing check sets `needs_reconnect`, a later
  passing one clears it; Threads state round trip, seed-hash reset,
  provisional expiry; a v10 database whose token expires in 5 days is
  refreshed on the first loop.

### Task A2.3: the switch

One commit: every caller moves to the registry and account-keyed specs.

**Files:** `internal/dispatch/*`, `internal/api/*` (adapters only),
`cmd/publisher/main.go`, `internal/store/{accounts,models}.go`,
`e2e/harness/*`, test helpers.

- [ ] **Dispatcher:** `Accounts`, `AccountIDs`, `SendsAs`,
  `errOtherAccount`, `ConnectedIDs` go; `Dispatcher.Registry
  AccountSource` (`Acquire`, `Caps`, `Get`, `List`, `ReportAuthFailure`).
- [ ] **Specs:** `PostSpec.Targets []TargetSpec{AccountID, Type, Text,
  Overrides, Layout}` replaces `Platforms`, `Overrides`, `Layouts`,
  `Accounts`; `Overrides.Text` goes. `InteractSpec` gets `SourceAccount`
  and `Targets`; `SourcePlatform` stays as the source's type.
  `interaction_json` stores `source_account`; matching a target to the
  interaction (`interact.go:164`) uses it, falling back to the type for
  rows that predate it.
- [ ] **Every lookup by type** becomes by account id, including
  `planTarget` (`plan.go:44`) and repost planning (`plan.go:323`), which
  use `AccountItemKey`; `planPostCards`; `publishItem`; `repost`;
  recovery (`engine.go:343,484,693,828` call `Caps(ctx, accountID,
  type)`); `interact.go:127`; `post.go:35`; `retry.go:54-57`;
  `publish.go:46-119,165,244`; `fanout.go:110-116`; `CanLocate`/`Locate`
  (`api/v1_posts.go:729` passes the target's account); the API's `a.Caps`
  (`api.go:146,221`).
- [ ] **Unavailable accounts at send time (spec §2.2):**
  - `locked`: the item is recorded as not sent (`ItemResult.NotSent`, as
    `engine.go:350` does) with `next_attempt_at` pushed by the backoff
    (capped at 15 minutes). It doesn't count as an attempt, never gives
    up, and is claimed even with automatic retries off, so it goes out as
    soon as the account opens.
  - `needs_reconnect`, `disabled`, `not_configured`: fail as `auth` with
    the reason.
  - An `auth` failure from the network calls `ReportAuthFailure`.
- [ ] **Fan-out:** the worker always runs; a job leases its account, else
  any connected `Redeliverer` of the same type, else waits without
  counting a failure. The retrier passes the target id
  (`retrier.go:127`).
- [ ] **Store:** `SeedEnvAccounts(ctx, owner, []EnvAccount{Type, Key,
  Label})` creates missing env rows (label only then), never writes
  `enabled` on existing rows, never touches `source != 'env'` rows, and
  returns every env row. `SavePost` refuses an empty `AccountID`;
  `envAccountFor` goes. New items use `AccountItemKey`.
- [ ] **`main.go`:** `drivers.EnvAccounts` → `SeedEnvAccounts` →
  `drivers.AdoptLegacy` (sync relays to the first env Nostr account, then
  its `sync_relays_seed`) → `Registry.Load` → refresh loop →
  `d.Recover` → workers. The Threads type assertion and `TokenManager` go.
- [ ] **API adapters:** `planCreate` and interactions build `TargetSpec`s
  with today's validation unchanged (A2.5 changes the API).
  `v1_drafts_convert.go` decodes old drafts into a local legacy struct
  (it reads `Overrides.Text`) and keeps mapping each type to the first
  enabled account.
- [ ] **Harness:** registry with an `Opener` that returns the recording
  fakes; one account per type as today plus a `not_configured` row; a
  restart builds a new registry and dispatcher, and the API reaches both
  through the engine wrapper.
- [ ] **Tests:** helpers (`dispatchtest.Registry(map[type]Account)` with
  `acc_<type>` ids, a spec builder); the dispatcher's test-only entry
  points (`Post`, `Schedule`, `Fire`, `Retry(platforms)`, `Interact`) are
  deleted after their tests move to `Enqueue`, if nothing else uses them.
  New: two accounts of a type in one post (own text, layout, caps, keys
  differ); locked account waits through more than `MaxAttempts` backoffs
  and with retries off, then goes out after `Reopen`; disabled fails `auth`
  without a network call; **two accounts of one type each leave an item
  `sending`, a restart recovers both and `Locate` runs on each item's own
  account**; fan-out through its own account, through another when locked,
  waiting when none.

### As built (A2.1–A2.3)

- **Store names.** The per-account relay-sync functions are
  `AccountSyncRelays`, `SetAccountSyncRelays`,
  `SeedAccountSyncRelaysIfEmpty` and `AdoptLegacySyncRelays`; the global
  `SyncRelays`/`SeedSyncRelaysIfEmpty` stay until A2.4 moves relay sync to
  accounts. `InsertAccount(ctx, Account)` was added for tests and the
  harness (a second account of a type, a `not_configured` row); C's write
  API replaces it for real accounts.
- **Test helpers stay in their packages.** `store` has
  `fixtures_test.go` (`saveWithAccounts`, `testAccount`); `dispatch` has
  `registry_test.go` (a `testRegistry` with lease counts, `typeSpec` and
  `typeInteract` builders for the old per-type tests). No `storetest` or
  `dispatchtest` package: nothing outside the package needs them yet.
- **The dispatcher's per-type entry points** (`Post`, `Schedule`, `Fire`,
  `Retry(types)`, `Interact`, `RetryRelay`) moved to `registry_test.go`
  as test-only wrappers over `Enqueue` instead of rewriting about 150
  tests; production code no longer has them.
- **Registry status lives in memory.** It is recomputed on `Load` and by
  checks; nothing new is stored. `destination.Expirer`
  (`CredentialsExpireAt`) is how the refresh loop and the expiry alert
  read an account's expiry; the Threads account implements it.
- **A `Restore` error makes the account `locked`** ("its saved state could
  not be read; it is retried at the next start") rather than failing the
  start or opening with the environment's token, which could be older than
  the refreshed one.
- **Locked items wait `clamp(time since the post was due, 1 min, 15
  min)`** rather than a per-item backoff: no attempt is counted, so the
  backoff has nothing to grow from, and the post's age gives the same
  shape. The item stays `planned` with `not_sent`, class `transient`, the
  error "the account is locked: <reason>; it is sent once the account
  opens", no settled step and no alert. Part C should make a locked
  account's items due at once when it unlocks.
- **`InteractSpec.Targets` includes the source account** (the one that
  acts natively) with every copy; `split()` separates them and forces the
  source target's type to `SourcePlatform`.
- **The API checks accounts up front** (`accountUnusable`, pulled forward
  from A2.5): a target whose account isn't loaded or can't send is refused
  with `account_disabled` and the registry's reason, rather than being
  accepted and failing as `auth`. `duplicate_type` stays until A2.5.
- **Fan-out and relay retry** use the job's account, else the first
  connected `Redeliverer` of the type, else wait; `NewFanout` lost its
  `enabled` argument.
- **Startup** (`main.go`): env accounts → `SeedEnvAccounts` →
  `AdoptLegacy` (an error is logged, not fatal: the lists can be fixed in
  the app) → `Registry.Load` → refresh and check loops → recovery →
  workers. Until A2.4 the interim identity, resolve and relay-sync services
  lease the first connected account of their type; startup still fails if
  the env Nostr account can't open (A2.6 makes Nostr optional).
- **Known follow-ups:** `internal/web/assets/accounts.js` picks the first
  `enabled` account per type; since v11 every env row is enabled, so A2.5
  must expose `status` and A3 (or A2.5) pick the first `connected` one.
  `TokenManager` and `drivers.OpenEnv` are unused in production (A2.7).

### Task A2.4: services per account

**Files:** delete `internal/identity`, `resolve.Service` and adapters;
`internal/health`, `internal/relaysync`, `internal/feed`,
`internal/api/{v1_resources,v1_tools,v1_interact}.go`, `internal/nostr`
(`RelayLister`), `cmd/publisher/main.go`, `internal/drivers/guard_test.go`.

- [ ] Identity from `accounts.Identities` (A2.2).
- [ ] **Readiness:** `health.Checker.External` is a provider evaluated per
  probe (results cached); it returns `account:<id>` per enabled account
  implementing `Checker` (unauthenticated) and `blossom` when set. The
  separate `nostr_relay` check goes (each Nostr account's check probes its
  bootstrap relay).
- [ ] **Resolve:** `/v1/resolve` and interactions resolve with an
  account's `SourceResolver` (A2.5 adds the `account` input);
  `resolve.Detect` stays.
- [ ] **Relay sync:** `relaysync.Sync` takes home relay and public key per
  call; the `/v1/accounts/{id}/nostr/relays*` handlers lease the path's
  account and assert `nostr.RelayLister`; another type → 404. Lists come
  from `nostr_sync_relays`. `np` and `HomeRelay` leave the API;
  `features.relay_sync` = a Nostr account is connected.
- [ ] **Feed:** `feed.Link` gains `account` and `handle`; `Eligible`
  drops targets of accounts with `public_feed = 0` (feed and webhook).
- [ ] Guard test covers `cmd/publisher` and `internal/accounts`, and
  forbids them importing a driver package (only `internal/drivers` may).
- [ ] Tests: readiness names; relay handlers per account; feed flag in
  page and webhook.

### As built (A2.4)

- **The API reaches accounts through one interface,** `api.AccountRegistry`
  (`Get`, `List`, `Acquire`, `Profile`; the registry, or the harness's
  engine wrapper). `api.New(mp)`; the `np`, `HomeRelay`, `Identity` and
  `Resolve` fields are gone.
- **Profiles** come from `Registry.Profile` for every account (two of a
  network included); `main.go` calls `RefreshProfiles` at startup. An
  account the owner turned off isn't asked for its profile (its
  credentials aren't used while off), matching the checks.
  `internal/identity` is deleted.
- **Readiness glue lives in the registry** (`Probeable`, `Probe`,
  `Readiness(owner)`), so it is tested there; `health.Checker` gained
  `MoreExternal func() []Check`, asked at each refresh of the external
  results. A probe failure is classed `unreachable`, `timeout` or `not ok`;
  the network's message only reaches the log. `health.NostrRelay` is gone.
  `blossom` is listed only when `BLOSSOM_URL` is set.
- **Looking posts up:** `resolve` keeps the types, `Detect` (now exported,
  trims space) and `FromResolved`; `Service` and the adapter are deleted.
  The API looks a post up with the first connected account of the detected
  type, and an interaction prefers its own target account on that network
  (pulled forward from A2.5: a Mastodon status id is only valid on its
  own server). `ErrThreadsUnsupported` became `ErrUnsupported`, its message
  built from the driver's name. Behaviour change: with every account of
  the post's network turned off, `/v1/resolve` answers `not_configured`
  ("none of your accounts on that post's network is connected") instead
  of a preview with nothing allowed, since looking up uses the account.
- **Relay sync** works for any of the owner's Nostr accounts (not only the
  env default): `relaysync.Sync` takes a `relaysync.Owner{Home, Pubkey}`
  per call; `nostr.RelayLister` is implemented by the Nostr account; lists
  are `AccountSyncRelays`/`SetAccountSyncRelays`; an unusable account is
  `409` with the registry's reason; another owner's account is `404`
  without being leased. `features.relay_sync` is true while a Nostr
  account of the caller's is connected.
- **Feed** links carry `account` and `handle` (OpenAPI updated).
- **Guard tests:** `cmd/publisher` joins the string-literal check; a new
  test forbids `cmd/publisher` and `internal/accounts` importing a driver
  package (found by reflection on the registered drivers, so a new driver
  is covered without editing the test). `main.go` no longer imports any
  driver package.
- The startup log no longer prints the Nostr key and bootstrap relay
  (they belong to an account); it prints `blossom_signer`.
- Left for A2.7: the global `sync_relays` store functions (`SyncRelays`,
  `SetSyncRelays`, `AddSyncRelay`, `RemoveSyncRelay`,
  `SeedSyncRelaysIfEmpty`) are now used only by `AdoptLegacy` and tests.

### Task A2.5: the API by account

**Files:** `internal/api/{v1,v1_create,v1_interact,v1_drafts,v1_resources,v1_tokens}.go`,
`docs/api/openapi.yaml`, `docs/api/errors.md`, tests.

- [ ] **Scopes:** add `accounts:write`. `admin` implies the scopes that
  existed before step 4 and none added since (`principal.can`). In A2 only
  a session may create a token holding `accounts:write` (re-authentication
  arrives in B). OpenAPI text "`admin` includes every scope" updated.
- [ ] **Create/preview/drafts:** `duplicate_type` and "isn't connected on
  this server" go; the same account twice keeps today's field code
  `duplicate`; an unavailable account → `account_disabled` with the status
  in the detail; drafts accept it with a warning.
- [ ] **Interactions:** the target of the source's type acts natively
  (exactly one; a second → `one_native_actor`); the source is resolved
  with that account; other targets are link-quote copies.
- [ ] **`/v1/resolve`:** optional `account` input (it already returns
  `account`); default: the first connected resolver account of the
  detected type.
- [ ] **Account views:** `status`, `status_reason`, `remote_id`, `order`,
  `default_selected`, `public_feed`, `profile_url`, `capabilities`; the
  flat `display_name` and `avatar` stay; `ETag` on the single view.
- [ ] **`PATCH /v1/accounts/{id}`** (`accounts:write`; `If-Match`
  optional, `*` allowed, stale → `412`): `label` (1–64 characters, no
  control characters, trimmed), `enabled`, `default_selected`,
  `public_feed`; unknown fields refused; event `account.updated` with
  field names. The conformance test calls it.
- [ ] Tests: two Mastodon accounts in one create; preview with different
  limits; each unavailable status; interactions with two accounts of the
  source network; resolve with `account`; PATCH with ETags and scopes; an
  admin token can't PATCH or mint `accounts:write`.

### As built (A2.5)

- **Account views** carry `status`, `status_reason`, `order`, `remote_id`,
  `profile_url`, `default_selected` and `public_feed`; the handlers moved
  to `v1_accounts.go`. The single view's `ETag` is `updated_at` in Unix
  nanoseconds (which moves only on an owner's change); the store compares
  it inside the update's transaction (`AccountMeta.IfUpdatedAt`,
  `ErrAccountConflict`), so a change landing between read and write is a
  412 even without `If-Match`, as for drafts.
- **`PATCH /v1/accounts/{id}`** decodes each field raw so `null` is
  refused (`invalid`) instead of read as "keep". `api.AccountRegistry`
  gained `Update`. `Registry.Update` now takes only the owner's fields and
  ignores a row older than the one it holds (two PATCHes whose registry
  notices cross), keeping the handle and remote id the registry learnt.
- **`account.updated`** is written by `UpdateAccountMeta` in its
  transaction with `post_id` NULL; data: account, type, field names,
  actor (`session`, `token`, `proxy`) and token id. `/v1/events` sends it
  only to a caller with `accounts:read` (`EventFilter.PostsOnly`); `post`
  became optional in the event schema. Pulled forward from C (spec §10.1).
- **Drafts** accept an unavailable account without a warning field (they
  never had one); the composer shows the account's status when it loads
  the draft (A3).
- **The token form** offers `accounts:write`; `admin` reads "everything
  else".
- **Fixed on the way:** `AdoptLegacy` gave the 2.x relay-sync list to the
  first env Nostr row in the order given, which could be a numbered
  account (same `created_at`, random id); it now goes to the unnumbered
  (`default`) account, 2.x's one account. Its test was order-dependent
  (failed about half the runs).

### Task A2.6: Nostr and Blossom optional

**Files:** `cmd/publisher/main.go` (extract `run(ctx, getenv)`),
`internal/api/{api,v1_media,v1_drafts_convert,v1_interact,v1_resources}.go`,
`internal/media`, `internal/web/assets/compose.js` (attach button), tests,
`.github/workflows/ci.yml`.

- [ ] `NSEC_HEX` not required; `BLOSSOM_URL` optional; set without an env
  Nostr key → startup error saying so (D gives the store its own key).
- [ ] Nil-media traps: no `mp.Lookup` on a nil pipeline; the dispatcher's
  `Fetcher` is a plain media fetcher (never a nil `*Pipeline` inside an
  interface) that still fetches existing posts' media after `BLOSSOM_URL`
  is removed; `Host` nil without media.
- [ ] `api.New(mp)`; nil media → `POST /v1/media`, `:compress` and video
  jobs answer `503 unavailable` "no media store is configured";
  `ConvertOldDrafts` and interaction media re-hosting skip media;
  `features.media`. The composer hides the attach button with a hint.
- [ ] Startup log lists accounts by type and status.
- [ ] Tests: `run` with only `BLUESKY_*` set answers `/readyz` and
  `features.media=false`; CI's Docker smoke run gains a second container
  with no destination variables.

### As built (A2.6)

- **`run(ctx, runOptions{Getenv, Started, Stopping})`** holds what `main`
  did and returns an exit code (2 configuration, 1 failure, 0 after a
  graceful stop); it listens with `net.Listen` so a test can use port 0.
  `config.LoadFrom(getenv)` reads through the given function (`Load` is
  `LoadFrom(os.Getenv)`). Tests in `cmd/publisher`: only `BLUESKY_*` set
  (ready, `features.media=false`, one connected account, upload 503,
  graceful exit 0), nothing set (ready), `BLOSSOM_URL` without a Nostr
  key (exit 2).
- **No media store:** `mp` is nil; `media.NewFetcher()` (a plain fetcher
  split out of `Pipeline.Fetch`) is the dispatcher's `Fetcher` either way;
  `Host` is set only with a store; the video runner is built only with a
  store. `POST /v1/media` (image or video) and `:compress` answer 503 "no
  media store is configured (set BLOSSOM_URL)"; `features.media`, and
  `features.video` now also needs the store. The composer hides the attach
  button and the video preset and says why.
- **Startup log:** accounts grouped by status; one warning per account
  that isn't connected (with its reason), an info line for one turned
  off, and a warning when there is none.
- **CI:** a second Docker smoke run with only `DB_PATH` checks `/readyz`,
  `features.media=false` and the healthcheck (checked locally with the
  binary; this container has no Docker daemon).

### Task A2.7: cleanup

- [x] Delete what the switch left unused: old driver constructors,
  `ConnectedIDs`, `LegacyItemKey` outside the v4 migration and reads, the
  old config plumbing. `go vet` clean and no unused exported helpers left.

### As built (A2.7)

- **Removed:** `threads.TokenManager` (the registry's refresh loop and
  `accounts.RefreshDue`, which also sets `publisher_token_expiry_seconds`,
  do its work; `threads/refresh.go` keeps `SeedProvisional` and
  `SeedHash`); `drivers.OpenEnv`, `errNoSave` and `envHost` (tests open
  env accounts through each driver with `destinationtest.NewHost`); the
  store's global relay-sync functions (`SyncRelays`, `SetSyncRelays`,
  `AddSyncRelay`, `RemoveSyncRelay`, `SeedSyncRelaysIfEmpty`, and the
  unused `AddAccountSyncRelay`/`RemoveAccountSyncRelay`): table
  `sync_relays` is only read by `AdoptLegacySyncRelays`, and tests write
  it with SQL; `config.Load` (callers use `LoadFrom(os.Getenv)`).
- **Moved to tests:** the inline drive (`drive`, `driveTarget`,
  `beginInline`, `endInline`, `notSaved`, `runDue`) to
  `dispatch/inline_test.go`. Production only enqueues; it was reachable
  before only through reflection. The workers still skip a post a test
  drives inline (`inlinePosts`), and `memMedia` stays (tests hand media
  in memory).
- **Kept on purpose:** `bluesky.New`, `mastodon.New`, `threads.New` (test
  conveniences over `Open` with a trusted network, 69 call sites);
  `destination.MergeState`, `OptStrings`, `Unregister` (contract and test
  helpers); `LegacyItemKey` (v4 migration; tests building 2.x items).
  `ConnectedIDs` was already gone. Every `config.Config` field is read.
- `deadcode ./...` now reports a subset of what it reported on `main`.

### Task A2.8: e2e

**Files:** `e2e/harness/*`, `e2e/tests/*`, `e2e/README.md`.

- [x] The harness seeds through an injectable env list with keys: one
  account per type as today (default selected), plus a second Mastodon
  (1000-character limit) and a second Bluesky with `default_selected=0`,
  and the `not_configured` row; existing specs keep their four rows.
- [x] The recorder records the account of each call and accepts
  `?account=` on its controls (`down`, `invalid`, `hold`).
- [x] Fake `SourceResolver`s on the fakes, with URLs `Detect` recognises,
  so interactions can be tested.
- [x] Tests that change accounts restore them in `afterEach`.
- [x] New `api-accounts.spec.ts` (API level, since the composer is still
  per type in A2): create a post to both Mastodon accounts and see two
  sends with their own texts; retry one account's failed item; PATCH a
  label; the `not_configured` account refused with its reason.

### As built (A2.8)

- **`harnessAccounts`** (`e2e/harness/main.go`) is the one table of the
  harness's accounts, seeded with `SeedEnvAccounts` in table order; the
  registry's env list is built from it (an entry with `NotConfigured` is
  stored but left out, so it is `not_configured`). Accounts: one of each
  network (the composer's four rows), "Mastodon (long posts)" (1000
  characters, through the account config key `e2e_max_length`), "Bluesky
  (alt)" (`default_selected` off), "Mastodon (old)" (not configured).
- **`SeedEnvAccounts` keeps the order given:** rows seeded together got
  the same `created_at`, so the registry fell back to the random ids and
  two env accounts of a type came in either order (no driver reads
  numbered variables yet, so only the harness had two). Each row is now
  1 µs after the previous (`TestSeedEnvAccountsKeepsOrder`, which fails
  without the fix).
- **Fakes per account:** `newFake(rec, type, accountID, config)` from the
  registry's `Open`; calls record `account`; `/__e2e/*` controls take
  `account=acc_…` instead of `platform=`; attempts count per platform and
  per account. Bluesky, Mastodon and Nostr fakes implement
  `ResolveSource` (Threads has none, as in production); ids they return
  carry the account's id and the preview names it.
- **Specs:** `api-accounts.spec.ts` (6 tests): list order, status, reason
  and flags; one post to two Mastodon accounts, each with its own text
  and limit; a retry of one account's failed item; PATCH with `If-Match`
  and a stale ETag (label restored in `afterEach`); a reply from the
  account named, with `one_native_actor`; `/v1/resolve` with and without
  `account`, and its refusals. The reliability test of the second account
  now covers the `not_configured` one. Mutation checks: ignoring the
  per-account limit or per-account outages fails two of the tests.
- Not done: the Interact tab's tests still stub `/v1/resolve` in the
  browser (they test the tab's rendering); A3 moves the composer to
  accounts and its e2e then.

### Task A2.9: docs, PR, review

- [ ] README: Nostr and Blossom optional, readiness names `account:<id>`,
  feed `links[]` per account, n8n flows keep working with account ids.
  CHANGELOG and upgrade notes (readiness names, alert texts with handles,
  feed fields).
- [ ] Rehearsal steps in the PR body.
- [ ] Open PR A2; 2 QA + 1 security reviews; fix; hand over.

### As built (A2.9): review fixes

- **Names** (`api.nameProblem`): account labels and token names refuse
  control, format (bidi, zero-width, BOM), separator, private-use,
  surrogate and noncharacter code points, and need one visible
  character; ZWJ/ZWNJ after a visible character pass (emoji, Persian).
- **Env credentials are the operator's:** `envFor` opens an env row with
  the environment's credentials only when its owner is `DefaultOwner`
  (an empty key reads as the unnumbered one); the 2.x Threads token goes
  only to the operator's unnumbered Threads row.
- **Turned off still renews** the Threads token (`refreshOne` runs for
  `connected` and `disabled`).
- **Waits:** a waiting item is an `item.waiting` event (status `planned`,
  the reason); a cancel that lands while it is claimed ends it
  `canceled` (`RecordItemResult` turns a planned result on a canceled
  post into canceled), and the post settles. An item given up unsent
  (`release`) is now an `item.unknown` event; only the restart re-queue
  stays silent.
- **Relay-sync lists** (v11, amended before release):
  `nostr_sync_relay_lists` marks an account whose list exists, even
  emptied, so adopt and seed run once (`SeedAccountSyncRelaysOnce`).
  Part C's account delete must delete its row too.
- **No account for relays:** fan-out jobs that wait are deferred a minute
  (`DeferFanout`, no attempt counted) so they don't hold the head of the
  queue; the fan-out and the retrier log the wait when it starts and then
  every 15 minutes (`lastingWarn`), and once when it ends.
- **Small:** `Acquire` after `Close` is `ErrClosed` (transient, not an
  auth give-up); `displayName` ignores a label that repeats the type in
  any case; the registry no longer logs each unavailable account (the
  start log does); reasons say what to do ("you turned it off; turn it on
  again to use it", "the network refused its credentials: …"); a JSON body
  that isn't an object is `400 invalid_json`.
- **Web:** a new post selects the networks with a connected account that
  new posts select, once, when the accounts first load (never on a
  reload), and `accountFor` prefers that account; the video preset stays
  hidden without a media store; the Tools tab says relay sync needs a
  Nostr account (`features.relay_sync`). `web-account-defaults.spec.ts`.
- **Docs:** `enabled` is only the owner's switch (CHANGELOG Breaking:
  pick targets by `status`), `admin` lacks `accounts:write`, `order -1`,
  the PATCH answer, `invalid_state` for relays, feed `handle` absent.

---

# PR A3 — The composer, History and Interact by account

Spec §2.7, §11 (composer e2e). No migration.

### Task A3.1: accounts and tokens in the web app

**Files:** `accounts.js`, `tokens.js`, `common.js`.

- [ ] `accounts.js` keeps the list in the API's `order` (registration
  order of types, so the composer order is unchanged), and exports
  `accounts()`, `byId`, `ofType`, `connected()`, `defaultTargets()`;
  `accountFor` goes. `ORDER` stays only as a fallback for types the API
  doesn't list.
- [ ] `tokens.js`: `accounts:write` can be granted; the "admin: everything"
  text says what admin covers.

### Task A3.2: state and recovery by account

**Files:** `state.js`, `drafts.js`, `drafts_recovery.js`, new
`snapshot.js` (pure functions), `e2e/unit/snapshot.test.mjs` (run with
`node --test`, no new dependency).

- [ ] `state.targets` (account ids) replaces `state.platforms`; overrides,
  breaks and media positions keyed by account id; option cards by type.
- [ ] Snapshot version 3 keyed by account id, used by both the recovery
  snapshot and `specFromDraft` (they share the format). Conversion from
  version 1 (marker-based) and version 2 (type-keyed, including
  `interaction.platform` and `prevPlatforms`): each type maps to its first
  connected account; unknown keys dropped.
- [ ] A restore waits for accounts to load and survives a failed accounts
  read at boot (today's `regress-web-acc` behaviour).
- [ ] `specFromDraft` keeps every account of a type (`drafts.js:191`
  drops the second today); a draft naming a deleted account drops it with
  a notice.
- [ ] Unit tests for the conversions.

### Task A3.3: the composer by account

**Files:** `compose.js`, `editor.js`, `count.js`, `plan.js`, `preview.js`,
`app.css`.

- [ ] Target rows grouped by network, avatar plus handle. An unavailable
  account shows its reason as visible text (not a tooltip), is
  `aria-disabled` and can't be selected. A per-network "select all" when a
  network has several accounts. An empty state when there are no accounts
  ("add an account in the environment", later "in Settings").
- [ ] Default selection: connected accounts with `default_selected`.
- [ ] Editor tabs and Compare columns one per selected account; labels are
  the short handle, extended with the instance when two collide
  (`@geoffrey@fosstodon.org`); accessible names "Mastodon,
  @geoffrey@fosstodon.org"; `role=tab` with `aria-controls` and arrow
  keys; the tab row scrolls at every width and keeps the active tab
  visible; Compare columns have a minimum width and scroll horizontally.
- [ ] Alt+1 stays "Shared"; Alt+2…Alt+9 follow account tabs (documented).
- [ ] Counting, limits and the preview per account (`plan.js` keyed by
  account id).

### Task A3.4: History, Interact, Tools, header

**Files:** `history.js`, `interact.js`, `tools.js`, `main.js`.

- [ ] History shows each target's account and filters by account.
- [ ] Interact: an acting-account picker when the source's network has
  several connected accounts; `/v1/resolve` gets `account`.
- [ ] Tools → relays picks the Nostr account (hidden without one).
- [ ] Header: the first account (API order) with a display name.

### Task A3.5: e2e

- [ ] `accounts.spec.ts`: select both Mastodon accounts, edit each tab,
  Compare shows both with their own limits; publish sends once per account
  with its own text; retry one account's item; a version-2 snapshot
  restores into account-keyed state (including a reply's acting account);
  a draft with two Mastodon accounts round-trips; the not-configured
  account is greyed with its reason; the interaction picker; colliding
  handles are disambiguated; a `@mobile` variant of the tab row.
- [ ] Existing specs use `data-account` selectors where they select tabs
  or rows.

### Task A3.6: docs, PR, review

- [ ] README: keyboard shortcuts, several accounts per network (env gives
  one each until C). CHANGELOG.
- [ ] Open PR A3; 2 QA + 1 security reviews; fix; hand over.

### As built (A3)

- **Modules.** `accounts.js` holds the list (sorted by `order`, then
  `ORDER` for `order -1`, then `created_at`), fires a `document`
  `accounts` event on each successful read, and names accounts:
  `labelsFor` (short handle, the full handle when two of a network
  collide, else the owner's label or the network), `nameOf` ("Mastodon
  @me@host", the network alone when the owner has one account there),
  `accessibleName` ("Mastodon, @me@host"). The new pure `snapshot.js`
  (`primaryIn`, `cleanOv`, `upgrade`) converts snapshots; unit tests in
  `e2e/unit/*.test.mjs` run with `node --test` (no dependency; a CI step
  in the e2e job, `npm run unit`).
- **Snapshot v3** (recovery and `specFromDraft`): `targets` (account
  ids), `overrides` by account id, `interaction.{account, platform,
  prevTargets}`. v1/v2 map each network to `primaryIn`: the first
  connected account with `default_selected`, else the first connected
  one (deviation: the plan said "first connected"; the default-selected
  one is what the composer posted from before). A network with no
  connected account is dropped and counted (`dropped`), and the restore
  says how many were left out. Renamed option keys (`bluesky_reply` →
  `reply`, …) are renamed on the way.
- **Restore** waits for `loadAccounts()`; a failed read flashes and
  doesn't restore (try again). A reply or quote is resolved again by its
  acting account.
- **Boot.** `main.js readAccounts` retries a failed first read (2 s,
  doubling to 60 s) while no read ever succeeded; the composer shows
  "Reading your accounts…", the error with **Try again**, or the empty
  state ("No accounts yet: set them in the server's environment").
  `#chips[data-state]` says which. Post with nothing selected before any
  read, or with an unavailable account selected, reads again first.
- **Selection.** `state.targets` (ids) + `state.chosen`: defaults
  (`defaultTargets()`) apply once, when the accounts first load, unless
  the owner already chose. Toggling a row now fires `change` on `#chips`,
  so autosave and the dirty mark follow (they didn't before: a small fix).
  An unavailable selected account can still be unticked.
- **Rows.** Grouped by network (`role=group`), a header with **Select
  all/Clear** when a network has several; reason as visible text linked
  by `aria-describedby`; avatar with a 14 px network mark. Layout fix:
  `.compose-left/.compose-right/.composer-card/.editor{min-width:0}`
  (the tab row used to widen the page on a phone).
- **Tabs.** WAI-ARIA tabs with automatic activation, roving `tabindex`,
  arrows/Home/End; the row scrolls (`nowrap`, `overflow-x:auto`) and
  scrolls the active tab into view. Alt+1 Shared, Alt+2…9 accounts.
- **History** pills are one per target (`data-account`) plus grey pills
  for the owner's networks the post didn't go to; detail rows carry
  `data-account`; `#hacct` filters by account (hidden under two
  accounts).
- **Interact** shows `#srcacct` when the source's network has several
  connected accounts; changing it resolves again with `account`.
- **Tools** relays: `#syncacct` picks the Nostr account (hidden with
  one); none connected shows the "needs a connected Nostr account" note.
- **e2e:** `accounts.spec.ts` (11 tests: two Mastodon accounts with own
  tabs, limits 500/1000, own wording and posts; retry of one account;
  v2 snapshot and v2 reply restore; a two-account draft; grouped rows
  with a reason and Select all; the Interact picker; tab keys; History
  filter; `@mobile` tab row), `regress-web-acc.spec.ts` (Post and Try
  again after a failed boot read). The harness gives each account a
  handle; "Mastodon (long posts)" is not selected by default. Mutation
  checks: removing the collision label, the per-account limit, the
  History filter, the resolve `account`, the detail row's account, the
  v2 mapping, the return to the previous selection, Try again, and
  Post's read each fail a test.

### As built (A3): review fixes

Two QA reviews and one security review on PR #39; all findings fixed:

- **Restore keeps unavailable networks** (`snapshot.js standIn`): a v1/v2
  network maps to its primary account, else its first account in any
  state but deleted (ticked, unavailable, Post refuses until it is
  unticked), and is dropped only when the owner has no account there.
  `loadDraft` falls back to the default accounts only when the input
  named none (`dropped` says which it named and lost); otherwise nothing
  is selected.
- **A pending recovery snapshot survives** (`drafts_recovery.js
  setRecoveryPending`): while the banner offers it, an empty composer or
  an open draft doesn't remove it; Restore, Discard and explicit clears
  end that. Ticking accounts on an empty composer isn't unsaved work
  (`toggled` fires `change` only with text, media, a draft or a reply).
- **Interact picker:** the card's actions wait while another account
  looks the post up (`setActionsWaiting`); if it can't, the picker goes
  back and the status names the account; the picker keeps the keyboard.
- **Settings made before the accounts load** get their network's
  defaults once the type is known (`ovOf`, a WeakSet of untyped entries).
- **Restore fallback** leaves the failed acting account out of the
  candidates. **Header** uses connected accounts only.
- **No page jump:** the tab row keeps its sideways scroll and brings the
  active tab into view by scrolling itself, only when the tab changes
  (no `scrollIntoView`, which also scrolled the page to the editor).
  **Compare** sits beside the `tablist` (`.ed-tabbar`), not in it.
- **Labels:** an account that is the owner's only one on its network is
  labelled by the network (or the owner's label), as before accounts;
  colliding long handles are cut in the user name (`fitHandle`).
  `accessibleName` includes the owner's label ("Mastodon (long posts),
  @me@two…"); Compare headers carry it; tabpanels are labelled by their
  tab.
- **States:** a live region (`#chips-status`) says loading/error/empty;
  Try again keeps the keyboard; "no accounts selected" waits for the
  accounts. Pickers (`#srcacct`, Tools) styled like `#hacct` (36 px);
  History pills keep handle case; "per-account options".
- **Tools:** switching the Nostr account while relay work runs is
  refused (it would show one account's result under the other).
- **Security (low, defence in depth):** maps keyed by account id are
  null-prototype (`byKey`); `cleanOv` drops `__proto__`, `constructor`,
  `prototype`; History selectors use `CSS.escape`.
- **Tests:** 11 more e2e tests and 5 unit tests for the above, each
  mutation-checked; the `@mobile` overflow check now measures `main`.

---
# PR B — Sign-in without OIDC

Spec §1 (`PUBLIC_URL`, `AUTH_MODE`, `TRUSTED_PROXIES`, `SETUP_TOKEN_FILE`),
§8, §10.1, §12 (B).

### Task B1: verify before building

- [ ] Authelia: does it honour `prompt=login` and `max_age=0`, and return
  `auth_time` in the ID token? Check its current documentation and source;
  record here. **If it doesn't, stop and ask Geoffrey** (options would be:
  a local passkey as the re-authentication step for OIDC owners, or no
  re-authentication in OIDC mode). There is no silent fallback: a missing
  `auth_time` is refused.
- [ ] `github.com/go-webauthn/webauthn`: current release and API
  (discoverable login, registration, the `User` interface, test helpers).
  Pin it; record differences from the spec.

### Task B2: the auth mode in every gate

The security-critical task: today every gate asks "is OIDC configured?",
so a mode that isn't OIDC would be fully open.

**Files:** `internal/api/{api,auth,middleware,middleware_auth,v1}.go`,
`cmd/publisher/main.go`, tests.

- [ ] `a.Mode` (`oidc | local | proxy`) replaces `authEnabled()` at every
  use: `withGates` (`middleware.go:141`), the proxy principal in
  `v1Principal` (`v1.go:189,218`), the `/auth` routes (`api.go:207`), the
  login redirect and the session allowlist check
  (`middleware_auth.go:28,42`), and the session sweep and `AppBaseURL` in
  `main.go`.
- [ ] The `proxy` principal exists only in `proxy` mode, with an explicit
  scope set (the pre-step-4 scopes and `accounts:write`); routes that proxy
  mode refuses carry a `NoProxy` flag in `v1Routes()`, enforced in
  `v1Auth` (`403 sign_in_required`).
- [ ] Sessions match the mode: in `local` only `users.source = 'local'`
  sessions count, in `oidc` only OIDC ones (a mode switch never keeps the
  other mode's sessions alive).
- [ ] In `local` the gates stay on in every state (setup pending,
  `PUBLIC_URL` missing). Exempt paths, matched after `path.Clean`: `/setup`
  and `/setup/*`, `/auth/*`, `/oauth/callback/*` (E), `/media/*` (D),
  `/healthz`, `/readyz`, the manifest and icons, and the sign-in and setup
  assets.
- [ ] Test matrix per mode: what unauthenticated `GET /`, `GET
  /v1/accounts`, `POST /v1/tokens`, `GET /setup` and `GET /media/x` answer;
  a local session in OIDC mode and the reverse are refused.

### Task B3: bootstrap variables

**Files:** `internal/config/config.go`, tests.

- [ ] `PUBLIC_URL`: an origin only; `https`, or `http://localhost[:port]`.
  An IP-literal host is refused when `AUTH_MODE` resolves to `local`
  (passkeys need a domain name; WebAuthn refuses IP relying parties).
  When set: added to the allowed hosts, the app base URL, and the default
  `OIDC_REDIRECT_URL` base.
- [ ] `AUTH_MODE`: `oidc | local | proxy` or empty; `oidc` needs
  `OIDC_ISSUER`; `OIDC_ISSUER` with `local` or `proxy` is an error.
- [ ] `TRUSTED_PROXIES`: CIDRs or addresses.
- [ ] `SETUP_TOKEN_FILE`: its directory must exist.
- [ ] Tests for each rule.

### Task B4: migration v12 and the birth version

**Files:** `internal/store/{migrations,migrate}.go`, new
`internal/store/localauth.go`, tests, golden schema.

- [ ] v12 (spec §8.2) tables and `users.source TEXT NOT NULL DEFAULT
  'oidc'`, `instance_meta`.
- [ ] `migrateOnce` knows the starting version: a new database (0) records
  `created_schema = <latest>`; an existing one records the version it had.
  Unset `AUTH_MODE` then resolves to `oidc` with `OIDC_ISSUER`, `local`
  when `created_schema >= 12`, `proxy` otherwise.
- [ ] Store functions run their own transactions (no exported `*sql.Tx`):
  credentials, login failures (pruned to 24 h and 10,000 rows), setup
  tokens (mint invalidates older; consume once), reauth nonces (consume
  once; purge expired), `CreateFirstCredential` (inserts only when no local
  credential exists, in one transaction, so two setup finishes can't both
  win).
- [ ] Migration test from a v11 fixture; birth-version tests.

### Task B5: client address, throttling, hashing limits

**Files:** new `internal/httpx/clientip.go`,
`internal/localauth/{throttle,password}.go`, tests.

- [ ] `ClientIP(r, trusted)`: the connection's address unless it is a
  trusted proxy; then the rightmost `X-Forwarded-For` address that isn't
  trusted. A startup and first-request warning when `X-Forwarded-For`
  arrives from a private peer while `TRUSTED_PROXIES` is unset (every
  client would share the proxy's address).
- [ ] Throttle: only password failures count per address (passkey sign-in
  is never locked out by them); IPv6 addresses are keyed by their /64; per
  address 5 failures in 15 minutes then a doubling delay up to 15 minutes;
  per owner a delay after 20 failures, never a block. Every password check
  goes through it: sign-in, `/auth/reauth`, the current-password check.
  Answers `429 rate_limited` with `Retry-After`.
- [ ] Hashing: argon2id t=3, m=64 MiB, p=4, 16-byte salt, 32-byte key, PHC;
  at most 2 at once (a third gets `429` at once); passwords 12–1024 bytes;
  constant-time compare.
- [ ] Tests: spoofed `X-Forwarded-For` ignored without trusted proxies;
  rightmost-untrusted; /64 grouping; windows with a fake clock; the
  semaphore under `-race`.

### Task B6: passkeys and password sign-in

**Files:** `internal/localauth/{webauthn,ceremony,service}.go`,
`internal/api/auth_local.go`, tests.

- [ ] Relying party id = the `PUBLIC_URL` host, origin = `PUBLIC_URL`,
  user verification required, discoverable credentials. Without
  `PUBLIC_URL` WebAuthn is refused outright (never derived from `Host`).
- [ ] Ceremonies: server-side state for 5 minutes under a random id in an
  HttpOnly, Secure, SameSite=Strict cookie with `Path=/`; each records its
  purpose (login, register, setup, reauth plus operation) and, except for
  login and setup, the session; finish refuses another purpose or
  session. The map is capped (oldest dropped) and `begin` is rate limited
  per address.
- [ ] Routes (outside `/v1`): `POST /auth/passkey/begin|finish`, `POST
  /auth/password`, `POST /auth/logout`. They require an `Origin` exactly
  equal to the `PUBLIC_URL` origin and `Content-Type: application/json`.
  Success creates a new session id (the old cookie's session, if any, is
  deleted), records `login.succeeded`; failure records `login.failed`
  without saying which part failed.
- [ ] A sign count lower than stored refuses the assertion (always-0
  authenticators accepted); a credential of another relying party id is
  refused.
- [ ] In `local` mode an unauthenticated page shows the sign-in screen.
- [ ] Tests: register and log in; wrong origin; wrong relying party;
  replayed challenge; a register ceremony finished as a login; password
  right, wrong, throttled; CSRF (missing or other Origin, form content
  type).

### Task B7: setup token, first credential, recovery commands

**Files:** `internal/localauth/setup.go`, `internal/api/setup.go`,
`cmd/publisher/cli.go`, tests.

- [ ] At startup in `local` mode with no local credential: mint a token
  (32 random bytes, hashed, 1 hour, single use), invalidate older ones, log
  `setup: open /setup#<token> on the address you use for Publisher`
  (prefixed with `PUBLIC_URL`), or write the URL to `SETUP_TOKEN_FILE`
  (0600) and log only the path; the file is deleted once used.
- [ ] `/setup`: exchanges the fragment once (`POST /setup/exchange`) for a
  server-side setup state referenced by a cookie (`Path=/setup`, 10
  minutes, HttpOnly, Secure, SameSite=Strict), clears the fragment, then
  creates the first passkey or password through `CreateFirstCredential`
  and a session; the setup state is deleted on success. Afterwards every
  `/setup` route answers `410 gone`.
- [ ] The setup page spots an origin other than `PUBLIC_URL` or an
  insecure context and says what to do; an expired token says to run
  `publisher setup-url`. Without `PUBLIC_URL` it explains how to serve
  Publisher over https (`tailscale serve`, Caddy, Traefik).
- [ ] CLI: `publisher setup-url`; `publisher owner reset-login
  [--revoke-tokens]` (deletes local credentials, sessions and nonces,
  lists tokens holding `admin` or a step-4 scope, revokes them with the
  flag, prints a setup URL). Both refuse unless the mode resolves to
  `local`.
- [ ] Tests: single use and expiry; restart invalidates; two concurrent
  finishes create one credential; setup closes; CLI on a temp database.

### Task B8: re-authentication

**Files:** `internal/localauth/reauth.go`, `internal/api/reauth.go`,
`internal/auth/auth.go`, tests.

- [ ] Operations (closed list): `signin.add`, `signin.remove`,
  `password.set`, `token.create_privileged`; later parts add
  `account.reveal`, `account.delete`, `instance.secrets`, `export`,
  `import`. Nonces for `account.reveal` and `account.delete` are also bound
  to the account id.
- [ ] Local: `POST /auth/reauth/begin {operation}` (passkey ceremony with
  purpose reauth) and `POST /auth/reauth {operation, assertion |
  password}` → `{nonce, expires_at}` (single use, 5 minutes, bound to
  session and operation).
- [ ] OIDC: `GET /auth/reauth/oidc?operation=…` starts a login with
  `prompt=login&max_age=0`; the login-state cookie carries the session
  hash, operation and start time. The callback requires `auth_time` after
  the start and `sub` equal to the session user's subject, creates no
  session and doesn't touch the session cookie; it records a single-use
  mark for that operation (5 minutes), which `POST /auth/reauth/claim
  {operation}` turns into a nonce.
- [ ] `requireReauth(op)` reads `Publisher-Reauth`: missing or invalid →
  `403 reauth_required`; a token → `403 session_required`; proxy mode →
  `403 sign_in_required`.
- [ ] Applied in B to creating tokens that hold `admin` or a step-4 scope,
  and to adding or removing a sign-in method and setting the password.
- [ ] Tests: single use, wrong operation, expiry, another session's nonce,
  OIDC with a stale or missing `auth_time`, another subject.

### Task B9: sign-in methods and sessions

**Files:** `internal/api/v1_me.go`, OpenAPI, tests.

- [ ] `GET /v1/me/sign-in` (passkeys, password set or not, mode); passkey
  register (`signin.add`), rename, remove (`signin.remove`); `PUT
  /v1/me/password` (`password.set`, plus the current password when one is
  set); `DELETE /v1/me/password`. Removing the last local method → `409
  last_sign_in_method`. Session only.
- [ ] `GET /v1/me/sessions` and `DELETE /v1/me/sessions/{id}`; changing the
  password, removing a passkey and `reset-login` end every other session
  and pending nonce.
- [ ] `GET /v1/me/sign-in-events` (last 50, from events and failures).
- [ ] Events: `login.succeeded|failed`, `token.created`,
  `signin.added|removed`.
- [ ] `GET /v1/instance` reports `auth` and, in proxy mode,
  `sign_in_recommended: true`.

### Task B10: the web app

**Files:** new `signin.js`, `setup.js`, `access.js`, `reauth.js`;
`tokens.js`, `main.js`, `index.html`, `app.css`.

- [ ] Sign-in screen (passkey, and password when set); setup screen.
- [ ] `reauth(op)` dialog returning a nonce; `v1()` retries a
  `reauth_required` answer once through it.
- [ ] Access panel (in Tools until C's Settings tab): passkeys, password,
  sessions, recent sign-in events.
- [ ] Proxy-mode banner "Set up sign-in".

### Task B11: e2e and docs

- [ ] A second Playwright `webServer` runs the harness with
  `E2E_AUTH=local` on `http://localhost:<port>` (the base URL matches
  `PUBLIC_URL`; Chromium treats localhost as secure). `signin.spec.ts` with
  a CDP virtual authenticator: setup → passkey → signed in; sign out and
  in; add a password; re-authenticate to create an admin token; five wrong
  passwords throttle the password but a passkey still signs in; setup
  closed afterwards. Fixtures for sign-in and re-authentication are shared
  with C–F.
- [ ] `errors.md`: `reauth_required`, `session_required`,
  `sign_in_required`, `last_sign_in_method`, `gone`, `rate_limited`.
  OpenAPI proxy-mode text.
- [ ] README: modes, `PUBLIC_URL`, `TRUSTED_PROXIES`, setup and recovery;
  upgrade notes (OIDC deployments: creating a privileged token now asks
  for a fresh OIDC login; OIDC-off stays proxy until `AUTH_MODE=local`).
- [ ] Open PR B; 2 QA + 1 security reviews; fix; hand over.

---

# PR C — Secret store, credentials and the account API

Spec §3, §4.1, §4.4 (Threads token move), §5, §10.2, §12 (C).

### Task C1: verify PDS discovery

- [ ] For a custom-domain handle, a `bsky.social` handle and a `did:web`
  account: handle → DID → document → `#atproto_pds`, and `createSession`
  on the discovered PDS with a handle, a DID and an email. Record here.

### Task C2: the keyring

**Files:** new `internal/secrets/{keyring,seal}.go`, `cmd/publisher/cli.go`
(`keygen`), `internal/config`, tests.

- [ ] Master key variables (spec §3.1): 32 bytes in any base64 alphabet,
  padded or not; both forms of one set, or malformed, is a startup error;
  `_FILE` read once and trimmed (a warning when it is world-readable;
  Docker secrets are 0444 by default, so not an error).
- [ ] After reading, `os.Unsetenv` the master key variables, `NSEC_HEX`,
  `OIDC_CLIENT_SECRET` and the other credential variables, and
  subprocesses (ffmpeg, ffprobe) run with a minimal environment.
- [ ] HKDF derivation (encryption key, key id, and an `idempotency v1`
  HMAC key for C9); `Seal`/`Open` with the spec's AAD and format.
- [ ] `publisher keygen` prints a key and a compose snippet.
- [ ] Tests: round trip; each AAD field; unknown key id; tampering;
  previous key opens; base64 variants; known-answer derivation; subprocess
  environment has no secret.

### Task C3: migration v13 and the secrets store

**Files:** `internal/store/{migrations,secrets,scrub}.go`, `store.go`
(pragma), tests, golden schema.

- [ ] v13: `secrets` with `idx_secrets_key`; `accounts.deleted_at`,
  `accounts.auth_method`; partial unique index `(owner_id, type,
  remote_id) WHERE deleted_at IS NULL AND remote_id IS NOT NULL`.
- [ ] `PRAGMA secure_delete = ON` on every connection.
- [ ] Store methods take sealed values and run their own transactions:
  `CreateAccountWithSecrets`, `ReplaceAccountSecrets`,
  `DeleteAccount` (soft delete, secrets gone, scheduled targets canceled),
  `Secrets(owner, scope, ref)`, `SecretKeyIDs`, `Reseal(fn)`.
- [ ] Scrubber (spec §3.2) plus bare `nsec1…` strings and URLs inside
  `url.Error` messages.
- [ ] Migration test from a v12 fixture.

### Task C4: the network guard everywhere owner-set addresses are reached

**Files:** `internal/netguard/{dialer,policy}.go`, `internal/config`
(`PRIVATE_DESTINATIONS`), `internal/accounts/state.go`, each driver's
tests, tests.

- [ ] `netguard.Policy` from `PRIVATE_DESTINATIONS` (hostnames and CIDRs);
  entries overlapping loopback, link-local or unspecified ranges, or a /0,
  are startup errors.
- [ ] Dialer: a `DialContext` wrapper checks the hostname (allowlisted
  private hosts) and a `Control` function checks the connected address with
  `netguard.IsBlockedIP` (which already covers 0/8, 198.18/15, 240/4,
  NAT64, 6to4, SIIT, loopback, link-local and metadata addresses), after
  `Unmap()`, with a carve-out only for allowed private ranges and never for
  the server's own listen addresses. No proxy from the environment.
- [ ] Redirects: at most 5, each dialed through the guard; a request that
  carries a credential (Authorization, a password or secret in the body)
  refuses cross-origin redirects, https→http, and any redirect of a POST.
- [ ] `netguard.OwnerURL(raw, schemes)`: https (and `wss` for relay
  fields), http/ws only for allowed private hosts; no userinfo, query or
  fragment; normalized origin.
- [ ] The host decides the policy: every transport of a `source = 'db'`
  account, and every saved setting's address, is guarded whatever the
  driver asks; env-configured addresses stay trusted. Adopted accounts and
  copy-from-env values become owner-set and are validated with `OwnerURL`
  when copied.
- [ ] Tests: each blocked range and IPv4-mapped forms; DNS rebinding;
  redirects (to a blocked address, cross-origin with Authorization, POST);
  per driver, a database account pointed at `127.0.0.1` or
  `169.254.169.254` never dials (HTTP and WebSocket).

### Task C5: field specs and auth methods

**Files:** `internal/destination/fields.go`, driver manifests, tests.

- [ ] `AuthMethod`, `FieldSpec` (+ `Revealable`), `EnumValue`,
  `Manifest.Auth/Config/InstanceConfig`; `ValidateFields` (types,
  required, pattern, enum, bounds, `MaxLength` default 4096, unknown keys,
  URLs through `OwnerURL`).
- [ ] Manifests per spec §5.7; the Nostr key accepts `nsec1…` or hex; the
  relay defaults offered for a new Nostr account are the env values when
  set, else a neutral list (never this instance's relay).
- [ ] Contract test: no `Secret` in `Config`; `Audience` fields are URLs;
  defaults validate.

### Task C6: database accounts, key status, rotation, the Threads token move

**Files:** `internal/accounts/{registry,state,keys}.go`, `main.go`,
`internal/health`, `internal/api/v1_resources.go`, tests.

- [ ] Startup: store → keyring → rotation (reseal everything under the
  previous key in one transaction; any failure changes nothing) → registry.
- [ ] Key status `none | ok | missing | mismatch`; affected accounts
  `locked` (their items wait, A2.3); secret writes refused
  (`master_key_required` / `master_key_mismatch`); non-critical `secrets`
  readiness check; `/v1/instance.secrets`; an error at startup.
- [ ] Database accounts open with decrypted secrets; `Host.Save` reseals
  and writes secrets and config in one store call.
- [ ] Env accounts with a key: state moves to `secrets`. The Threads
  token moves from `threads_token` at startup (overwrite, delete, then
  `wal_checkpoint(TRUNCATE)`, checking the result and retrying while busy);
  the seed hash goes to the account's config. A warning names the
  `.pre-vN.bak` files that still hold the plaintext token; the README says
  to delete them once the upgrade is confirmed.
- [ ] Tests: each status; rotation success and rollback; a locked
  account's item goes out after the key is fixed; the move (then grep the
  database, WAL and backups made afterwards for the token).

### Task C7: Bluesky PDS discovery

**Files:** `internal/bluesky/discover.go`, tests.

- [ ] DNS TXT `_atproto`, else `/.well-known/atproto-did` (guarded); DID
  document from `PLC_DIRECTORY_URL` or `did:web` (guarded); `#atproto_pds`
  validated with `OwnerURL`. A DID skips the first step; an email needs
  `pds_url`.
- [ ] Tests with fake DNS and `httptest`, including a PDS on a private
  address (refused).

### Task C8: log scrubbing at the handler

**Files:** `cmd/publisher/main.go` (`setupLogger`), `internal/logging`,
`internal/notify`, tests.

- [ ] The slog handler scrubs every string and error attribute
  (`ReplaceAttr`); alert bodies are scrubbed; Blossom and other upstream
  error bodies are no longer echoed verbatim.
- [ ] Test: logging a URL error with `access_token=` and an `nsec1`
  string prints neither.

### Task C9: the account write API

**Files:** `internal/api/{v1_accounts,v1,v1_idem}.go`,
`internal/accounts/write.go`, OpenAPI, `errors.md`, tests.

- [ ] Scopes `settings:read`, `settings:write`, `instance:admin` added;
  `admin` implies none of the step-4 scopes; tokens holding a step-4 scope
  need `token.create_privileged`.
- [ ] `POST /v1/accounts` (spec §5.2) with `Idempotency-Key`. Upstream
  error text: the submitted secret values are removed from it, then it is
  scrubbed and cut to 200 characters.
- [ ] Idempotency store: the request fingerprint replaces secret fields
  with an HMAC (C2's key) before hashing; a route-table test asserts that
  no idempotent route can return a secret or a capability. `:reveal` has
  its own route entry, never idempotent, `no-store`.
- [ ] `GET`/`PATCH`/`DELETE`, `:check`, `:reveal`, `:adopt` per spec
  §5.3–5.6 (reveal and delete bound to the account by their nonce). A
  delete also removes the account's `nostr_sync_relays` and
  `nostr_sync_relay_lists` rows (both reference `accounts`).
- [ ] Proxy mode through `NoProxy` (B2): credential writes, adopt, reveal,
  and later connect, settings secrets and instance settings. A table test
  over `v1Routes()` checks every flagged route.
- [ ] Rate limit: 10 creates or credential changes per minute per owner
  (fake clock in tests); an idempotent replay doesn't use it up or call the
  network.
- [ ] Events `account.created|updated|deleted|adopted` with field names.
- [ ] Tests: each route, scope and mode; the duplicate race; audience
  change without and with credentials; a leak test greps responses,
  events, logs, `idempotency_keys`, the database and WAL for the
  plaintext.

### Task C10: the Settings tab, Accounts and Access

**Files:** new `settings.js`, `fields.js`; `accounts.js`, `access.js`,
`tokens.js`, `index.html`, `main.js`, `app.css`.

- [ ] Settings tab with Accounts and Access (passkeys, password, sessions,
  API tokens moved here, sign-in events). D adds the rest.
- [ ] Accounts: grouped list with status and health; add (network → method
  → form from the manifest); Nostr generate then reveal once; import a key
  (`nsec1…`); check; adopt; enable/disable; default selection; public feed;
  rename; delete (re-authentication); change credentials. Remote profile
  fields rendered with `textContent`; only http(s) profile links.
- [ ] Banners: master key missing or mismatched (with the fix), proxy mode.
- [ ] First-run steps 2 (master key: generated in the browser, shown with
  the compose snippet; skippable) and 3 (first account).

### Task C11: e2e, docs, PR

- [ ] The account specs run in the local-auth project (B11's fixtures).
  The harness has a master key; its fakes declare the real auth methods
  and accept credentials by a simple rule (`bad` is rejected).
- [ ] `account-settings.spec.ts`: add Bluesky (app password), Mastodon
  (token), Threads (token), Nostr (generate and reveal; import `nsec1`);
  duplicate refused; rejected credentials on the field; rename, disable,
  delete with re-authentication; the new account in the composer, not
  selected by default.
- [ ] README: the master key (Docker secret file), adding accounts,
  `PRIVATE_DESTINATIONS`, what encryption protects, backups after the token
  move; upgrade notes for C.
- [ ] Open PR C; 2 QA + 1 security reviews; fix; hand over.

---

# PR D — Settings, account sets, media stores

Spec §1 (precedence), §7, §8.5 step 4, §12 (D).

### Task D1: migration v14

- [ ] `settings`, `accounts.defaults_json`, `account_sets` (spec §7.1),
  and `target_items.manual_due INTEGER NOT NULL DEFAULT 0` (set by a
  manual retry, cleared on claim) so "automatic retries off" can still let
  manual retries through. Migration test; golden schema.

### Task D2: the settings service

**Files:** new `internal/settings/{defs,service,patch}.go`, tests.

- [ ] Definitions (spec §7.2), including driver `InstanceConfig` fields
  as `<type>.<key>` read from manifests.
- [ ] Precedence env → saved → default with `source` and locks; env values
  parsed as config does today.
- [ ] `Current()` immutable snapshot; `OnChange`; writes serialized.
- [ ] Merge patch; validation; secret settings through the secrets store
  with audience binding (a webhook URL is itself a secret; its token is
  bound to its origin).
- [ ] Tests: precedence, locks, merge-patch edge cases, secrets absent
  from snapshot JSON, audience change without the token.

### Task D3: consumers apply settings live

- [ ] Translator, feed webhook, alert webhook, retry policy, schedule
  grace, user languages, public feed token read `Current()` or rebuild on
  change. Saved webhook addresses use the guarded client (C4).
- [ ] Automatic retries off: items waiting for an automatic retry aren't
  claimed; manual retries (`manual_due`) and locked items (A2.3) still are.
- [ ] Tests: a change applies to the next claim or request; retries off
  and on with a waiting item and a manual retry.

### Task D4: settings and account-set API; per-account defaults

**Files:** `internal/api/{v1_settings,v1_account_sets,v1_accounts,v1_create}.go`,
OpenAPI, `errors.md`, tests.

- [ ] `/v1/settings` and `/v1/instance/settings` (spec §7.4). Proxy mode
  refuses every instance setting and every owner secret setting (route
  flag plus a field check in the owner PATCH and `:copy-from-env`).
  Instance secret writes need `instance.secrets`.
- [ ] `:test` (alerts, feed, media) uses saved values only, one per 10 s,
  answers a class.
- [ ] Feed token "Generate": its own non-idempotent, `no-store` route.
- [ ] `/v1/account-sets` with ETags; per-account `defaults` with the
  spec's precedence; preview lists applied defaults.
- [ ] Tests for each, including the proxy refusals table.

### Task D5: media stores and Blossom's own key

**Files:** `internal/media/{store,blossom}.go`, `internal/api/v1_media.go`,
tests.

- [ ] `media.Store` (spec §7.6); metadata stripping in front of every
  store.
- [ ] Blossom signs with `media.blossom_key` once set (shown as an npub)
  and only then adds the BUD-11 server tag; until a key is set, env
  deployments sign as today with the env Nostr key and no server tag (so
  Geoffrey's uploads don't change). Descriptor URLs returned by the server
  must be on the Blossom origin.
- [ ] Switching stores affects new uploads only.

### Task D6: the local disk store

**Files:** `internal/media/local.go`, `internal/api/media_serve.go`,
`internal/config` (`MEDIA_DIR`), tests.

- [ ] Storage, names and serving per spec §7.6. The type is sniffed at
  `Put` and stored; served responses replace the global CSP header with
  `default-src 'none'; sandbox`; non-allowlisted types are attachments;
  `Owns`/`Fetch` read from disk by the validated name, never over HTTP.
- [ ] Free-space floor and optional quota (`insufficient_storage`).
- [ ] `media.public` self-test: fetch a probe from the operator-set
  `PUBLIC_URL` with a client that follows no redirects (the operator's
  address is trusted, so Tailscale users needn't allowlist 100.64/10);
  require 200 and the exact bytes.
- [ ] `media_unsupported` for URL-transport accounts without a public
  store.
- [ ] Tests: traversal, wrong extension, HTML/SVG served as attachment,
  floor and quota, self-test success and each failure.

### Task D7: the Settings tab, all sections; account sets in the composer

- [ ] Composing, Media, Publishing, Integrations, System (spec §7.5) with
  sources and "copy into the app".
- [ ] Composer: account-set picker; the default set is the starting
  selection.
- [ ] First-run step 4 (media store).

### Task D8: e2e, docs, PR

- [ ] The harness accepts extra env for the settings specs (locked
  values). `settings.spec.ts`: a locked setting and copy-from-env;
  languages in the composer; account sets; account defaults in the
  preview; local media served with its headers; the public self-test
  passing (harness `PUBLIC_URL` = its own address) and failing.
- [ ] README: settings tiers, local media and the proxy rule for
  `/media/`, the Nostr home-IP warning; upgrade notes for D.
- [ ] Open PR D; 2 QA + 1 security reviews; fix; hand over.

---

# PR E — OAuth connect

Spec §4.3 (`Connector`), §6.1–6.3, §12 (E).

### Task E1: verify Threads OAuth

- [ ] From Meta's documentation: authorize host and parameters, code
  exchange, long-lived exchange, redirect URI rules, and whether replies to
  the account's own threads need `threads_manage_replies`. Record here;
  update spec §6.3 where it differs.

### Task E2: migration v15 and the flow manager

- [ ] `oauth_flows` (+ `redirect_uri`) and `oauth_clients`; rotation (C6)
  includes `oauth_flows.data_sealed`, sealed with AAD naming the flow id,
  owner, user and type.
- [ ] State: 32 random bytes, hashed, single use, 10 minutes; PKCE S256;
  binding to owner and user; hourly purge; fixed error codes; 10 starts
  per minute per owner.

### Task E3: connect routes

- [ ] Routes per spec §6.1. **Redirect URI:** `PUBLIC_URL`, else the app
  base URL from `OIDC_REDIRECT_URL`, else connect is refused ("set
  PUBLIC_URL"); never from a request header. Stored with the flow and used
  verbatim at the exchange.
- [ ] The callback completes by state; a present session must be the
  flow's user; a second callback (refresh) shows the first outcome without
  redoing the exchange; strict-CSP result page, no upstream text.
- [ ] Every OAuth request (discovery, registration, token exchange,
  identity) goes through the guarded client.
- [ ] Tests: replayed and expired state, wrong session, no session (the
  home-screen app case), denied consent, double callback, master key lost
  mid-flow, `duplicate_account` and `identity_changed` at completion,
  choosing among candidates.

### Task E4: Mastodon connect

- [ ] Spec §6.2, with `iss` required whenever the metadata advertises
  `authorization_response_iss_parameter_supported` (and checked when
  present otherwise).
- [ ] Tests against a fake instance: 4.3+ with PKCE; pre-4.3; issuer and
  endpoint mismatches; `iss` missing when advertised; `invalid_client`
  then re-registration.

### Task E5: Threads connect with the owner's Meta app

- [ ] Per E1; `InstanceConfig` `app_id`/`app_secret`; long-lived token;
  expiry state set so A2.2's refresher takes over; the grant's end date
  shown.
- [ ] Tests against a fake Graph API, including tokens in query strings
  never logged.

### Task E6: the web app

- [ ] Connect buttons (new tab; the starting tab polls the flow);
  candidates; reconnect for `needs_reconnect`; Settings → Integrations →
  Threads app with the redirect URI and Meta's guide.

### Task E7: e2e, docs, PR

- [ ] The harness serves a fake Mastodon instance (4.3+ and pre-4.3) and
  a fake Graph API; `connect.spec.ts` (local-auth project) connects both
  and reconnects one.
- [ ] README: connecting Mastodon; the Meta app for Threads.
- [ ] Open PR E; 2 QA + 1 security reviews; fix; hand over.

---

# PR F — Documentation, export and import

Spec §9, §12 (F).

### Task F1: export

- [ ] Passphrase from a 2048-word list with a compatible licence (the
  BIP-39 English list; licence recorded), shown once; file format per spec
  §9; session + `export` re-authentication; `no-store`; never idempotent.
- [ ] Tests: round trip; tampered header; pinned parameters.

### Task F2: import

- [ ] `POST /v1/import` → summary (every endpoint it would set, accounts
  skipped because they match an env account's `remote_id`) and a
  confirmation id; `POST /v1/import:confirm` (with the `import` nonce)
  applies it. The decrypted contents stay in memory, bound to the session,
  for at most 10 minutes. KDF under the hashing semaphore; URLs through
  `OwnerURL`; each account re-identified before it is marked connected.
- [ ] Tests for each refusal and a round trip between two databases.

### Task F3: the web app

- [ ] Settings → System: export (passphrase shown once) and import (file,
  passphrase, summary, confirm).
- [ ] `export.spec.ts`: export, wipe accounts in a second harness
  database, import, confirm.

### Task F4: documentation and release

- [ ] README rewritten around the minimal compose file; adding accounts per
  network; configuration by tier; backups (database and key separately,
  keys across rotations); upgrading from env-only; the security model;
  `PRIVATE_DESTINATIONS`, `TRUSTED_PROXIES`, local media and proxy rules;
  the readiness table; feed `links[]`; keyboard shortcuts.
- [ ] The env defaults that point at this instance's own relay
  (`NIP65_BOOTSTRAP_RELAY`, `FALLBACK_RELAYS`, `PRIMARY_RELAYS`) become
  neutral for new installs, with an upgrade note. **Confirm with Geoffrey
  first** (it changes his relays if he relies on the defaults).
- [ ] CHANGELOG release section and release notes for a `vX.Y.Z` tag.
- [ ] Open PR F; 2 QA + 1 security reviews; fix; hand over.

---

## Spec deviations

Made by this plan; the spec is updated to match.

| Spec | Plan |
|---|---|
| §Parts: six PRs | Part A is three PRs (A1–A3); eight in all. |
| §2.1: order by type alphabetically | Order by driver registration (`TypesInOrder`), which keeps the composer's order. |
| §2.2: `needs_reconnect` on an `auth` failure | Only after an authenticated check confirms it; a later passing check clears it. |
| §2.2: locked items retried "on the normal schedule" | Not counted as attempts, never given up, claimed with retries off. |
| Plan A2.3: locked items pushed "by the backoff" | Wait `clamp(age, 1 min, 15 min)`, no alert (see As built A2). |
| Plan A2.5: unavailable account refused at create | Pulled into A2.3 (`accountUnusable`), so no post is accepted for an account that can't send. |
| §2.4: `nostr_relay` readiness check kept | Dropped; each Nostr account's check covers its relay. |
| §2.6: field code `duplicate_account` | Today's `duplicate` kept for the same account twice; `duplicate_account` is C's 409. |
| §2.6: `profile` object | Flat `display_name`, `avatar` (as today) plus `profile_url`. |
| §4.2: `SaveSecrets` and `SaveConfig` | One atomic `Save(StateUpdate)`. |
| §4.2: `HTTPClient(policy)` (and plan A1.2's `Dialer`) | `Net(policy)`: one dialer, proxy and redirect rule every client, transport and WebSocket is built from. |
| Plan A1.2: `NetTrusted` / `NetOwner` | `NetAccount` / `NetContent`: the driver says where an address came from, the host decides how far it is trusted. |
| §4.2: `Save` semantics unstated | A patch merged key by key (`MergeState`). |
| Plan A1.2: `EnvVars() []string` | `EnvVars() []EnvVar{Name, Required}`. |
| §4.3: `Identity.RemoteID` as `json:"remote_id"` | `omitempty` added (a network may not report one). |
| §4.2: Nostr's unauthenticated check opens a WebSocket | The home relay's NIP-11 document over HTTP, through the host's network (the library's WebSocket can't use it yet). |
| §4.3 (plan A1.3): `BLUESKY_PDS_URL` optional | Required with `BLUESKY_APP_PASSWORD` until PDS discovery (C7). |
| Plan A1.4: Mastodon `RemoteID` is the account URL | The ActivityPub actor id (`uri`), else the profile URL. |
| §4.3: `ResolveSource` returns `SourceRef` | Returns `ResolvedSource` (reference, preview, actions). |
| §6.1: redirect URI from the request origin | `PUBLIC_URL`, else the OIDC app base URL, else connect refused. |
| §7.6: BUD-11 server tag always | Only with the media store's own key. |
| §8.4: OIDC fallback when `auth_time` is missing | No fallback; stop and ask if Authelia lacks it. |
| §8.5: first-run step 2 in D | In C, with step 3. |

## Risks and how the plan handles them

| Risk | Handling |
|---|---|
| The engine switch (A2.3) is one large commit | It lands only after the store and registry are tested alone; every existing dispatch and API test passes through test helpers before new cases are added; the restart-recovery test covers two accounts of a type. |
| A schema or key mistake loses posts | Every migration writes a backup and is tested from a fixture and in the realistic-upgrade test; a missing or wrong key locks accounts and their posts wait without using retries. |
| Geoffrey's production changes silently | Each PR lists behaviour changes in CHANGELOG and upgrade notes, with rehearsal steps for migrations; Blossom signing, relay defaults and composer order stay as they are unless he agrees. |
| Lockout after enabling local sign-in | Existing databases default to proxy mode; passkey sign-in isn't blocked by password throttling; `setup-url` and `reset-login` recover from the container. |
| Credentials sent somewhere else | Audience binding, the guard on every owner-set transport, and no credential-carrying redirects across origins. |
| A long-lived branch drifts from `main` | Parts are pushed only after the previous merge, rebased onto `main`. |
