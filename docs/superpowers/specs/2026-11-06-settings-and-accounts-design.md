# Settings and accounts for self-hosters (roadmap step 4)

**Status:** Spec, revised after review (2026-11-06)
**Date:** 2026-11-06
**Area:** Accounts, credentials, configuration, sign-in, media, self-hosting
**Analysis and decisions:** Claude Doc "Step 4: settings and accounts for
self-hosters" (https://claude.ai/artifact/2EhoDrXr8GosiyhuYk73Hk). Geoffrey
picked the recommended option (A) for all nine decisions on 2026-11-06.

## Review history

The first draft was critiqued by three reviewers (fit with the code,
security, product and API). This version folds in their findings. The main
changes:

- **Parts reordered** (§Parts): sign-in comes second, before anything can
  write a credential, and settings come before the OAuth flows that need an
  instance setting (the Threads app). The letters follow the new order.
- **Contract v2 moves into part A** (`Open`, `FromEnv`, `Checker`, a stable
  remote identity), so there is one way to open an account from the start
  and the e2e harness can inject fakes through it.
- **Credentials are bound to where they are sent** (§3.3): changing an
  instance URL, PDS, webhook or Blossom URL requires the credential again,
  so a stored secret can never be redirected to another server.
- **Private network destinations need an operator allowlist**
  (`PRIVATE_DESTINATIONS`), checked when connecting, not only before.
- **Proxy mode can't manage credentials**; existing proxy deployments stay
  in proxy mode after upgrading instead of being locked out.
- **A missing or wrong master key never stops the instance**; accounts it
  can't open are `locked` and their posts wait instead of failing.
- **Re-authentication is a one-use nonce** bound to one operation, and
  covers adding sign-in methods and creating powerful tokens.
- **The local disk media store** (new in this spec) is hardened: content
  types allowlisted, its own directory and disk floor, public serving only
  after a self-test.

### Changes made while planning (2026-11-06)

The implementation plan (`docs/superpowers/plans/2026-11-06-settings-and-accounts.md`)
was reviewed three times and changes some details of this spec. Where the
two differ, the plan's "Spec deviations" table wins. In short:

- Part A ships as three PRs (A1 contract and drivers, A2 engine and API, A3
  web app).
- Accounts are ordered by driver registration, not alphabetically.
- `needs_reconnect` needs a confirming authenticated check.
- A locked account's items don't use up retries.
- `AccountHost` has one atomic `Save`.
- `ResolveSource` returns a reference with its preview.
- The OAuth redirect URI never comes from a request header.
- OIDC re-authentication has no fallback.
- The Blossom server tag comes only with the media store's own key.
- `nostr_relay` readiness is folded into the account checks.
- Part B as built: every token created from a browser asks to confirm
  it's the owner; a password change takes the current password instead;
  passkey ceremonies are sealed in the browser's cookie; a browser the
  owner signed in with before skips the per-owner delay; setup opens
  again when no usable way to sign in is left.

## Decisions (Geoffrey, 2026-11-06)

| # | Question | Decision |
|---|---|---|
| 1 | Where the master key comes from | `PUBLISHER_MASTER_KEY` or `PUBLISHER_MASTER_KEY_FILE`, required only once a secret is stored in the database. Env-only instances need nothing new. |
| 2 | Sign-in without OIDC | Passkeys (WebAuthn), an optional password, and a one-time setup token on first run. |
| 3 | Which settings move to the app | Three tiers (§1): bootstrap and tuning stay env-only; accounts, media, composing, integrations and publishing move to the database and the Settings page. |
| 4 | Bluesky OAuth | App passwords now. atproto OAuth later, as an option for publicly reachable instances. |
| 5 | Nostr remote signer (NIP-46) | Designed for now (§6.5), built after step 4. |
| 6 | Threads | Bring your own Meta app, OAuth in the page, and paste-a-token as a fallback. |
| 7 | Composer with several accounts of a network | One editor tab and one Compare column per selected account, plus account sets. |
| 8 | Delivery | One PR per part, six parts, each deployable and reviewed. |
| 9 | Env value vs saved value | The environment wins; the field shows as locked. |

## Problem

Checked on `main` at `2f43764` (step 3, PR #36, merged).

1. **One account per type above the database.** Accounts are rows with
   stable `acc_` ids since step 2 (migration v3) and targets are stored per
   account, but:
   - the dispatcher keeps `Accounts map[type]Account` and refuses a second
     account of a type (`AccountIDs`, `SendsAs`,
     `internal/dispatch/publish.go:46-68`);
   - the engine's claims carry the type, not the account
     (`store.Claim`, `SendingItem`, `Unscheduled`,
     `internal/store/engine.go:20-28`);
   - `POST /v1/posts` refuses two targets of one type (`duplicate_type`,
     `internal/api/v1_create.go:345`);
   - `PostSpec` and `InteractSpec` are keyed by type;
   - idempotency keys derive from the type
     (`store.ItemKey(postID, typ, ordinal)`);
   - Nostr fan-out rows have no account and relay retries go to a post's
     first target (`store/fanout.go:20-40`, `retrier.go:127`);
   - `cmd/publisher/main.go` wires identity, resolve, readiness, relay sync
     and the Threads token manager to the single client of each type;
   - `SavePost` silently gives a target with no account the env account of
     its type (`store/models.go:170-174`);
   - the public feed lists links by network (`feed.Link{Platform, URL}`);
   - the web app keeps `accountFor(type)` (12 modules) and a recovery
     snapshot keyed by type (`state.js`).
2. **Nostr and Blossom are mandatory.** Step 2's task 1.6 never landed:
   `NSEC_HEX`, `OWNER_PUBKEY` and `BLOSSOM_URL` are required
   (`internal/config/config.go:197-206`). Blossom uploads are authorized
   with an event signed by the Nostr key (BUD-11 kind 24242,
   `internal/media/media.go:146-153`). Every media object lives on Blossom,
   so even a Bluesky-only instance needs a Blossom server to attach a photo.
3. **Every credential is an environment variable** (56 variables, read
   once at startup, `config.go:109`). The one credential that is stored,
   the refreshed Threads token, sits in plain text (`threads_token.token`).
4. **The contract can't describe or open an account.** Step 2's planned
   `AuthSpec` wasn't built, `destination.Identity` has no stable remote id,
   and each driver's client is built by hand in `main.go`.
5. **No sign-in without an OIDC provider.** With `OIDC_ISSUER` unset every
   request the proxy passes can do everything.
6. **Invalid booleans are ignored.** `AUTO_RETRY_ENABLED=flase` keeps
   retries on with no warning (`getBool`, `config.go:254`).

## Goals

- A new self-hoster starts the container with one volume, one master key
  and an https address, and does everything else in the web app: sign-in,
  accounts, media, integrations.
- Any number of accounts per destination type, in the engine, the API and
  the composer.
- Credentials in the database are encrypted with a key that isn't in the
  database, bound to the server they are sent to, and never returned.
- The destination contract describes each type's account form, instance
  prerequisites and connect flow, so a third-party driver needs no core UI
  code.
- Existing env-configured deployments upgrade with no configuration change
  and keep their account ids, history, drafts and API tokens.
- Reliability first: a key problem, a network outage or a reconnect never
  loses a scheduled post.
- Nothing designs against multiple owners: every new row is scoped by
  `owner_id` or explicitly instance-wide, and instance-wide changes need an
  instance-level permission.

## Non-goals

- Bluesky OAuth, NIP-46 remote signing (designed in §6.4–6.5), new
  destinations, multiple owners, a KMS or Vault backend, per-account media
  stores, cross-posting two accounts of one network into one thread,
  collecting unused media.
- Moving bootstrap and tuning variables into the database (§1).
- Moving post history between servers (the database backup does that).

## Parts

| Part | Scope | Migration |
|---|---|---|
| A | Accounts by id. Contract v2 core (`Driver.Open`, `FromEnv`, `AccountHost`, `Checker`, `Identity.RemoteID`, `SourceResolver`). Account registry with leases. Engine, fan-out, retries, API, feed, readiness, identity, resolve and relay sync per account. Composer, History, Interact per account. Nostr and Blossom optional. `getBool` fix. | v11 |
| B | Sign-in without OIDC: `AUTH_MODE`, passkeys, optional password, setup token, re-authentication nonces, trusted proxies, throttling. | v12 |
| C | Secret store and master key. Field specs (`Auth`, `Config`, `InstanceConfig`). Account write API and credential methods (app password, token, key, generate). Audience binding. Adopt an env account. Reveal. Threads token moved and encrypted. | v13 |
| D | Settings store (owner and instance) with env precedence; `/v1/settings`, `/v1/instance/settings`; the Settings tab; per-account defaults; account sets; media store choice (Blossom with its own key, or local disk); integration tests. | v14 |
| E | OAuth connect: Mastodon (authorization code with PKCE where supported), Threads with the owner's Meta app; reconnect; flow status for the PWA. | v15 |
| F | README and compose rewrite, upgrade notes, encrypted export and import. | none |

Each part is one PR, deployable on its own, reviewed by 2 QA agents and 1
security agent before handover. Migration numbers are indicative.

**Releases.** `main` publishes the `edge` image. Part A alone is a safe
release. Part B changes `/v1/instance.auth` and adds sign-in for fresh
instances; existing ones are unaffected (§8.1). Parts C–E add features.

---

## 1. Configuration tiers

| Tier | Where | Changed by | Variables |
|---|---|---|---|
| Bootstrap | env only | the operator, restart | `PORT`, `DB_PATH`, `LOG_LEVEL`, `FFMPEG_PATH`, `FFPROBE_PATH`, `VIDEO_WORKDIR`, `ALLOWED_HOSTS`, `OIDC_*`, `SESSION_TTL`; new: `PUBLIC_URL`, `AUTH_MODE`, `TRUSTED_PROXIES`, `PRIVATE_DESTINATIONS`, `MEDIA_DIR`, `PUBLISHER_MASTER_KEY[_FILE]`, `PUBLISHER_MASTER_KEY_PREVIOUS[_FILE]`, `SETUP_TOKEN_FILE` |
| Accounts | database (env accounts still supported) | the owner, live | credentials and per-account config |
| Settings | database, env overrides | the owner (owner settings) or an instance admin (instance settings), live | languages, DeepL, media store, feed and alert webhooks, auto-retry, Threads app |
| Tuning | env only | the operator, restart | `AUTO_RETRY_BASE_DELAY`, `AUTO_RETRY_MAX_DELAY`, `PUBLISH_WORKERS`, `RETRIER_TICK`, `FANOUT_TICK`, `PUBLISH_TIMEOUT`, `POW_TIMEOUT`, `RELAY_CACHE_TTL`, `VERIFY_HTTP_TIMEOUT`, `PLC_DIRECTORY_URL` |

Where every current variable goes:

| Variables | Part | New home |
|---|---|---|
| `NSEC_HEX`, `OWNER_PUBKEY` | A, C | The env Nostr account (A). `NSEC_HEX` alone is enough (the public key is derived); `OWNER_PUBKEY` alone is an error; both must match, as today. A database Nostr account takes a key or generates one (C). |
| `NIP65_BOOTSTRAP_RELAY`, `FALLBACK_RELAYS`, `PRIMARY_RELAYS`, `NOSTR_PRIMARY_FANOUT`, `POW_DIFFICULTY_DEFAULT`, `POW_DIFFICULTY_MAX` | A | The env Nostr account's config (through `FromEnv`) and the defaults offered for a new Nostr account (C). |
| `SYNC_RELAYS` | A | Seeds an env Nostr account's sync relay list when it is empty. |
| `MASTODON_BASE_URL`, `MASTODON_TOKEN`; `BLUESKY_*`; `THREADS_ACCESS_TOKEN`, `THREADS_USER_ID` | A, C, E | Env accounts (A); database accounts by credentials (C) or OAuth (E). |
| `BLOSSOM_URL` | A, D | Optional (A); the media store setting (D). |
| `USER_LANGUAGES`, `DEEPL_API_KEY` | D | Owner settings: composing. |
| `PUBLIC_FEED_TOKEN`, `FEED_WEBHOOK_URL`, `FEED_WEBHOOK_TOKEN`, `ALERT_WEBHOOK_URL`, `ALERT_WEBHOOK_USER`, `ALERT_WEBHOOK_PASS` | D | Owner settings: integrations. |
| `AUTO_RETRY_ENABLED`, `AUTO_RETRY_MAX_ATTEMPTS`, `SCHEDULE_GRACE` | D | Owner settings: publishing. |
| everything else | — | Unchanged. |

**Precedence (decision 9).** For a setting: a non-empty env variable wins,
then the saved value, then the default. Every setting shows its source
(`env | saved | default`) and names its variable; writing an env-set key is
`409 locked_by_env`. "Copy the environment value into the app" saves the
current env value, so the operator can then remove the variable without
the setting falling back to its default.

For an env account, credentials and connection config come from env. Its
label, `enabled`, defaults, default selection and public-feed flag belong
to the app and are editable. Adopting it (§5.6) moves its credentials into
the database.

**`PUBLIC_URL`** is the https address the owner's browser uses
(`https://post.example.com`; `http://localhost[:port]` allowed). When set,
it is added to the allowed hosts, sets the app's base URL (CSRF and
redirects), is the passkey relying party (§8), and is the default
`OIDC_REDIRECT_URL` base.

**Invalid env values are startup errors** (part A). `getBool` accepts
`1/true/yes/on` and `0/false/no/off` (any case) and fails `config.Load`
on anything else, naming the variable.

## 2. Accounts by id (part A)

### 2.1 The account registry

New package `internal/accounts`:

```go
type Registry struct { /* id → *entry, behind a RWMutex; ids are global */ }

type Entry struct {
    Row    store.Account // id, owner, type, label, handle, source, enabled, flags
    Status Status        // connected | disabled | not_configured | locked | needs_reconnect | deleted
    Reason string        // human-readable
}

func (r *Registry) Load(ctx context.Context) error                 // at startup, before d.Recover
func (r *Registry) Get(id string) (Entry, bool)
func (r *Registry) List(owner string) []Entry                      // order below
func (r *Registry) OfType(owner, typ string) []Entry
// Acquire leases the live account object; release must be called when the
// attempt (or request) using it ends.
func (r *Registry) Acquire(id string) (destination.Account, func(), error)
func (r *Registry) Caps(ctx context.Context, id, typ string) destination.Capabilities
func (r *Registry) Reopen(ctx context.Context, id string) error    // credentials or connection config changed
func (r *Registry) Update(id string, row store.Account)            // metadata only: same object
func (r *Registry) OnChange(func(Change)) (unsubscribe func())     // many subscribers
```

- **Order.** Types in `destination.Types()` order (alphabetical), then
  `created_at`, then id. "The first account of a type" always means this
  order. `GET /v1/accounts` keeps its `(created_at, id)` pagination; the
  web app sorts with the registry order the API also returns as `order`.
- **Opening** goes through `Driver.Open` (§4.2) for every account: env
  accounts with what `FromEnv` read, database accounts (part C) with their
  stored config and secrets.
- **Leases, not timers.** `Reopen` builds the new object and swaps it in;
  the old object is closed (if it implements `io.Closer`) when its last
  lease is released. A send, a fan-out pass or a relay scan holds a lease
  for as long as it runs.
- **Metadata changes keep the object.** A new label, handle, flag or
  `enabled` goes through `Update` and never rebuilds the account, so caches
  (a Bluesky session, Mastodon instance limits) survive. Writing the handle
  learned from an identity refresh does not emit a `Change`, so refresh and
  change can't trigger each other.
- **Capabilities without an open account.** `Caps(ctx, id, typ)` returns
  the account's capabilities when it is open, else the type's manifest
  defaults. Recovery decisions that read `Idempotent`, `CommitMarks` and
  `IdempotencyKeyTTLSecs` (`engine.go:343-389`) therefore behave as today
  for an account that is locked or not yet opened.

### 2.2 Status and what happens to posts

| Status | Meaning | A due item for it |
|---|---|---|
| `connected` | open and usable | sent |
| `locked` | its secrets can't be opened (master key missing or wrong, part C) | waits: fails as `transient` with "account locked: <reason>", retried on the normal schedule, so fixing the key and restarting lets it go out |
| `needs_reconnect` | the network rejected its credentials (`auth` on a send or a Check) | fails as `auth`, not retried automatically; History offers retry once reconnected |
| `disabled` | the owner turned it off | fails as `auth` "account disabled" |
| `not_configured` | an env account whose variables are gone, or a driver no longer registered | fails as `auth` |
| `deleted` | deleted (part C) | its scheduled targets were canceled at deletion |

### 2.3 The engine and dispatcher keyed by account id

- `store.Claim`, `SendingItem` and `Unscheduled` gain `AccountID` (the
  queries behind them select `post_targets.account_id`). Every engine path
  looks up the account by id: `publishItem`, `repost`, recovery, retries,
  `CanLocate`/`Locate`, interactions.
- `Dispatcher.Accounts` and `AccountIDs` go; the dispatcher holds the
  registry (an `AccountSource` interface for tests). `SendsAs` and
  `errOtherAccount` go.
- **Ids for lookups, types for the rest.** The account id selects the
  account. `SourceRef.Type`, `TargetResult.Platform`, metric labels and
  `post.platforms` keep the destination type.
- `displayName(id, typ)` returns "Bluesky (@geoffrey.one)": the type's
  name plus the handle or label, or the type's name alone for an account
  the registry no longer has.
- **Plan input is a list of targets:**

  ```go
  type TargetSpec struct {
      AccountID string
      Type      string
      Text      string        // the text this account posts: its own or the shared text
      Overrides Overrides     // options and content fields; Overrides.Text is removed
      Layout    thread.Layout
  }
  type PostSpec struct {
      MasterText   string
      Targets      []TargetSpec // replaces Platforms, Overrides, Layouts, Accounts
      DelaySeconds int
      Source       string
      Images       []Img
      MediaRecords []store.Media
      Number       bool
  }
  ```

  `InteractSpec` gets `SourceAccount` (the account that acts natively) and
  `Targets []TargetSpec` for link-quote copies, replacing the maps and
  `Fanout []type`. The stored `interaction_json` records `source_account`;
  matching a target to the interaction uses the account id
  (`interact.go:164`).
- **Idempotency keys** of new items are `store.ItemKey(postID, accountID,
  ordinal)`, including the fallbacks at `publish.go:244` and
  `models.go:197`. Stored keys are read, never recomputed.
- **`SavePost` refuses a target without an account id** instead of
  attaching it to an env account.
- **Fan-out and relay retries** become account-aware. `EnqueueFanout` sets
  `item_id`; a fan-out delivery leases the target's account and, when it
  isn't open, any open Nostr account (the event is already signed, and no
  relay authentication is involved). The retrier passes the target id to
  `RetryRelay` instead of target 0. The fan-out worker always runs; each
  Nostr account's `primary_fanout` decides what is enqueued.
- **Alerts** name the account. The Threads token-expiry gauge reports the
  soonest expiry over Threads accounts.

### 2.4 Services wired per account

| Service | Part A |
|---|---|
| Identity | Through `destination.Identifier` per account, cached 10 minutes per account. The handle and remote id go to `accounts.handle` and `accounts.remote_id`. The composer header uses the first account (registry order) with a display name. `identity.Service` goes. |
| Resolve a pasted URL | Split in two. `resolve.Detect(input) (typ string)` does no I/O. Resolution is an optional `destination.SourceResolver` on the account (`ResolveSource(ctx, input) (SourceRef, error)`). `POST /v1/resolve` takes an optional `account`, else the first connected account of the detected type. `POST /v1/posts` with `reply_to`/`quote`/`repost` resolves with the request's target of the detected type (the acting account), so a Mastodon `LocalID` belongs to the acting account's instance. |
| Verify | Unchanged: public endpoints. |
| Readiness | `/readyz?external=1` asks a provider for checks: one per enabled account through `destination.Checker` with `Authenticated: false` (today's probes), named `account:<id>`; `blossom` only when Blossom is configured; `nostr_relay` only when a Nostr account exists. Never critical. The rename is in the upgrade notes. |
| Relay sync | Per Nostr account. `relaysync` takes the account's public key and home relay on each call instead of at construction. The handlers lease the account and assert `nostr.RelayLister` (defined in the Nostr package; the API already imports it for relay URL checks). `features.relay_sync` is true only with a Nostr account. |
| Threads token refresh | The `TokenManager` becomes the Threads account's `Refresher` (§4.3), owned by the registry entry, so a reopened account keeps the refreshed token (it reads it back from the host on open). |
| Public feed | `feed.Link` gains `account` and `handle`. Each account has a `public_feed` flag (default on, migration v11); targets of accounts with it off are left out. |

### 2.5 Nostr and Blossom optional

- No `NSEC_HEX`: no env Nostr account. Text posts work everywhere.
- `BLOSSOM_URL` optional. In part A it still signs uploads with the env
  Nostr key; set without a key is a startup error that says so (part D
  gives the media store its own key).
- No media store: `POST /v1/media` and `:compress` answer `503
  unavailable` with the detail "no media store is configured"; `GET
  /v1/instance` reports `features.media: false`; the composer hides the
  attach button and says why.
- `/v1/drafts` conversion (`ConvertOldDrafts`) and any code that read
  `a.media.BlossomURL` handle a nil media store.

### 2.6 API changes in part A

- `POST /v1/posts`, `:preview`, drafts: `duplicate_type` and the "isn't
  connected on this server" refusal go; a duplicate account is still
  refused. Field codes (`validation_failed` with a pointer):
  `account_disabled` stays for disabled and not-configured accounts and
  gains `locked` and `needs_reconnect` variants in its detail; drafts
  accept such accounts with a warning (a draft isn't sent).
- Interactions: one target per source network acts natively; a second
  target of that network is `one_native_actor`.
- `GET /v1/accounts[/{id}]` gains `status`, `status_reason`, `remote_id`,
  `order`, `default_selected`, `public_feed`, that account's profile and
  that account's capabilities. `PATCH /v1/accounts/{id}` accepts `label`,
  `enabled`, `default_selected`, `public_feed` for every account
  (`accounts:write`, a new scope, §5.1).
- `POST /v1/resolve` takes `account`; the response names the account used.
- `GET /v1/instance` gains `features.media`.
- OpenAPI updated; `docs/api/errors.md` lists new codes.

### 2.7 Composer, History and Interact (decision 7)

- **State keyed by account id.** `state.platforms` becomes
  `state.targets` (account ids in registry order); per-target overrides,
  breaks and media positions are keyed by account id; type-specific option
  cards are chosen by the account's type. `ORDER` remains only as the
  web app's fallback type order.
- **Recovery snapshot.** The `localStorage` snapshot gets a version 3,
  keyed by account id. A version-2 snapshot (keyed by type) is converted on
  restore by mapping each type to the first connected account of it.
- **Target rows** list every account, grouped by network, labelled avatar
  plus handle. An account that isn't connected shows greyed with its
  reason and can't be selected.
- **Default selection** is every connected account whose
  `default_selected` flag is on (on for every account that exists at the
  upgrade; off for accounts created later, part C). Part D adds account
  sets and a default set.
- **Editor tabs and Compare columns** are one per selected account,
  labelled avatar plus short handle; the tab row scrolls horizontally on
  narrow screens; Alt+1…Alt+9 follow tab order. Counting and limits use
  each account's capabilities.
- **History** shows each target's account and filters by account.
- **Interact:** with several connected accounts on the source's network,
  a picker chooses the acting account (default: the first).
- **Tools → relays** picks the Nostr account.

### 2.8 Migration v11 and the legacy-state step

- `ALTER TABLE accounts ADD COLUMN remote_id TEXT`,
  `default_selected INTEGER NOT NULL DEFAULT 1`, `public_feed INTEGER NOT
  NULL DEFAULT 1`.
- `nostr_sync_relays (account_id TEXT NOT NULL REFERENCES accounts(id),
  url TEXT NOT NULL, added_at TIMESTAMP NOT NULL, PRIMARY KEY (account_id,
  url))`.
- `SeedEnvAccounts` sets the label only when it creates a row, and never
  touches rows whose `source` isn't `env`.
- **Legacy state is adopted at startup, not in the migration** (the store
  must not name destination types): `drivers.AdoptLegacy` copies
  `sync_relays` to the env Nostr account's list when that list is empty,
  then empties `sync_relays`. The old table is dropped in a later
  migration; a downgrade is already refused by the schema-version guard.

### 2.9 The e2e harness

The harness registers fake drivers whose `Open` returns recording
accounts, and seeds two Mastodon accounts (different instance limits), two
Bluesky accounts, one Threads and one Nostr account, plus one
`not_configured` account, so composer, preview, publish, retry, recovery
and fan-out run with several accounts of a type.

## 3. Secrets at rest (part C)

### 3.1 The master key

- `PUBLISHER_MASTER_KEY` (32 bytes, base64, standard or URL-safe, padded
  or not) or `PUBLISHER_MASTER_KEY_FILE` (a file whose trimmed content is
  that). Both set, or malformed, is a startup error.
- `publisher keygen` prints a new key. First-run setup (§8.5) offers one
  with the compose snippet when none is set.
- Keys derived with HKDF-SHA-256 (`golang.org/x/crypto/hkdf`):
  `enc = HKDF(master, salt "publisher", info "secrets v1 enc")`,
  `kid = hex(HKDF(master, salt "publisher", info "secrets v1 key id")[:8])`.
- **No key, or a key that matches no stored secret.** The instance runs.
  Env accounts and settings work. Database accounts whose secrets can't be
  opened are `locked`; their due items wait (§2.2). Writing any secret is
  refused (`409 master_key_required` or `409 master_key_mismatch`) so a
  database never mixes keys. `/readyz` reports a non-critical `secrets`
  check, startup logs an error naming the cause, and the web app shows a
  banner. Nothing is deleted.
- **Secrets sealed under a key id that is neither current nor previous**
  are treated the same way (their accounts `locked`, writes refused).
- **Rotation:** set the new key and the old one as
  `PUBLISHER_MASTER_KEY_PREVIOUS[_FILE]`. At startup, before the registry
  loads, every value sealed with the previous key (in `secrets` and in
  open `oauth_flows`) is re-encrypted under the new one in one
  transaction. If any value fails to open, nothing is changed, the error
  is logged and the instance runs as with a mismatched key. The operator
  removes the previous key afterwards. A backup taken before a rotation
  needs the key that was current then; the README says so.

### 3.2 Cipher and storage

- AES-256-GCM (Go standard library), a random 96-bit nonce per seal
  (well below the 2^32 seals per key where random nonces become a concern).
- Additional authenticated data:
  `"publisher/secrets/v1\x00" + kid + "\x00" + owner + "\x00" + scope +
  "\x00" + ref + "\x00" + name + "\x00" + audience`, where `audience` is
  the normalized origin the secret is sent to (§3.3), or empty.
- Stored as `v1.<kid>.<base64url(nonce || ciphertext || tag)>`.
- Table (migration v13):

  ```sql
  CREATE TABLE secrets (
    owner_id   TEXT NOT NULL,   -- an owner id, or 'instance'
    scope      TEXT NOT NULL,   -- account | setting | oauth_client | export
    ref        TEXT NOT NULL,   -- account id, setting key, client id
    name       TEXT NOT NULL,   -- app_password, access_token, secret_key, …
    audience   TEXT NOT NULL DEFAULT '',
    sealed     TEXT NOT NULL,
    key_id     TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    PRIMARY KEY (owner_id, scope, ref, name)
  );
  CREATE INDEX idx_secrets_key ON secrets(key_id);
  ```

- Package `internal/secrets`: `Keyring` (current, previous), `Seal`,
  `Open`, and a `Store` that reads and writes through `store.Store` in the
  caller's transaction.
- Plaintext lives only inside an open account object and, briefly, in the
  request that writes it. It is never logged, put in an event, attempt,
  trace or error message. The scrubber gains `secret_key`,
  `client_secret`, `app_secret`, `code_verifier`, `refresh_token`, and a
  pattern for query strings and form bodies (`access_token=`,
  `client_secret=`, `password=`); upstream `"code":` error codes are not
  redacted.
- **No plaintext copies.** The store sets `PRAGMA secure_delete = ON`.
  When the Threads token moves into `secrets` (§4.4), the old row is
  overwritten then deleted, followed by `wal_checkpoint(TRUNCATE)`; the
  README recommends a fresh backup afterwards.

### 3.3 Audience binding

A credential is only ever sent to the server it was entered for.

- Fields that name where a credential goes are marked `Audience: true`
  (§4.1): Mastodon `instance_url`, Bluesky `pds_url`, a webhook URL, the
  Blossom URL, a future WordPress site URL.
- Changing an audience field deletes the secrets bound to it and requires
  them again in the same request; otherwise `422 credentials_required`
  with a pointer to each missing field.
- The audience's normalized origin is part of the secret's AAD, so a value
  copied or edited in the database file fails to open rather than going to
  a new server.
- Webhook URLs (Discord, Slack, n8n carry their secret in the path) are
  themselves stored as secrets; views show only their origin.

### 3.4 What it protects

A copied database file or backup reveals labels, handles and origins, not
credentials. Someone who can read the container's environment or memory
has the key and the data. The README says so and recommends a Docker
secret file over an env variable (it isn't in `docker inspect`).

## 4. Contract v2 (part A, completed in C and E)

`destination.ContractVersion` goes from 1 to 2 in part A. Only the four
built-in drivers exist, so the bump is cheap now.

### 4.1 Describing an account (part C)

```go
type Manifest struct {
    // … as today …
    Auth           []AuthMethod `json:"auth"`                      // most recommended first
    Config         []FieldSpec  `json:"config,omitempty"`          // per account, non-secret
    InstanceConfig []FieldSpec  `json:"instance_config,omitempty"` // per instance: a Threads app, a LinkedIn client
}

type AuthMethod struct {
    ID      string    `json:"id"`      // driver-defined: "app_password", "oauth"
    Flow    FlowKind  `json:"flow"`    // fields | redirect | generate
    Label   string    `json:"label"`
    Help    string    `json:"help,omitempty"`
    HelpURL string    `json:"help_url,omitempty"`
    Fields  []FieldSpec `json:"fields,omitempty"` // what the owner types; secrets marked
    // NeedsInstanceConfig lists InstanceConfig keys that must be set first.
    NeedsInstanceConfig []string `json:"needs_instance_config,omitempty"`
    NeedsPublicURL bool `json:"needs_public_url,omitempty"`
}

type FieldSpec struct {
    Key         string      `json:"key"`
    Label       string      `json:"label"`
    Help        string      `json:"help,omitempty"`
    Type        FieldType   `json:"type"` // string | url | text | bool | int | enum | string_list
    Secret      bool        `json:"secret,omitempty"`
    Audience    bool        `json:"audience,omitempty"` // where credentials are sent (§3.3)
    Required    bool        `json:"required,omitempty"`
    Pattern     string      `json:"pattern,omitempty"`  // RE2, whole value
    Placeholder string      `json:"placeholder,omitempty"`
    Enum        []EnumValue `json:"enum,omitempty"`
    Default     any         `json:"default,omitempty"`
    Min         *int        `json:"min,omitempty"`
    Max         *int        `json:"max,omitempty"`
    MaxLength   int         `json:"max_length,omitempty"`
    Advanced    bool        `json:"advanced,omitempty"`
}
```

`destination.ValidateFields(specs, values)` checks types, required,
pattern, enum, bounds and length, refuses unknown keys, and returns
per-field errors. A contract test refuses `Secret: true` in `Config`.

### 4.2 Opening an account (part A)

```go
type Driver interface {
    Manifest() Manifest
    // Open builds an account. No network I/O: an unreachable network must
    // not stop an account from loading (Check and Identity find out).
    Open(ctx context.Context, req OpenRequest) (Account, error)
}

type OpenRequest struct {
    AccountID string
    Method    string            // AuthMethod.ID
    Config    map[string]any
    Secrets   map[string]string
    Tuning    Tuning            // timeouts and directories the operator set
    Host      AccountHost
}

type Tuning struct {
    PublishTimeout, POWTimeout, RelayCacheTTL, VerifyTimeout time.Duration
    PLCDirectoryURL string
}

type AccountHost interface {
    SaveSecrets(ctx context.Context, secrets map[string]string) error // atomic, durable
    SaveConfig(ctx context.Context, patch map[string]any) error       // e.g. token_expires_at
    HTTPClient(policy NetPolicy) *http.Client // the host's SSRF policy (§10.2), timeouts
    Logger() *slog.Logger
}

// EnvSource is implemented by drivers that can be configured from env.
type EnvSource interface {
    FromEnv(getenv func(string) string) ([]EnvAccount, error)
}
type EnvAccount struct {
    Key     string            // env_key: "default"
    Method  string
    Config  map[string]any
    Secrets map[string]string
}
```

- `main.go` iterates the registered drivers, calls `FromEnv` on those that
  implement it, and no longer names a destination variable. An invalid
  value is a startup error naming the variable.
- For env accounts `SaveSecrets` writes where the driver's state lived
  before part C (the Threads token: `threads_token`), and into `secrets`
  once a master key exists (§4.4). `SaveConfig` writes `config_json`.
- The drivers guard test covers `internal/accounts`, `internal/secrets`,
  `internal/settings` and `cmd/publisher` as well as today's packages;
  `internal/drivers` stays the one place that names types.
- `Open` and `Identity` errors map to HTTP as: invalid config or
  credentials format → 422, credentials rejected by the network → 422
  `credentials_rejected` on `/credentials`, network unreachable → 502
  `upstream_failed`.

### 4.3 Optional interfaces

```go
// Identity gains a stable id: a DID, an account URL, a user id, a pubkey.
type Identity struct {
    RemoteID    string `json:"remote_id"`
    Handle      string `json:"handle"`
    DisplayName string `json:"display_name,omitempty"`
    AvatarURL   string `json:"avatar_url,omitempty"`
    ProfileURL  string `json:"profile_url,omitempty"`
}

type Checker interface { Check(ctx context.Context, req CheckRequest) Health }
type CheckRequest struct { Authenticated bool } // false: reachability only (probes)
type Health struct {
    OK        bool       `json:"ok"`
    Reachable bool       `json:"reachable"`
    AuthOK    *bool      `json:"auth_ok,omitempty"`
    Message   string     `json:"message,omitempty"`
    ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

type Refresher interface {
    NextRefresh(now time.Time) time.Time // zero: never
    Refresh(ctx context.Context) error   // saves through AccountHost
}

type SourceResolver interface {
    ResolveSource(ctx context.Context, input string) (SourceRef, error)
}

// Connector runs a redirect flow (part E). The host owns state, PKCE, the
// redirect URI and sealing; the driver builds URLs and exchanges codes.
type Connector interface {
    BeginConnect(ctx context.Context, req ConnectRequest) (ConnectStart, error)
    CompleteConnect(ctx context.Context, req ConnectComplete) (ConnectResult, error)
}
type ConnectRequest struct {
    Method         string
    Config         map[string]any    // e.g. instance_url
    InstanceConfig map[string]any    // e.g. the Threads app id
    InstanceSecrets map[string]string
    RedirectURI    string
    State          string
    CodeChallenge  string            // S256, from the host's verifier
    Host           AccountHost
}
type ConnectStart struct {
    AuthorizeURL string
    Data         json.RawMessage // driver data the host seals until completion
}
type ConnectComplete struct {
    ConnectRequest
    Data         json.RawMessage
    Callback     url.Values
    CodeVerifier string
}
type ConnectResult struct {
    Config     map[string]any
    Secrets    map[string]string
    Identity   Identity
    Candidates []ConnectResult // when the network offers several identities (pages); the owner picks
}
```

Part A adds `Checker`, `SourceResolver` and `Identity.RemoteID` to the
built-in drivers (unauthenticated checks are today's probes). An account
without a `Refresher` whose `Health.ExpiresAt` approaches gets the generic
"expires soon" alert and status (LinkedIn-style tokens without refresh).

### 4.4 Credential refresh

A refresh loop in the registry calls `Refresher.Refresh` for accounts
whose `NextRefresh` has passed, one at a time, with jitter, holding a
lease. A failure alerts once a day while the credential still works; when
an authenticated `Check` says it no longer does, the account becomes
`needs_reconnect`. Threads keeps today's rules (refresh when at least 24 h
old and within 10 days of expiry, alert within 7 days). Its state moves:
the token into the account's secrets (env account: `threads_token` until a
master key exists, then a startup step moves it into `secrets` with the
`secure_delete` treatment of §3.2), `token_expires_at` into `config_json`.

## 5. Accounts API (part C)

### 5.1 Scopes and who may do what

- New token scopes: `accounts:write`, `settings:read`, `settings:write`,
  `instance:admin`. **`admin` no longer implies every scope**: it keeps
  meaning tokens and relay management; the new scopes must be granted
  explicitly. `principal.can` changes accordingly; existing tokens are
  unaffected because none holds a new scope.
- Creating a token that holds `admin`, `accounts:write`, `settings:write`
  or `instance:admin` needs a re-authenticated session (§8.4).
- Operations only a session can do (`403 session_required` for a token):
  revealing a key, export, import, adding or removing sign-in methods.
- **Proxy mode** (§8.1) can't write credentials: creating an account with
  credentials, changing them, adopting, connecting, revealing, secret
  settings, instance settings, export and import answer `403
  sign_in_required` ("set up sign-in to manage credentials"). Labels,
  flags, defaults, account sets and non-secret owner settings work.

### 5.2 Create

`POST /v1/accounts` (`accounts:write`; `Idempotency-Key` supported):

```json
{
  "type": "bluesky",
  "label": "Personal",
  "method": "app_password",
  "config": {},
  "credentials": {"identifier": "geoffrey.one", "app_password": "abcd-efgh-ijkl-mnop"}
}
```

1. Validate `type`, `method` (a `fields` or `generate` flow), `config`
   and `credentials` with `ValidateFields`; check instance prerequisites.
2. Require a usable master key.
3. Resolve anything the account needs to know once (Bluesky: the PDS from
   the handle, stored as `pds_url`).
4. `Open` and call `Identity` (15 s). Rejected → `422
   credentials_rejected` on `/credentials` with at most 200 characters of
   the network's parsed `error`/`error_description`, control characters
   removed; unreachable → `502 upstream_failed` with a generic detail.
   Nothing is stored.
5. Refuse an existing account with the same `(type, remote_id)` for the
   owner: `409 duplicate_account` naming it (a partial unique index
   `(owner_id, type, remote_id) WHERE deleted_at IS NULL` closes the race).
6. In one transaction: the row (`source = 'db'`, `auth_method`,
   `remote_id`, `handle`, `config_json`, `default_selected = 0`) and its
   secrets. Register it; answer `201` with the account view.

**Generate a Nostr key** (`method: generate`): stored like an imported
key; the response carries no secret. The owner reveals it with
`POST /v1/accounts/{id}:reveal` (session, re-authenticated, `Cache-Control:
no-store`), which the web app calls right after creation and shows once
with an "I've saved it" confirmation. Idempotent replay works normally.

Rate limit: 10 creates or credential changes per minute per owner (each
calls the network and may spend the account's own limits there).

### 5.3 Read

`GET /v1/accounts[/{id}]` adds `auth_method`, `credentials`
(`{"app_password": {"set": true, "updated_at": "…"}}`, never values),
`config`, `health` (last check and time), `locked_fields` (for env
accounts), `ETag`. `?include=deleted` lists deleted accounts.

### 5.4 Update

`PATCH /v1/accounts/{id}` (`If-Match` optional, as for drafts):

- `label`, `enabled`, `default_selected`, `public_feed`, `defaults` (D):
  every account.
- `config`, `credentials`: database accounts only (`409 locked_by_env`
  otherwise). Re-validated, re-opened and re-identified; a different
  `remote_id` is `409 identity_changed`; changing an audience field needs
  the credentials again (§3.3).
- A metadata-only change goes through `Registry.Update`; a credential or
  connection change through `Reopen`.

### 5.5 Delete, check, reveal

- `DELETE /v1/accounts/{id}`: soft delete (`deleted_at`, `enabled = 0`),
  secrets deleted, scheduled targets of the account canceled with the
  reason, drafts drop it when opened. Needs re-authentication. An env
  account can't be deleted (`409 locked_by_env`; unset its variables).
- `POST /v1/accounts/{id}:check`: authenticated `Check`, rate limited to
  one per 10 s per account.
- `POST /v1/accounts/{id}:reveal`: Nostr keys only (the driver marks which
  secret fields may be revealed).

### 5.6 Adopting an env account

`POST /v1/accounts/{id}:adopt` (session, master key) copies the env
account's config and credentials into the database, sets `source = 'db'`,
keeps id, `env_key`, history and flags. From then on its variables are
ignored; at startup, if they are still set, a warning names them. Seeding
never creates a second account for them (it skips a `(type, env_key)` row
whose source is `db`). Restoring the pre-adoption backup is the way back.

### 5.7 Credential methods (part C)

| Type | Methods | Config | Identity (`remote_id`) |
|---|---|---|---|
| Bluesky | `app_password`: identifier (handle, DID or email), app password | `pds_url` (audience; discovered at create, editable as advanced) | DID |
| Mastodon | `token`: access token | `instance_url` (audience, required) | the account's URL |
| Threads | `token`: long-lived token | `user_id` (advanced, default `me`) | the Threads user id |
| Nostr | `secret_key` (`nsec1…` or hex); `generate` | `bootstrap_relay`, `fallback_relays`, `primary_relays`, `primary_fanout`, `pow_default`, `pow_max` (defaults from today's env defaults) | the public key (hex) |

- **Bluesky PDS discovery:** DNS `_atproto` TXT, then
  `https://<handle>/.well-known/atproto-did`, then the DID document
  (`PLC_DIRECTORY_URL` or `did:web`), reading `#atproto_pds`. All through
  the host's HTTP client (§10.2).
- **Bluesky password shape:** a value that isn't `xxxx-xxxx-xxxx-xxxx`
  gets a warning ("this looks like your main password"), not a refusal.
- **URLs the owner types:** https, or http for a host allowed by
  `PRIVATE_DESTINATIONS`; normalized to the origin.

## 6. Connect flows (part E)

### 6.1 Plumbing

- **Redirect URI:** `PUBLIC_URL + /oauth/callback/<type>`, or, when
  `PUBLIC_URL` is unset, the origin of the request that started the flow
  (it passed the same-origin check). A network that requires https
  redirects (Threads) needs an https origin; the method says so.
- `POST /v1/accounts:connect` `{type, method, label?, config}` →
  `201 {"flow": "flw_…", "authorize_url": "…", "expires_at": "…"}`.
  `POST /v1/accounts/{id}:reconnect` `{method?}` does the same for an
  existing account.
- `GET /v1/connect-flows/{id}` → `{"status": "pending | completed |
  failed | choose", "account": "acc_…", "error": "<code>", "candidates":
  […]}`; `POST /v1/connect-flows/{id}:choose {candidate}` when a network
  returns several identities.
- `GET /oauth/callback/{type}?code&state[&iss]` (browser navigation,
  outside `/v1`) completes the flow and shows a small page "Connected —
  return to Publisher" (or the failure code), then redirects to
  `/#settings/accounts`. The tab that started the flow polls the flow
  status, so a home-screen app whose browser has another cookie jar still
  completes.
- **Flow table** (migration v15): `oauth_flows (id, owner_id, user_id,
  type, method, account_id, state_hash UNIQUE, data_sealed, status,
  result_account_id, error_code, created_at, expires_at, used_at)`. State is
  32 random bytes, stored hashed, single-use, valid 10 minutes. The PKCE
  verifier and driver data are sealed.
- **Binding:** the flow belongs to the owner and user who started it. The
  callback needs no session (the state is the capability); if a session
  cookie is present, its user must be the flow's user, else
  `session_mismatch` and nothing is stored.
- **Reconnect** must return the same `remote_id` (`identity_changed`
  otherwise).
- `connect_error` and flow `error` are fixed codes: `denied`,
  `expired`, `state_invalid`, `session_mismatch`, `identity_changed`,
  `duplicate_account`, `exchange_failed`, `issuer_mismatch`,
  `master_key_required`. Upstream text is never echoed into the page.
- Without a usable master key, connect is refused up front.
- Rate limit: 10 connect starts per minute per owner.

### 6.2 Mastodon

1. The owner enters the instance URL (audience).
2. Discovery: `GET <instance>/.well-known/oauth-authorization-server`.
   - Found (4.3.0+): `issuer` must equal the instance origin and every
     endpoint must be on that origin, else `issuer_mismatch`. PKCE S256 is
     used when listed.
   - 404 (before 4.3): the standard paths `/oauth/authorize` and
     `/oauth/token` on the instance origin, without PKCE.
   - Any other failure fails the flow.
3. Client: `oauth_clients (id, owner_id, type, issuer, redirect_uri,
   client_id, scopes, created_at)`, the client secret in `secrets` (scope
   `oauth_client`). When none exists for (issuer, redirect URI), register
   one: `POST /api/v1/apps` with `client_name` "Publisher (<host>)",
   `redirect_uris` [the callback], `scopes` "read:accounts read:search
   read:statuses write:statuses write:media", `website` (PUBLIC_URL when
   set). An `invalid_client` later re-registers once.
4. Authorize: `response_type=code`, `client_id`, `redirect_uri`, `scope`,
   `state` [, `code_challenge`, `code_challenge_method=S256`].
5. Callback: when the response has `iss` (RFC 9207), it must equal the
   issuer. `POST /oauth/token` (`authorization_code`, client id and
   secret, redirect URI [, `code_verifier`]). Tokens don't expire and
   have no refresh token.
6. `verify_credentials` → identity; create or reconnect.

No out-of-band (code paste) flow: token paste (part C) is the fallback.

### 6.3 Threads with the owner's Meta app

- **Instance prerequisite:** the Threads driver's `InstanceConfig` declares
  `app_id` and `app_secret` (secret). Settings → Integrations → Threads app
  (an `instance:admin` write, re-authenticated) stores them; the page shows
  the redirect URI to register and links Meta's guide.
- Authorize at Meta's Threads authorization window with `client_id`,
  `redirect_uri`, `scope=threads_basic,threads_content_publish`,
  `response_type=code`, `state`.
- Callback: exchange the code for a short-lived token and user id, then
  for a long-lived token (`th_exchange_token`, server side). Secrets in
  query strings are scrubbed from every log and error (today's
  `scrubToken`).
- **To verify at the start of part E** (Meta's access-token page didn't
  load during the analysis): the authorize host, the code exchange
  endpoint and parameters, https and exact-match rules for the redirect
  URI, whether replies to the account's own threads need
  `threads_manage_replies`. The part E plan records what was found; only
  this subsection changes.
- Verified (2026-11-06): each integrator needs a Meta app with the Threads
  use case; a Threads tester can grant permissions without App Review;
  short-lived tokens last 1 hour, long-lived 60 days, refreshable once 24
  hours old; grants from public profiles last 90 days, after which the
  owner reconnects (the date shows ahead of time).

### 6.4 Bluesky OAuth (not in step 4)

atproto OAuth needs a client metadata document fetched from the public
web, mandatory DPoP and PAR, and a confidential client (ES256 key,
`private_key_jwt`) for sessions longer than two weeks. The `indigo` module
in `go.mod` ships a client (`atproto/auth/oauth`). When built, it is another
`AuthMethod` (`flow: redirect`, `needs_public_url`), with the client key in
`InstanceConfig`.

### 6.5 Nostr remote signer (designed, not built)

A `remote_signer` method takes `bunker://<pubkey>?relay=…&secret=…`. The
account keeps a client keypair (secret) and the signer's pubkey and relays
(config). `Prepare` (signing) becomes a NIP-46 request (kind 24133, NIP-44
encrypted) with a timeout; no answer fails the attempt as `transient`.
Blossom upload auth uses the media store's key (part D), so nothing else
assumes the account holds its key.

## 7. Settings (part D)

### 7.1 Store

```sql
CREATE TABLE settings (
  owner_id   TEXT NOT NULL,   -- an owner id, or 'instance'
  key        TEXT NOT NULL,
  value_json TEXT NOT NULL,
  updated_at TIMESTAMP NOT NULL,
  updated_by TEXT,
  PRIMARY KEY (owner_id, key)
);
ALTER TABLE accounts ADD COLUMN defaults_json TEXT NOT NULL DEFAULT '{}';
CREATE TABLE account_sets (
  id         TEXT PRIMARY KEY,   -- set_…
  owner_id   TEXT NOT NULL REFERENCES owners(id),
  name       TEXT NOT NULL,
  accounts   TEXT NOT NULL,      -- JSON array of account ids
  is_default INTEGER NOT NULL DEFAULT 0,
  created_at TIMESTAMP NOT NULL,
  updated_at TIMESTAMP NOT NULL,
  UNIQUE (owner_id, name)
);
```

Secret settings live in `secrets` (scope `setting`).

### 7.2 Definitions

Declared in code (`internal/settings`): key, scope, type, default, env
variable, secret, audience, validation, apply hook.

| Key | Scope | Env | Type |
|---|---|---|---|
| `composing.languages` | owner | `USER_LANGUAGES` | string list (ISO 639-1) |
| `translate.deepl_key` | owner | `DEEPL_API_KEY` | secret |
| `publishing.auto_retry` | owner | `AUTO_RETRY_ENABLED` | bool |
| `publishing.max_attempts` | owner | `AUTO_RETRY_MAX_ATTEMPTS` | int 1–20 |
| `publishing.schedule_grace` | owner | `SCHEDULE_GRACE` | duration 1m–7d |
| `feed.token` | owner | `PUBLIC_FEED_TOKEN` | secret ("Generate" shows it once) |
| `feed.webhook_url`, `feed.webhook_token` | owner | `FEED_WEBHOOK_URL`, `FEED_WEBHOOK_TOKEN` | secret (audience), secret |
| `alerts.webhook_url`, `alerts.webhook_user`, `alerts.webhook_pass` | owner | `ALERT_WEBHOOK_*` | secret (audience), string, secret |
| `media.store` | instance | — (`BLOSSOM_URL` set ⇒ `blossom`) | enum `none \| blossom \| local` |
| `media.blossom_url` | instance | `BLOSSOM_URL` | url (audience) |
| `media.blossom_key` | instance | — (env deployments: the Nostr key, as today) | secret: generated or pasted |
| `media.public` | instance | — | bool (local store only, §7.6) |
| `<type>.<key>` for each driver's `InstanceConfig` | instance | — | per `FieldSpec` (`threads.app_id`, `threads.app_secret`) |

Turning `publishing.auto_retry` off stops new automatic retries; items
already waiting for a retry are not claimed while it is off and resume
when it is turned back on.

### 7.3 Applying a change

Consumers read `settings.Current()` (an atomic snapshot) at the point of
use, or rebuild on a change notification: the translator, feed notifier,
alert notifier, retry policy and media store. A change applies to the
next request or claim; nothing in flight is interrupted.

### 7.4 API

- `GET /v1/settings` (`settings:read`) and `PATCH /v1/settings`
  (`settings:write`): the owner's settings, as nested JSON
  (`{"composing": {"languages": ["en", "fr"]}}`). Each leaf in `GET` is
  `{"value", "source", "env", "locked"}`; a secret's value is
  `{"set": bool, "updated_at"}` (a webhook URL shows its origin). `PATCH`
  is a JSON Merge Patch: `null` reverts a key to its default or clears a
  secret. `If-Match` optional (missing: latest; `*` allowed); stale →
  `412 precondition_failed`. A locked key → `409 locked_by_env`.
- `POST /v1/settings/{key}:copy-from-env` saves the env value.
- `GET`/`PATCH /v1/instance/settings` (`instance:admin`; secret writes
  re-authenticated): same shape, instance scope.
- `POST /v1/settings/{group}:test` for `alerts` and `feed`, `POST
  /v1/instance/settings/media:test`: `{"ok": bool, "error": "<class>"}`
  where the class is `unreachable | refused | timeout | http_error |
  blocked_destination`; never a status code or body. One per 10 s.
- `/v1/account-sets[/{id}]` (`accounts:read`; writes `accounts:write`),
  with ETags.
- `PATCH /v1/accounts/{id}` accepts `defaults`: visibility, content
  warning, languages and the type's options, validated against the
  account's capabilities and `OptionSpec`s. **Precedence:** a target's own
  value, then the request's shared `content`, then the account default; an
  explicit `""` or `[]` clears a default. `POST /v1/posts:preview` lists the
  defaults it applied per target, and the create response's target
  warnings say "default applied: visibility=unlisted".
- `GET /v1/instance` keeps `user_languages` and `features.translate`
  (the owner's, while there is one owner).

### 7.5 The Settings tab

Sections: **Accounts** (status, health, add, reconnect, check, adopt,
disable, delete, flags), **Composing** (languages, translation, account
sets and the default set, per-account defaults), **Media**,
**Publishing**, **Integrations** (feed, alerts, instance configs such as
the Threads app), **Access** (sign-in methods, API tokens moved here,
sessions, recent sign-in events), **System** (version, readiness, an
allowlist of bootstrap and tuning values with secrets never shown, key
status, export). Tools keeps relay sync. Forms render from `FieldSpec`s
and `OptionSpec`s. Every field shows its source; a locked field shows "set
by `USER_LANGUAGES`" and offers "copy into the app".

### 7.6 Media store: Blossom or local disk

Added in this spec: without it a Bluesky-only self-hoster needs a Blossom
server to attach a photo.

- `media.Store`: `Put(ctx, body, mime)`, `PutFile`, `Fetch(ctx, url)`,
  `Owns(url) bool`, `Public() bool`. Every `Put` path strips metadata as
  today (EXIF/GPS/XMP), video posters included.
- **Blossom** signs uploads with `media.blossom_key`, adding the BUD-11
  server tag so a captured authorization can't be replayed elsewhere.
  Existing deployments keep signing with the Nostr env key until another
  key is set. The page shows the key's npub so the owner can allow it on
  their Blossom server.
- **Local disk:**
  - Objects at `MEDIA_DIR` (default `<data dir>/media`) as `<sha256>`
    files (0600, directory 0700), served at `GET /media/<sha256>.<ext>`.
    The name must match `^[0-9a-f]{64}\.[a-z0-9]{2,5}$` and the extension
    the stored type, else 404. No path is ever built from a URL.
  - Served types are an allowlist of sniffed image and video types;
    anything else goes out as `application/octet-stream` with
    `Content-Disposition: attachment`. Headers: `nosniff`, `Content-Security-Policy:
    default-src 'none'; sandbox`, immutable caching.
  - Unauthenticated, like a Blossom server; names are content hashes. The
    README says that draft media is public to anyone who has its bytes.
  - Uploads are refused below a free-space floor (1 GiB or 5 % of the
    volume) and above an optional quota (`media.local_quota`). The floor
    protects SQLite when media and the database share a volume.
  - **Public serving is opt-in** (`media.public`) and needs `PUBLIC_URL`.
    Turning it on runs a self-test: Publisher fetches
    `PUBLIC_URL/media/<probe>` through the public address and requires a
    200 with the probe's bytes (no redirect, no 401), which catches a
    forward-auth proxy in front of `/media/`. The README documents the
    proxy rule that exposes only `/media/`. Only a public store can carry
    media for URL-transport accounts (Threads, Nostr); a post with media
    to such an account without one is refused (`media_unsupported`). The
    page warns that Nostr images served from home reveal the home IP.
- Switching stores applies to new uploads; existing media keep their URLs
  and are fetched where they live (drafts keep Blossom attachments).

## 8. Sign-in without OIDC (part B)

### 8.1 Modes

`AUTH_MODE` (bootstrap): `oidc`, `local`, `proxy`.

- Default: `oidc` when `OIDC_ISSUER` is set; otherwise `local` for a
  **fresh** database (created by this version) and `proxy` for an
  **existing** one. Existing OIDC-off deployments therefore keep working
  after the upgrade, with a banner "Set up sign-in" and a startup warning.
- `local` needs `PUBLIC_URL` (https, or `http://localhost`): session
  cookies are `Secure` and passkeys need a secure context. Without it the
  instance starts, and the setup page explains how to get https
  (`tailscale serve`, Caddy, Traefik) instead of looping on a dropped
  cookie.
- `proxy`: no app sign-in (today's behaviour) and no credential
  management (§5.1). A startup warning when `ALLOWED_HOSTS`/`PUBLIC_URL`
  is unset, as today.
- `GET /v1/instance` reports `auth: oidc | local | proxy`.

### 8.2 Local sign-in

- **Passkeys** with `github.com/go-webauthn/webauthn` (v0.18.x). Relying
  party id: the host of `PUBLIC_URL`; allowed origins: its origin only.
  The relying party id is stored with each credential.
- **Password**, optional: argon2id (`golang.org/x/crypto/argon2`, t=3,
  m=64 MiB, p=4, 16-byte salt, 32-byte key, PHC string); at least 12
  characters; changing it needs the current one. At most 2 hashes run at
  once; more answer `429`.
- **Client address:** `TRUSTED_PROXIES` (CIDRs). The client is the
  rightmost `X-Forwarded-For` address that isn't a trusted proxy; without
  trusted proxies, the connection's address.
- **Throttling:** checked before hashing. Per client address: 5 failures
  in 15 minutes, then exponential delay up to 15 minutes. Per owner: a
  delay (never a block) after 20 failures in 15 minutes. Passkey
  assertions are exempt from the per-owner delay. `login_failures` is
  pruned to the last 24 hours and capped at 10,000 rows. Failures are
  logged with the address and shown in Settings → Access.
- Sessions reuse today's table and cookie; local users are `users` rows
  with `source = 'local'`.
- Tables (migration v12): `webauthn_credentials (id, user_id, rp_id,
  credential_id UNIQUE, public_key, sign_count, transports, aaguid, name,
  created_at, last_used_at)`, `password_credentials (user_id PRIMARY KEY,
  phc, updated_at)`, `login_failures (addr, at)`, `reauth_nonces
  (nonce_hash PRIMARY KEY, session_id, operation, expires_at)`,
  `setup_tokens (token_hash PRIMARY KEY, expires_at, used_at)`,
  `users.source`, `instance_meta (key PRIMARY KEY, value)` (records
  whether the database was created fresh, for §8.1).

### 8.3 Setup token and recovery

- In `local` mode with no local credential, each start mints a token (32
  random bytes, hashed, 1 hour, single-use) and invalidates older ones. It
  logs `setup: open /setup#<token> on the address you use for Publisher`
  (with `PUBLIC_URL` prefixed when set), or, with `SETUP_TOKEN_FILE`, writes
  the URL to that file (0600) and logs only the path.
- The setup page exchanges the token once for a setup cookie (Path=/setup,
  10 minutes), removes the fragment from the address bar
  (`history.replaceState`), and binds the first credential's registration
  to that cookie. Setup closes for good once a credential exists.
- `publisher setup-url` mints a new token on a running instance (the
  command writes it into the database and prints the URL).
  `publisher owner reset-login [--revoke-tokens]` deletes local
  credentials and sessions, lists the active tokens holding `admin`,
  `accounts:write`, `settings:write` or `instance:admin` (revoking them
  with the flag), and prints a setup URL. Both run in the container
  (`docker exec publisher /publisher …`); whoever can run them already
  controls the instance.

### 8.4 Re-authentication

- Needed for: adding a passkey, setting or changing the password, removing
  a sign-in method, creating a token with `admin`, `accounts:write`,
  `settings:write` or `instance:admin`, revealing a key, deleting an
  account, export, import, instance secret settings.
- `POST /auth/reauth` with a passkey assertion or the password (local),
  or after a fresh OIDC login (`prompt=login`, `max_age=0`, and the ID
  token's `auth_time` must be after the request started) returns a nonce
  for one named operation, valid 5 minutes, single-use. The operation's
  request carries it in `Publisher-Reauth`; without it: `403
  reauth_required` with the operation name.
- Proxy mode: these operations are refused (§5.1).
- **To verify at the start of part B:** that Authelia honours
  `prompt=login`/`max_age=0` and returns `auth_time`.

### 8.5 First-run setup

1. In `local` mode: create a passkey (or password) with the setup token.
2. If no master key is set: show a generated key and the compose snippet,
   and explain that accounts need it (skippable: env accounts work
   without).
3. Add a first account (part C onwards).
4. Choose a media store or skip (part D onwards).

An instance configured by env (Geoffrey's) never sees steps 2–4; a proxy
or OIDC instance never sees step 1.

## 9. Export and documentation (part F)

- `POST /v1/export` (session, re-authenticated): the server generates a
  passphrase (24 words from a 2048-word list, about 264 bits), shows it
  once, and returns a file: a JSON header `{format: "publisher-export",
  version: 1, kdf: "argon2id", t: 3, m: 65536, p: 4, salt, nonce}` and an
  AES-256-GCM payload with the header as AAD. Contents: accounts (config,
  secrets, flags, defaults), account sets, owner and instance settings
  (with secrets), Nostr sync relays. No posts, drafts, media or tokens.
- `POST /v1/import` (session, re-authenticated): refuses KDF parameters
  other than the pinned ones; imports into an owner with no database
  accounts; first answers a summary that lists every endpoint it would set
  (instance URLs, webhook origins, Blossom URL, Threads app) for the owner
  to confirm; skips accounts that match an env account's `remote_id`;
  re-identifies each imported account before marking it connected.
- README: Self-hosting rewritten around the minimal compose file (one
  volume, the master key as a Docker secret, `PUBLIC_URL`); "Adding
  accounts" per network, including creating the Meta app; the
  configuration table with tiers; backups (database and key separately,
  keys across rotations); upgrading from env-only; the security model;
  `PRIVATE_DESTINATIONS`, `TRUSTED_PROXIES`, local media and proxy rules.

## 10. Security

### 10.1 Summary

- Secrets: §3; never in responses, logs, events, traces, or exports
  without the generated passphrase.
- Audience binding: §3.3.
- Scopes, proxy mode and re-authentication: §5.1, §8.4.
- Events (`account.created|updated|deleted|adopted`, `settings.updated`,
  `login.succeeded|failed`, `token.created`) record the principal and
  token id and the names of changed fields, never values.
- OAuth: §6.1–6.2 (state hashed and single-use, PKCE where supported,
  issuer and `iss` checks, same-origin endpoints, fixed error codes).
- CSRF: today's Origin check covers new routes; the OAuth callback only
  completes a flow by its state.
- Rate limits: login (§8.2), account writes (§5.2), connect (§6.1), checks
  and tests (one per 10 s).

### 10.2 Outbound requests to owner-set addresses

Instance URLs, PDS discovery and `did:web`, Blossom, webhook URLs, the
local media self-test, and driver requests made through
`AccountHost.HTTPClient`:

- go through a client whose dialer checks the connected address
  (`net.Dialer.Control`), on every redirect too, not only a lookup
  before;
- never reach loopback, link-local (including cloud metadata
  `169.254.169.254`), unspecified or multicast addresses, or the server's
  own listen addresses;
- reach private ranges (RFC 1918, ULA, CGNAT) only when the host or its
  address is in `PRIVATE_DESTINATIONS` (hostnames or CIDRs, operator-set);
- build request paths only from validated origins (a Blossom URL with a
  query or fragment is refused), and report failures as classes, never as
  the target's body.

Env-configured addresses (today's variables) are trusted as today.

## 11. Testing

- Unit: keyring (seal/open, AAD mismatch, audience change, unknown key
  id, rotation success and rollback), field validation, every driver's
  `Open`/`FromEnv`/`Check`/`ResolveSource`, registry leases and swaps under
  `-race`, settings precedence and merge patch, argon2id limiter and
  throttling with trusted proxies, setup tokens, re-auth nonces, the
  dial-time network policy.
- Store: migration tests for v11–v15 from fixtures, the schema golden,
  the legacy-state adoption.
- API: every new route, scope and proxy-mode refusals, problem codes,
  idempotency, ETags; the OpenAPI contract test.
- e2e (Playwright): composer with several accounts of a type (tabs,
  Compare, publish, retry, recovery snapshot v2 → v3), adding accounts by
  each credential method against fakes, Mastodon OAuth against a fake
  instance (4.3+ and pre-4.3 discovery), Threads OAuth against a fake Graph
  API, settings with env locks and copy-from-env, local media store and its
  public self-test, passkey sign-in with Chromium's virtual authenticator,
  setup token, re-authentication, first-run setup, export and import.
- Every PR: 2 QA reviews and 1 security review; findings fixed with
  regression tests before handover.

## 12. Upgrade path for an env-configured deployment

- **A:** no configuration change; ids kept; sync relays adopted; the
  composer shows the same four accounts with handles. Readiness check
  names change to `account:<id>` (external monitors that match `bluesky`
  etc. need updating). An invalid boolean env value now stops startup.
- **B:** OIDC deployments: no change; re-authentication uses a fresh
  OIDC login. OIDC-off deployments stay in proxy mode until the owner sets
  `AUTH_MODE=local` (and `PUBLIC_URL`).
- **C:** no change needed. With a master key: the Threads token moves into
  the encrypted store at the next start (take a fresh backup); adopting env
  accounts is optional. Tokens holding `admin` don't gain the new scopes.
- **D:** env values show as locked settings; "copy into the app" before
  removing a variable.
- **E:** the Threads app goes in Settings → Integrations before connecting.
- **F:** documentation and export only.

## 13. To verify during implementation

- Threads OAuth endpoints and redirect rules (§6.3), at the start of E.
- Authelia's handling of `prompt=login`, `max_age=0` and `auth_time`
  (§8.4), at the start of B.
- go-webauthn's current API (v0.18.2, 2026-09-19), at the start of B.
- That a Bluesky PDS rejects nothing new from the discovery path
  (handle → DID → PDS) for custom-domain handles, in C.
