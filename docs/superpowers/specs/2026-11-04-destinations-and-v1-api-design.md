# Destinations and the `/v1` API (roadmap step 2)

**Status:** Draft for review (roadmap step 2, started 2026-11-04)
**Date:** 2026-11-04
**Area:** Architecture — platform contract, accounts, publish engine, HTTP API

## Problem

Step 1 made the codebase safe to change. Step 2 changes its shape, so that
steps 3 (thread editor) and 4 (settings and accounts) and later plug-ins
have something to stand on. Five things block that today. Each was checked
in the code on `main` at `0293ab5`.

1. **Platforms are hard-coded into the core.** Each platform has its own
   poster interface with its own method names (`NostrPoster`,
   `MastodonPoster`, `BlueskyPoster`, `ThreadsPoster`,
   `internal/dispatch/dispatch.go`), and the dispatcher picks one with
   `switch plat` (`internal/dispatch/platform.go:18`, and again for
   repost/quote). Limits are switch statements too: `thread.LimitFor`,
   `thread.MaxImagesFor` (`internal/thread/thread.go:22,37`) and
   `transcode.ProfileFor` (`internal/transcode/profiles.go:44`). 25 files
   outside tests contain a quoted platform name. Adding a fifth platform
   means touching all of them.
2. **One account per platform, by construction.** Credentials are single
   env vars (`MASTODON_TOKEN`, `BLUESKY_IDENTIFIER`, …), and the database
   enforces one target per platform per post
   (`UNIQUE(post_id, platform)`, `internal/store/migrations.go:110`). Nostr
   and Blossom are mandatory (`NSEC_HEX is required`, `BLOSSOM_URL is
   required`, `internal/config/config.go:182,188`), so a Bluesky-only
   self-hoster can't start the app.
3. **A publish is not durable until it is over.** `PostWithID` runs every
   platform and only then calls `SavePost` (`internal/dispatch/post.go:168`).
   `/api/post` runs it in a detached goroutine (`internal/api/posts.go:139`).
   If the process stops mid-publish (a deploy, a crash, `docker stop` with
   no graceful shutdown), the posts that went out have no record: no
   history, no retry, no resume.
4. **Retries can't tell a dead request from a bad one, and can duplicate.**
   The Retrier re-drives every `failed` target up to the attempt cap
   (`retryablePlatform`, `internal/dispatch/retrier.go:96`) whatever the
   error was: a 400 for an over-long post is retried six times like a
   timeout. And a request that timed out after the platform accepted it is
   recorded `failed` and posted again. Nothing asks the platform to dedupe,
   even though Mastodon supports it (see "Exactly-once publishing").
5. **The API is a set of endpoints that grew one feature at a time.** Four
   naming styles (`/publish`, `/upload-media`, `/api/post`,
   `/api/posts/{id}/relay-retry`), multipart-with-a-JSON-field for posting,
   `{"error": "..."}` bodies with no codes, offset pagination, no
   idempotency, no discovery of what a platform supports, and no published
   schema. API tokens only work on `/publish` and `/upload-media`; every
   `/api/*` route needs a browser session when OIDC is on
   (`internal/api/middleware.go:108`), so a script can't use the features
   the web app has (threads, scheduling, drafts, quotes).

## Goals

- **One Destination contract** that the four built-in platforms implement,
  with a **capability manifest** that the splitter, media fitting, preview,
  composer and API read instead of switching on names. Public quality and
  versioned, so the later out-of-process plug-in protocol is the same
  contract serialized, not a new design.
- The contract covers **long-form** (title plus formatted body: WordPress,
  Nostr NIP-23, LinkedIn articles) and **media destinations**, not only
  microblogs, even though no such destination ships in this step.
- **Accounts as first-class rows** with stable ids: many accounts per
  destination type in the data model and the engine, and data scoped by
  owner so multi-user stays possible.
- A **durable publish engine**: the post and all its planned items are
  written before the first network call; restarts resume; errors are
  classified; publishing is exactly-once where the platform allows it;
  graceful shutdown.
- A **`/v1` API** that is consistent, documented by an OpenAPI document
  that tests keep honest, idempotent, and usable by scripts for everything
  the web app can do. The web app moves to it; the old routes go.

## Non-goals (and where they land)

- **New destinations** (X, WordPress, LinkedIn, media platforms). The
  contract is designed and tested against their shapes on paper (appendix
  B) but none is built. Later plug-ins.
- **Adding and editing accounts at runtime, encrypted credential storage,
  OAuth flows.** Step 4. In this step accounts are created from env vars
  (one per type, as today) but are real rows, and the engine is tested with
  several accounts per type. See Decision 1.
- **The thread editor UI** (segment cards, per-platform breaks). Step 3.
  This step gives it what it needs: explicit `items[]` per target in the API
  and the store, and the counting rules in the manifest.
- **Out-of-process plug-ins** (sidecar/WASM). Later. This step keeps the
  contract JSON-serializable so that protocol is mechanical.
- **Media-store plug-ins** beyond Blossom (S3, platform-native). The
  `MediaStore` interface is introduced with Blossom as its only
  implementation.
- **Delete/edit propagation, engagement pull-back, durable outbound
  webhooks.** The contract reserves optional interfaces for delete and edit;
  the event model is designed so webhooks can be added on top later.

---

## 1. Concepts and vocabulary

| Term | Meaning | Today |
|---|---|---|
| **Destination type** (driver) | Code that knows one network: `nostr`, `bluesky`, `mastodon`, `threads`. Declares a manifest. | "platform" |
| **Account** | One configured identity on a destination type: an id, a label, credentials, an owner. Many per type. | implicit, one per platform from env |
| **Post** | What the owner publishes once: shared content, a set of targets, an optional schedule or interaction. | `posts` row |
| **Target** | One account's share of a post: its effective content and options. | `post_targets` row keyed by platform |
| **Item** | One published object on the network: a microblog post in a thread, or an article. Every target has at least one item. | `segments_json`, or nothing for single posts |
| **Delivery** | One endpoint an item is sent to, for destinations that fan out (Nostr relays). | `target_relays` |

"Platform" stays in user-facing copy where it reads better ("Bluesky");
code and API use *destination type* and *account*.

## 2. The Destination contract

New package `internal/destination`. Internal for now (Go's `internal/`
keeps third parties out until we want them in); it is written as if it
were public: no dependency on `store`, `dispatch` or any platform package,
every exchanged type JSON-tagged, doc comments on everything, and a
`ContractVersion`.

### 2.1 Driver and Account

```go
const ContractVersion = 1

// Driver is one destination type. Registered once at startup.
type Driver interface {
    Manifest() Manifest
    // Open binds an account's config and credentials. It must not do
    // network I/O; Check does that.
    Open(cfg AccountConfig, host Host) (Account, error)
}

// Account is one connected identity. Safe for concurrent use.
type Account interface {
    // Capabilities returns this account's effective capabilities: the
    // manifest's defaults, refined by what the server says (a Mastodon
    // instance's max_characters). The host caches the result.
    Capabilities(ctx context.Context) (Capabilities, error)
    Identity(ctx context.Context) (Identity, error)       // handle, name, avatar, profile URL
    Check(ctx context.Context) Health                     // credentials valid? token expiry?
    Publish(ctx context.Context, req PublishRequest) (PublishResult, error)
}
```

Optional interfaces, discovered with a type assertion and advertised in the
manifest so the API can say what an account supports without calling it:

| Interface | Methods | Built-ins |
|---|---|---|
| `Interactor` | `Repost(ctx, SourceRef)`, and `PublishRequest.Quote` / `InReplyTo` with a `SourceRef` | Nostr, Bluesky, Mastodon (Threads has no external quote/repost) |
| `Resolver` | `Resolve(ctx, url) (*Source, error)`: URL → native ref + preview | Nostr, Bluesky, Mastodon |
| `Redeliverer` | `Redeliver(ctx, ItemRef, endpoint)`: re-send to one delivery endpoint | Nostr (relays) |
| `Deleter`, `Editor` | reserved; not implemented in this step | — |

Why the host, not the driver, runs threads: resume after a crash, per-item
progress, per-item persistence and retry all live in one place, and a
driver stays a "post one thing" adapter. Postiz, the closest open-source
prior art, hands the provider a whole array of posts in one
`post(id, accessToken, postDetails[])` call
([docs.postiz.com](https://docs.postiz.com/configuration/create-provider.md)),
which pushes threading and partial-failure handling into every provider.
We don't want that.

### 2.2 What the host gives a driver (`Host`)

```go
type Host interface {
    Media() MediaReader            // bytes of a prepared media item, or its public URL
    HTTPClient() *http.Client      // with the app's timeouts and user agent
    Logger() *slog.Logger          // pre-tagged with account id and post id
    State() StateStore             // small per-account key/value (e.g. Threads' refreshed token)
}
```

`StateStore` generalizes the one-off `threads_token` table: a driver keeps
refreshed tokens or cached session data without its own migration.

### 2.3 Publishing one item

```go
type PublishRequest struct {
    Kind        Kind          // note | article
    Text        string        // note body, plain text (drivers add facets/mentions)
    Title       string        // article only
    Body        *Body         // article only: {Format: "markdown", Source: "..."}
    Media       []MediaItem   // already fitted to this account's media caps by the host
    ContentWarning string
    Sensitive   bool
    Languages   []string      // BCP 47
    Options     map[string]any // validated against Manifest.Options
    LinkCard    *LinkCard     // only when caps.LinkPreview == "client"
    InReplyTo   *ThreadRef    // previous item in this target's thread (root + parent)
    Quote       *SourceRef    // native quote of an external post (head item only)
    ReplyTo     *SourceRef    // native reply to an external post (head item only)
    IdempotencyKey string     // stable across retries of this item; see §5.3
}

type PublishResult struct {
    Ref        ItemRef     // ID, URL, and Data: driver-private JSON (Bluesky cid, Nostr signed event)
    Status     ItemStatus  // published | partial (published, but a side effect failed)
    Deliveries []Delivery  // per-endpoint outcome (Nostr relays); empty elsewhere
    Warnings   []string
    Trace      Trace       // redacted request/response for the attempt log
}
```

`ItemRef.Data` replaces today's special cases (`CID`, `SignedEventJSON`,
`ReplyRef.AuthorPubkey`): the host stores it opaquely and hands it back in
`ThreadRef` and `Redeliver`. Only the driver reads it.

**Errors.** A driver returns `*destination.Error`:

```go
type Error struct {
    Class      ErrorClass    // transient | rate_limited | auth | invalid | rejected | unknown_outcome
    Message    string
    RetryAfter time.Duration // rate_limited (from Retry-After / x-ratelimit-reset)
    Err        error
}
```

The retry policy reads the class (§5.4). `unknown_outcome` means "the
request may have been applied" (timeout after sending, connection reset
after the body went out); the host then only retries with the item's
idempotency key, or asks the owner.

### 2.4 The manifest and capabilities

`Manifest` is static per type; `Capabilities` is the same struct, refined
per account. Both are served by the API and drive the composer.

```go
type Manifest struct {
    Contract    int          // ContractVersion the driver was built against
    Type        string       // "mastodon"
    Name        string       // "Mastodon"
    Kinds       []Kind       // [note] for microblogs; [note, article] for Nostr later; [article] for WordPress
    Auth        AuthSpec     // kind: app_password | oauth2 | token | nostr_key; fields for step 4's form
    Options     []OptionSpec // destination-specific options, declarative (below)
    Optional    []string     // optional interfaces implemented: "interact", "resolve", "redeliver"
    Defaults    Capabilities
}

type Capabilities struct {
    Text struct {
        MaxLength  int        // 0 = unlimited
        Counting   Counting   // how MaxLength is measured (below)
        Formats    []string   // note body: ["plain"]; article body: ["markdown", "html"]
    }
    Title     struct{ Supported, Required bool; MaxLength int }
    Threading struct{ Supported bool; MaxItems int }        // 0 = unlimited
    Media     MediaCaps
    LinkPreview string      // none | server (platform unfurls) | client (we must send the card: Bluesky)
    ContentWarning, Sensitive bool
    Languages   int         // how many languages a post may carry; 0 = none
    Visibility  []string    // e.g. ["public","unlisted","private","direct"]; empty = not supported
    Interactions struct{ Reply, Quote, Repost bool }        // of external posts
    Idempotent  IdempotencyMode // none | key (server dedupes a key) | natural (re-sending is harmless: signed Nostr events)
}

type MediaCaps struct {
    Transport    string    // bytes (we upload) | url (the network fetches a public URL: Threads, Nostr)
    Images       Limit     // per item: MaxCount, MaxBytes, MaxPixels, Mimes
    Video        Limit     // + MaxDurationSecs, MaxCount
    MixImagesVideo bool
    AltText      struct{ Supported bool; MaxLength int }
}
```

**Counting is declarative, not code**, so an out-of-process plug-in can
describe it and the splitter, the preview and the browser all count the
same way:

```go
type Counting struct {
    Unit      string // graphemes | codepoints | utf16 | bytes | weighted
    URLLength int    // fixed cost per URL (Mastodon: 23); 0 = counted literally
    MentionLocalPartOnly bool // "@user@host" costs len("@user") (Mastodon)
    Weights   []WeightRange // "weighted": code point ranges and their cost (X)
}
```

Mastodon reports `configuration.statuses.max_characters`,
`max_media_attachments` and `characters_reserved_per_url` in
`GET /api/v2/instance`, plus `media_attachments.image_size_limit`,
`video_size_limit`, `description_limit` and `supported_mime_types`
([Mastodon docs](https://docs.joinmastodon.org/methods/instance/); the
endpoint exists since 4.0.0). The Mastodon account's `Capabilities` reads
them (cached, refreshed hourly and on `Check`), replacing the fixed 500 /
4 / 8 MB constants. Today a URL counts at its full length on Mastodon, so
posts with long links split earlier than they need to; `URLLength: 23`
fixes that.

**Options** are declarative so the composer (step 3) and per-account
defaults (step 4) render them without per-platform code:

```go
type OptionSpec struct {
    Key     string   // "reply_gate"
    Type    string   // enum | bool | string | int | string_list
    Enum    []EnumValue
    Default any
    Label, Help string
    Scope   string   // target (whole thread) | item
}
```

Today's `Overrides` struct maps onto common fields and options like this:

| Today (`dispatch.Overrides`) | New home |
|---|---|
| `text` | target content override (§4) |
| `spoiler_text`, `content_warning` | common `content_warning` |
| `sensitive` | common `sensitive` |
| `language`, `langs` | common `languages` |
| `visibility` | common `visibility` (checked against caps) |
| `pow` | Nostr option `pow` |
| `topic_tag`, `threads_reply_control` | Threads options `topic_tag`, `reply_control` |
| `bluesky_reply`, `bluesky_disable_quotes` | Bluesky options `reply_gate`, `disable_quotes` |
| `link_card` | host-computed `LinkCard`, persisted per item |

### 2.5 Built-in manifests (initial values = today's behaviour)

| | Bluesky | Mastodon | Threads | Nostr |
|---|---|---|---|---|
| Max length | 300 graphemes | instance (default 500), URL = 23 | 500 | unlimited |
| Images / item | 10 | instance (default 4) | 10 | unlimited (imeta) |
| Media transport | bytes | bytes | url (variants re-hosted) | url (Blossom + imeta) |
| Link preview | client | server | server | none |
| Interactions | reply, quote, repost | reply, quote, repost | reply only (own threads) | reply, quote, repost |
| Idempotent | key (client `rkey`)* | key (`Idempotency-Key`) | none (container id persisted)** | natural (signed event) |

\* `com.atproto.repo.createRecord` takes a client-chosen `rkey`; the code
already uses this for threadgates (`internal/bluesky/bluesky.go:440`). The
host derives a TID-format rkey from the item's idempotency key, so a
re-sent create collides instead of duplicating. To verify against a PDS in
the implementation PR: that a repeat create with an existing `rkey` is
rejected rather than overwriting.
\*\* Threads publishes in two calls (create container, publish container).
Persisting the container id between them makes a retry after a lost
publish response re-publish the same container, which the API rejects
instead of duplicating; to verify in the implementation PR.

### 2.6 Media stores

```go
type MediaStore interface {
    Put(ctx, body io.Reader, mime string) (StoredMedia, error) // URL, sha256, size
    Fetch(ctx, url string) (io.ReadCloser, string, error)
}
```

Blossom is the only implementation. Destinations with `Transport: url`
get a URL from the store; the host fits media to each account's limits
(today's `transcode.Profile` logic, now fed from `MediaCaps`) and re-hosts a
variant when it changes, as the Threads adapter does today
(`prepThreadsImgs`). Bluesky's in-client `fitBlob` moves to the host too,
so preview badges and published bytes come from one code path.

`BLOSSOM_URL` and the Nostr key become optional: without Blossom, URL-
transport destinations are unavailable (reported by `/readyz` and the
accounts API), and without a Nostr key there is no Nostr account.

### 2.7 Registry

```go
destination.Register(bluesky.Driver{})   // in cmd/publisher, explicit, no init() magic
```

The engine, API and composer iterate the registry. After this step the
only places that name a destination type are its own package and the
env-seeding code in `cmd/publisher`. A test greps for quoted type names
outside those packages to keep it that way.

## 3. Accounts and owners

### 3.1 Schema (migration v3)

```sql
CREATE TABLE owners (id TEXT PRIMARY KEY, created_at TIMESTAMP NOT NULL);
INSERT INTO owners VALUES ('own_default', CURRENT_TIMESTAMP);

CREATE TABLE accounts (
  id          TEXT PRIMARY KEY,           -- acc_<ulid>
  owner_id    TEXT NOT NULL REFERENCES owners(id),
  type        TEXT NOT NULL,              -- driver type
  label       TEXT NOT NULL,              -- "Personal Bluesky"
  handle      TEXT,                       -- cached from Identity
  source      TEXT NOT NULL,              -- env | db (step 4)
  env_key     TEXT,                       -- for source=env: which env set seeded it
  config_json TEXT NOT NULL DEFAULT '{}', -- non-secret config (instance URL, PDS URL)
  enabled     INTEGER NOT NULL DEFAULT 1,
  created_at  TIMESTAMP NOT NULL,
  updated_at  TIMESTAMP NOT NULL,
  UNIQUE(owner_id, type, env_key)
);
```

`owner_id` is added to `posts`, `drafts`, `api_tokens` and `users`
(OIDC users map to the default owner). Every store query takes an owner id.
There is one owner; nothing in the API exposes owners yet. This is the
"data gets an owner id from the start" decision.

**Credentials are not in the database in this step.** An env-sourced
account reads its secrets from the same env vars as today at startup.
Step 4 adds encrypted credential columns and `source = 'db'` accounts.

### 3.2 Seeding from env

At startup, for each destination whose env vars are set, upsert one
account with `source = 'env'`, `env_key = 'default'`. Its id is generated
once and then stable, so API clients can store it. Unsetting the env vars
disables the account (keeps history); it does not delete it.

### 3.3 Targets, items and deliveries (migration v4)

`post_targets` is rebuilt (SQLite can't drop a `UNIQUE` constraint in
place; the step-1 rules require the table-rebuild recipe):

- add `account_id` (backfilled from the env account of `platform`), keep
  `platform` as `type` for history of deleted accounts;
- `UNIQUE(post_id, account_id)`;
- add `content_json` (the target's effective content and options, replacing
  `fields_json` + `final_text`).

New table `target_items` replaces `segments_json` and the per-target
`remote_id` / `remote_url`:

```sql
CREATE TABLE target_items (
  id            INTEGER PRIMARY KEY,
  target_id     INTEGER NOT NULL REFERENCES post_targets(id),
  ordinal       INTEGER NOT NULL,
  content_json  TEXT NOT NULL,     -- text/title/body, media indices, link card
  status        TEXT NOT NULL,     -- planned | sending | published | partial | failed | unknown | canceled
  idem_key      TEXT NOT NULL,     -- stable per item
  remote_id     TEXT, remote_url TEXT,
  ref_json      TEXT,              -- ItemRef.Data (cid, signed event, container id)
  error_class   TEXT, error TEXT,
  attempt_count INTEGER NOT NULL DEFAULT 0,
  next_attempt_at TIMESTAMP,
  published_at  TIMESTAMP,
  UNIQUE(target_id, ordinal)
);
```

`target_relays` becomes `item_deliveries` (keyed by item). `target_attempts`
gains `item_id`. The backfill turns each `segments_json` entry into an item
and each non-threaded target into a one-item thread, so every target has
`items[]` from now on and the API never has to special-case "single post".
The schema golden test and a migration test with a copy of a real v1
database shape (threaded, single, scheduled, nostr partial) cover it. The
step-1 runner writes `publisher.db.pre-v3.bak` before applying.

## 4. Content model: shared content, target overrides, items

A post has **shared content**; each target may **override** parts of it;
the engine **plans items** per target from the effective content and the
account's capabilities.

```
post.content          {text, title, body, media[], content_warning, sensitive, languages}
  └ target.content    same fields, each optional; null = inherit
     target.options   destination options (validated against the manifest)
     target.items     optional explicit items (manual thread); absent = auto-split
```

- **Auto items (default).** The host splits the effective text with the
  account's `Counting` and `MaxLength`, places media with today's planner
  (`thread.SplitPlace`, now parameterized by caps instead of a type name),
  and numbers items if asked. `---` manual breaks keep working inside
  `text` for now; step 3 replaces them with explicit items in the editor.
- **Explicit items.** `target.items: [{text, media: [indices]}, …]` posts
  exactly those items. Each must fit; the API rejects one that doesn't
  with a pointer to it (§6.4). This is the `segments[]` the brainstorm
  asked for, and what the step-3 editor will send.
- **Kinds.** `kind: note` (default) or `article`. An article needs a
  `title` and a Markdown `body`; targets whose account doesn't support
  articles are rejected at validation. A later "announce" mode (article on
  WordPress, a linking note on microblogs) is a post-level option layered
  on this, not a contract change.
- **Markdown is the canonical long-form format.** Drivers convert (to HTML
  for WordPress; Nostr NIP-23 is Markdown already). Notes stay plain text.

## 5. The publish engine

### 5.1 Persist first

Creating a post writes, in one transaction: the post, every target, every
planned item (`status = planned`, a fresh `idem_key` each), and the media
records. Only then does any network call happen. "Publish now" is
"scheduled for now". One code path replaces `PostWithID`, `Schedule`+`Fire`,
`InteractWithID` and `Retry`.

### 5.2 Workers

A worker pool claims due items (`planned` with `next_attempt_at <= now`, in
ordinal order per target, one in-flight item per target so threads stay
ordered), marks them `sending`, publishes, and records the result and the
attempt in one transaction. Threads post sequentially per target and in
parallel across targets, as today. Progress events (§6.7) are emitted from
these transitions, so the SSE stream reflects the database, not an
in-memory registry. The Scheduler, Retrier and Nostr fan-out loops fold
into this one claim loop with three kinds of work (item, delivery, scheduled
post).

### 5.3 Exactly-once where the platform allows it

Each item carries a stable `idem_key`, passed to the driver on every
attempt:

- **Mastodon:** sent as the `Idempotency-Key` header. Mastodon documents it
  for `POST /api/v1/statuses`: "Provide this header with any arbitrary
  string to prevent duplicate submissions of the same status… Idempotency
  keys are stored for up to 1 hour"
  ([Mastodon docs](https://docs.joinmastodon.org/methods/statuses/)). So an
  in-doubt retry within the hour returns the original status instead of a
  duplicate.
- **Bluesky:** a deterministic `rkey` for the post record (§2.5).
- **Nostr:** the signed event is persisted in `ref_json` before the first
  send; every retry re-sends the same event id, which relays dedupe.
- **Threads:** the container id is persisted between create and publish.

On startup, items left in `sending` become `unknown`. For `Idempotent:
key|natural` they are retried normally (within Mastodon's one-hour window;
after it, they need a decision). For `none`, or Mastodon past the window,
they stop and show "may have been posted: check and confirm" in History and
via the API (`POST /v1/posts/{id}/targets/{tid}/items/{n}:resolve` with
`published` + URL, or `retry`). Today these cases are silent duplicates or
silent losses.

### 5.4 Retry policy by error class

| Class | Auto-retry | Notes |
|---|---|---|
| `transient` | yes, backoff (today's base 2m, max 1h, 6 attempts) | network errors, 5xx |
| `rate_limited` | yes, at `RetryAfter` (not counted against the cap) | 429 |
| `unknown_outcome` | only if idempotent (§5.3) | otherwise needs the owner |
| `auth` | no; account marked unhealthy, alert | expired token, revoked app password |
| `invalid` / `rejected` | no; alert | 4xx validation, content refused |

A later item in a thread waits for its predecessor; a permanent failure on
item k leaves k+1… `planned` until the owner retries or cancels.

### 5.5 Graceful shutdown

Proposed in step 1's follow-ups and folded in here because the workers are
being rewritten: on SIGTERM, `/readyz` returns 503, the HTTP server stops
accepting requests and drains (25 s), workers finish their in-flight item
and stop claiming, then the process exits. Docker's default stop timeout is
10 s, so the compose example and README set `stop_grace_period: 30s`.
Anything still `sending` at exit is handled by §5.3 on the next start.

## 6. The `/v1` API

### 6.1 Principles

- Resources and JSON. Multipart only for media upload.
- **Errors are RFC 9457 problem details** (`application/problem+json`, with
  `type`, `title`, `status`, `detail`, `instance`
  ([RFC 9457](https://www.rfc-editor.org/rfc/rfc9457.html))) plus our
  extensions: a stable `code` and, for validation, `errors[]` of
  `{pointer, code, detail}` (JSON Pointer into the request). `type` URIs
  resolve to `docs/api/errors.md#<code>`.
- **Idempotency on every create** via `Idempotency-Key`, following
  draft-ietf-httpapi-idempotency-key-header-08 (active WG draft, Oct 2026
  ([datatracker](https://datatracker.ietf.org/doc/draft-ietf-httpapi-idempotency-key-header/))):
  same key + same payload → the stored response (with
  `Idempotent-Replayed: true`); same key + different payload → `422`; same
  key while the first request is in flight → `409`. Keys are kept 24 h
  (documented, as the draft asks). Strongly recommended for `POST
  /v1/posts`; the web app always sends one.
- **Cursor pagination** (`?limit=&cursor=`, response `next_cursor`), stable
  under inserts, replacing `offset`.
- **Prefixed ids** (`pst_`, `acc_`, `med_`, `drf_`, `tok_`), RFC 3339 UTC
  timestamps, snake_case.
- **Async by default, wait on request.** `POST /v1/posts` answers `202` with
  the post (status `queued`) and `Location`. A client that wants the result
  in one call sends `Prefer: wait=60` (RFC 7240) and gets `201` with the
  final state, or `202` if the wait ran out. Good for n8n and shell
  scripts without an SSE client.
- **Optimistic concurrency** on drafts: `ETag` / `If-Match`, `412` on a
  stale write (two open tabs today silently overwrite each other).
- **One OpenAPI 3.1 document**, served at `GET /v1/openapi.json`, checked
  in under `docs/api/`, and enforced by a test that runs every route and
  validates requests and responses against it.

### 6.2 Auth

Every `/v1` route accepts either the browser session cookie (with the
existing Origin-based CSRF guard) or `Authorization: Bearer <token>`.
Tokens get **scopes**: `posts:read`, `posts:write`, `media:write`,
`drafts:read`, `drafts:write`, `accounts:read`, `admin` (tokens, relays,
settings), and an optional expiry. Existing tokens migrate to
`posts:write media:write` (what `/publish` + `/upload-media` allowed). With
OIDC off, behaviour stays "trust the reverse proxy" as today; the README
says so more loudly.

### 6.3 Resources

| Method and path | Purpose | Replaces |
|---|---|---|
| `GET /v1/instance` | version, enabled features, user languages, translate targets, limits | `/api/config`, part of `/api/identity` |
| `GET /v1/destinations` | registered types with manifests | — |
| `GET /v1/accounts`, `GET /v1/accounts/{id}` | accounts with identity, effective capabilities, health | `/api/identity` |
| `POST /v1/media` | upload an image or video (multipart or raw body); images return `ready`, videos `processing` | `/upload-media`, `/api/media/video` |
| `POST /v1/media:compress` | stateless re-encode (presets, HEIC conversion); returns bytes, stores nothing | `/api/media/compress` |
| `GET /v1/media/{id}` | media object with processing state, variants, alt | `/api/media/video/{id}` |
| `PATCH /v1/media/{id}` | set alt text | — |
| `POST /v1/posts` | create (publish now, schedule, reply/quote/repost) | `/publish`, `/api/post`, `/api/interact` |
| `POST /v1/posts:preview` | same body; validates and plans, sends nothing; returns per-target items, counts and warnings | `/api/thread-preview` |
| `GET /v1/posts` | list; filters `status`, `account`, `q`, `since`, `until`, `hidden` | `/api/posts` |
| `GET /v1/posts/{id}` | one post with targets, items, deliveries, attempts | `/api/posts/{id}` |
| `GET /v1/posts/{id}/events` | SSE progress (also `GET /v1/events` for all posts) | `/api/posts/{id}/progress` |
| `PATCH /v1/posts/{id}` | reschedule (`schedule.at`), hide/unhide | `/reschedule`, `/hide` |
| `POST /v1/posts/{id}:cancel` | cancel a scheduled post or the unsent rest of a thread | `/cancel` |
| `POST /v1/posts/{id}:retry` | retry failed/unknown items, optionally `{targets:[…]}` or deliveries | `/retry`, `/relay-retry` |
| `POST /v1/resolve` | URL → source with per-account reply/quote/repost availability | `/api/resolve` |
| `GET/POST /v1/drafts`, `GET/PATCH/DELETE /v1/drafts/{id}` | drafts with the same content model as posts, ETags | `/api/drafts` |
| `POST /v1/drafts/{id}:translate`, `POST /v1/translate` | DeepL | same |
| `POST /v1/verify` | signature verification tool | `/api/verify` |
| `GET/POST/DELETE /v1/tokens`, `GET /v1/me` | tokens with scopes | `/api/tokens`, `/api/me` |
| `/v1/accounts/{id}/nostr/relays…` | relay-list sync, now scoped to a Nostr account | `/api/sync/*` |
| `GET /v1/public/feed` | public feed (own token, as today) | `/api/public/feed` |

`/healthz`, `/readyz`, `/metrics` and `/auth/*` stay unversioned.
Colon-suffixed actions (`:cancel`, `:retry`, `:preview`) follow Google's
AIP-136 custom-method convention, which keeps verbs out of the resource
path.

### 6.4 Creating a post

```http
POST /v1/posts
Idempotency-Key: "6f1c…"
Prefer: wait=60

{
  "content": {
    "text": "Long post…",
    "media": [{"id": "med_01J…", "alt": "A gradient"}],
    "languages": ["en"]
  },
  "targets": [
    {"account": "acc_bsky…", "options": {"reply_gate": "following"}},
    {"account": "acc_masto…", "options": {"visibility": "unlisted"},
     "content": {"content_warning": "politics"}},
    {"account": "acc_nostr…",
     "items": [{"text": "Part one…", "media": [0]}, {"text": "Part two…"}]}
  ],
  "threading": {"numbering": true},
  "schedule": {"at": "2026-11-05T09:00:00Z"},
  "draft": "drf_01J…"
}
```

Interactions use the same endpoint:
`"reply_to": {"url": "https://bsky.app/…"}`, `"quote": {…}` or
`"repost": {…}`, with an optional `fanout` list of accounts that get a
link-quote reproduction (today's quote fan-out). Validation runs the same
planner as `:preview`, so a request that previews clean publishes clean.

Response (`201` after the wait, abbreviated):

```json
{
  "id": "pst_01J…", "status": "published", "created_at": "…",
  "targets": [{
    "id": "tgt_…", "account": "acc_bsky…", "type": "bluesky", "status": "published",
    "items": [
      {"ordinal": 0, "status": "published", "url": "https://bsky.app/…", "text": "…"},
      {"ordinal": 1, "status": "published", "url": "https://bsky.app/…", "text": "…"}
    ]
  }]
}
```

### 6.5 Statuses

Post and target: `scheduled → queued → publishing → published | partial |
failed | canceled | missed`, plus `needs_attention` when an item is
`unknown` or a permanent error stopped a thread. Item: `planned → sending →
published | partial | failed | unknown | canceled`. One vocabulary in the
store, the API and the UI (today's `success` becomes `published`).

### 6.6 Removing the old routes

The web app moves to `/v1` in the same step. `/api/*`, `/publish` and
`/upload-media` are removed in the last PR of the step (see Decision 2);
the README gets a "Migrating from the pre-1.0 API" section with a
`curl` mapping for each old call, including the n8n `/publish` flow.

### 6.7 Events

Server-sent events with typed payloads, also the shape of future webhooks:
`post.created`, `post.updated`, `item.sending`, `item.published`,
`item.failed`, `delivery.updated`, `post.completed`. Each has an `id`
(`Last-Event-ID` resumes) and is derived from the database transitions in
§5.2.

## 7. The web app

- Reads destinations, accounts and capabilities from `/v1` instead of the
  hard-coded `ORDER` and `defaultOv` (`internal/web/assets/state.js`). The
  per-platform option panels render from `Manifest.Options`. The account
  picker shows accounts, not types, so a second account appears with no
  UI change once step 4 can create it.
- Counting in the browser implements `Counting` (graphemes via
  `Intl.Segmenter`, URL weight, mention rule) instead of the per-platform
  copies; the server-side preview stays authoritative.
- The existing Playwright suite is the safety net: every scenario passes
  against `/v1` before the old routes go. New scenarios: idempotent
  double-submit, restart mid-thread (harness kills the server between
  items), an `unknown` item resolved by hand, an `invalid` error that is
  not retried.

## 8. Rollout

Four PRs, each green on unit, e2e and Docker build, each deployable:

1. **Contract and drivers.** `internal/destination`, the four drivers
   behind it, registry, manifests replacing the switch statements,
   host-side media fitting, error classes. Dispatcher calls drivers through
   the contract. No API change and one additive migration (v2, the error
   class); behaviour identical except classified retries and Mastodon's
   live limits.
2. **Accounts and the durable engine.** Migrations v3 (owners, accounts,
   owner ids) and v4 (targets by account, `target_items`, deliveries,
   idempotency table); env seeding; persist-first engine with workers;
   exactly-once keys; `unknown` handling; graceful shutdown. Old API routes
   adapted onto the new engine so the web app keeps working.
3. **`/v1` API.** All resources in §6, OpenAPI document and conformance
   test, scoped tokens, problem details, idempotency, `Prefer: wait`, SSE
   events. Old routes still present.
4. **Web app on `/v1`, old routes removed.** Composer and history read
   manifests and accounts; migration guide in the README.

Deploy note for Geoffrey: PR 4 breaks the n8n flows that call `/publish`.
Update them to `POST /v1/posts` (the guide has the exact request) before
deploying it.

## 9. Testing

- **Contract test suite** (`destination/destinationtest`): one table of
  behaviours every driver must pass against a fake server (publish, thread
  ref round-trip, error classification for 400/401/429/5xx/timeout,
  idempotent re-send, capability refresh). The built-ins run it; a future
  plug-in runs the same suite.
- **Fake driver** with scriptable outcomes for engine tests (crash between
  items, unknown outcome, rate limit) and for the e2e harness, which already
  fakes the platforms.
- **Migration tests** from representative v1 databases.
- **OpenAPI conformance** test over every route.
- Existing tests keep passing at each PR; tests that pinned
  platform-switch internals are rewritten against the contract.

## Decisions for Geoffrey

1. **Adding a second account before step 4.** Recommended: no. The data
   model, engine and API are multi-account in this step and tested with
   several accounts per type, but accounts are created from env (one per
   type) until step 4 brings encrypted credential storage and the accounts
   UI. Alternatives: an `accounts.yaml` file mounted into the container
   now (a second config surface to keep forever), or `POST /v1/accounts`
   now (pulls credential encryption into step 2).
2. **When the old routes go.** Recommended: in PR 4, once the web app is on
   `/v1`, with a migration guide; you update n8n before deploying it.
   Alternative: keep `/publish` as a deprecated shim for one release.
3. **Async by default with `Prefer: wait`** for synchronous callers, rather
   than synchronous by default. Recommended as written.

## Appendix A: things this design fixes along the way

- A post is lost from history if the process stops mid-publish (§5.1).
- Validation errors are retried six times (§5.4).
- A timed-out Mastodon post can be posted twice by the Retrier (§5.3).
- Mastodon instances with limits other than 500 characters / 4 images are
  mis-split; URLs over-counted on Mastodon (§2.4).
- A Bluesky-only or Mastodon-only install can't start without a Nostr key
  and Blossom (§2.6).
- Scripts can't use threads, schedules, drafts or quotes when OIDC is on
  (§6.2).
- Draft edits in two tabs silently overwrite each other (§6.1).

**Fixed already, with the spec:** retrying a failed reply, quote or repost
re-posted its text as a plain post, because the retry path
(`dispatchTargets`) called `runPlatform` with no reply or quote reference.
A reply went out as a top-level post, a quote lost its quote, and a repost
became an empty status, and the Retrier did this on its own. Reproduced
with a test, then fixed in its own commit: `Interaction` now stores the
source's native ref, a retry repeats the action (including the head of a
threaded reply), and a post recorded before the fix fails the retry with
a clear message instead of posting the wrong thing.

## Appendix B: future destinations checked against the contract

| Destination | Shape | What it uses |
|---|---|---|
| WordPress | article: title, HTML body, featured image, categories/tags, status draft/publish | `Kinds: [article]`, `Body` Markdown→HTML, options for categories, media transport bytes |
| Nostr long-form (NIP-23) | article as kind 30023, Markdown | `Kinds: [note, article]` on the Nostr driver |
| X | weighted counting (URLs 23), threads, 4 images | `Counting{Unit: weighted, URLLength: 23}`, threading, rate-limit class |
| LinkedIn | posts with a commentary limit, articles, organization vs member authors | per-account identity; author choice as an option |
| Pixelfed / media-first | at least one image required | `Media.Images.MinCount` (add when needed; manifest is versioned) |
| Generic webhook | POST JSON somewhere; no thread refs | `Threading.Supported: false`, `Idempotent: key` (sends the key) |

Pixelfed's "image required" is the one shape that needs a manifest field
not listed above; adding optional fields is a minor contract change.
