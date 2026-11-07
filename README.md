# publisher

A self-hosted, multi-protocol social publisher. Cross-posts to **Nostr,
Bluesky, Mastodon, and Threads** from a single HTTP API, with a SQLite
archive, per-platform retry, scheduled posting, and an embedded mobile web
UI. It signs Nostr events itself (NIP-13 PoW, NIP-65 outbox resolution,
NIP-92 `imeta` media via Blossom) and holds the signing key locally.
Accounts are added in the web app, where their credentials are encrypted
with your master key, or set in the environment.

Builds to a small static `FROM scratch` image (linux/amd64).

## Why

This started as an HTTP→Nostr sidecar for an n8n cross-posting workflow:
n8n's Code-node cannot reliably hold a long-lived WebSocket (the async
lifecycle finishes before the WS handshake completes), and the task-runner
module allowlist doesn't propagate `nostr-tools` into the runner image. The
sidecar moved the WebSocket plumbing into a dedicated Go process callable
over plain HTTP, with key custody kept out of n8n's credential store. It has
since grown into a standalone multi-platform publisher with its own web UI.

## HTTP API

### The `/v1` API

`/v1` is the publisher's API; the web app uses it too. It is described by an
OpenAPI 3.1 document, served at `GET /v1/openapi.json` (no sign-in needed)
and kept in [`docs/api/openapi.yaml`](docs/api/openapi.yaml); a test checks
every route, request and response against it. It has:

| Route | Scope | |
|---|---|---|
| `GET /v1/instance` | any | version, features, languages, limits |
| `GET /v1/destinations` | `accounts:read` | destination types and their manifests |
| `GET /v1/accounts`, `GET /v1/accounts/{id}` | `accounts:read` | accounts with status, profile and capabilities |
| `POST /v1/accounts` | `accounts:write` | add an account with its credentials (needs a master key; checked with the network first; `Idempotency-Key`) |
| `PATCH /v1/accounts/{id}` | `accounts:write` | rename an account, turn it off or on, choose whether new posts select it and whether its posts go to the public feed; for one added in the app, change its settings and credentials (`If-Match`) |
| `POST /v1/accounts/{id}:check` | `accounts:write` | check now that the account works (once every 10 s per account) |
| `DELETE /v1/accounts/{id}` | `accounts:write`, browser only | delete an account added in the app, after confirming it's you (its scheduled posts are canceled, its history stays) |
| `POST /v1/accounts/{id}:reveal`, `:adopt` | `accounts:write`, browser only | show a Nostr key (after confirming it's you); move an account from the environment into the app |
| `GET /v1/me` | any | who is calling, with which scopes |
| `GET/POST /v1/tokens`, `GET/DELETE /v1/tokens/{id}` | `admin` | API tokens with scopes and expiry |
| `POST /v1/posts` | `posts:write` | publish or schedule a post to one or more accounts (`Idempotency-Key`, `Prefer: wait`) |
| `POST /v1/resolve` | `posts:write` | look up a post by URL (optionally with a given `account`): author, text, media, and whether you can reply, quote or repost |
| `POST /v1/posts:preview` | `posts:write` | the same body: what each account would get (thread parts, link card, warnings); sends nothing |
| `GET /v1/posts` | `posts:read` | the history, newest first; filter by `status`, `account`, `q`, `since`, `until`, `hidden` |
| `GET /v1/posts/{id}` | `posts:read` | a post with its targets, items (thread parts), deliveries and attempts |
| `PATCH /v1/posts/{id}` | `posts:write` | reschedule (`schedule.at`), hide or show (`hidden`) |
| `POST /v1/posts/{id}:cancel` | `posts:write` | stop what a post still has to send; it stays in the history |
| `POST /v1/posts/{id}:retry` | `posts:write` | send failed items again, optionally only some `targets` or Nostr `deliveries` |
| `POST /v1/posts/{id}:resolve` | `posts:write` | say whether an item whose outcome is `unknown` went out; with the post's URL a thread goes on under it |
| `GET /v1/posts/{id}/events` | `posts:read` | a post's progress as server-sent events, from its first event until it settles |
| `GET /v1/events` | `posts:read` | every post's events as they happen (and account changes, with `accounts:read`) |
| `POST /v1/media` | `media:write` | upload an image (stored at once) or a video (processed in the background) |
| `GET/PATCH /v1/media/{id}` | `media:write` | a media object's state; set its alt text |
| `POST /v1/media:compress` | `media:write` | re-encode an image with a preset (HEIC to JPEG too); stores nothing |
| `POST /v1/translate` | `posts:write` | translate text with DeepL (when `DEEPL_API_KEY` is set) |
| `POST /v1/verify` | `posts:read` | who signed a Nostr event or a post URL, and how sure that is; uses no credentials |
| `GET/PUT /v1/accounts/{id}/nostr/relays` | `admin` | a Nostr account's relays for relay sync; `PUT` replaces the secondary list |
| `POST /v1/accounts/{id}/nostr/relays:scan` | `admin` | compare each relay with your home relay (changes nothing) |
| `POST /v1/accounts/{id}/nostr/relays:sync` | `admin` | copy missing events: `pull` to the home relay, `push` from it |
| `GET /v1/drafts`, `POST /v1/drafts` | `drafts:read`, `drafts:write` | saved drafts (filter by `q` and `tag`); a draft holds a `POST /v1/posts` body in the making |
| `GET/PATCH/DELETE /v1/drafts/{id}` | `drafts:read`, `drafts:write` | one draft; `PATCH` and `DELETE` take `If-Match` so a stale tab can't overwrite a newer save |
| `POST /v1/drafts/{id}:translate` | `drafts:write`, `posts:write` | a translated copy of a draft (DeepL) |
| `GET /v1/public/feed` | feed token | your published posts for a homepage, with `PUBLIC_FEED_TOKEN` as the bearer |

- **Posting** is two steps: upload media (`POST /v1/media`, which answers
  a `med_` id), then `POST /v1/posts` naming the accounts
  (`GET /v1/accounts`) and the media. Each account can override the text,
  content warning, sensitivity and languages, and set its destination's
  options (`GET /v1/destinations`; `visibility` on Mastodon). Text too long
  for an account becomes a thread there.
- **Threads** are planned on positions in the text, never on markers: a
  line of `---` is ordinary text. `content.breaks` lists where you want a
  new post (Unicode code point offsets into `content.text`, ascending),
  and a media item's `at` puts it on the post holding that position. An
  account can set its own `thread.breaks` (`[]` for none) on the shared
  text, or its own text with its own `thread.breaks` and `thread.media_at`.
  Any post still over the limit is split automatically (paragraphs, then
  sentences, then words), counted the way that platform counts.
  `POST /v1/posts:preview` answers each post's `range`, `length` and
  whether its break was `manual` or `auto`. The answer is `202` at once and
  the engine sends the post; add `Prefer: wait=30` to get `201` with the
  outcome when it settles within 30 seconds (60 at most). Send an
  `Idempotency-Key` so a retried request can't post twice. A video must
  be `ready` (poll `GET /v1/media/{id}`) before a post can use it.
- **Replies, quotes and reposts** use the same endpoint: add
  `"reply_to": {"url": "…"}`, `"quote": {…}` or `"repost": {…}` with the
  post's URL (or a Nostr `nevent`/`note` id). The server looks the post
  up; the action happens on your account on its network, which `targets`
  must include. A quote's other targets get a link-quote: your text, the
  original's text (at most 500 characters, mentions defused) and author,
  its link and, best-effort, up to four of its images (not videos).

- **Statuses** are one vocabulary: a post or target is `scheduled`,
  `publishing`, `published`, `partial`, `failed`, `canceled` or `missed`,
  and a settled post that needs you (an item whose outcome is unknown, or
  one the automatic retries gave up on) shows as `needs_attention`. An
  item (one post on one account, or one part of a thread) is `planned`,
  `sending`, `published`, `partial`, `failed`, `unknown` or `canceled`.
- **Events** are kept 7 days. Each has an `id`; reconnect with
  `Last-Event-ID` (browsers do it for you) or `?after=` to resume where
  you left off. `GET /v1/events` starts from now unless you pass one. A
  post's stream ends after `post.completed`; close your EventSource then
  (one that reconnects gets 204, which stops it).

- **Auth:** the browser session, or `Authorization: Bearer <token>`. Tokens
  carry scopes (`posts:read`, `posts:write`, `media:write`, `drafts:read`,
  `drafts:write`, `accounts:read`, `accounts:write`, and `admin`, which
  includes all of them but `accounts:write`) and an optional expiry. Tokens
  made before scopes keep `posts:write media:write`, which is what the old
  `/publish` and `/upload-media` let them do. A token can't create a token
  that outlives it, and only the signed-in owner can create one holding
  `accounts:write`.
- **Errors** are RFC 9457 problem details with a stable `code`; the
  [error catalogue](docs/api/errors.md) lists them.
- **Lists** page with `?limit=` (1 to 200) and `?cursor=`; the answer is
  `{"data": [...], "next_cursor": ...}`.
- **Creates** honor `Idempotency-Key` (the IETF draft): a retry with the
  same key and body gets the first answer back instead of doing the work
  twice. Keys are kept 24 hours. (Token creation is the exception: its
  answer holds the secret, which is never stored.)

```sh
curl -s -X POST https://publisher.example/v1/tokens \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"n8n","scopes":["posts:write","media:write"],"expires_at":"2027-11-05T00:00:00Z"}'

# Upload an image, then post it to Bluesky and Mastodon and wait for the outcome.
MED=$(curl -s -X POST "https://publisher.example/v1/media?alt=A%20sunset" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: image/jpeg' --data-binary @sunset.jpg | jq -r .id)
curl -s -X POST https://publisher.example/v1/posts \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -H "Idempotency-Key: \"$(uuidgen)\"" -H 'Prefer: wait=30' \
  -d '{"content":{"text":"Evening","media":[{"id":"'"$MED"'"}]},
       "targets":[{"account":"acc_…"},{"account":"acc_…","options":{"visibility":"unlisted"}}]}'

# Watch every post's progress (Ctrl-C to stop).
curl -sN https://publisher.example/v1/events -H "Authorization: Bearer $TOKEN"
```

### Migrating from the pre-1.0 API

The routes the publisher had before `/v1` are gone: `/publish`,
`/upload-media` and everything under `/api/` answer `410 Gone` with a
pointer here. The web app uses `/v1` itself. Tokens you already have keep
working: those made before scopes existed carry `posts:write media:write`,
which is what the old routes allowed.

| Old route | On `/v1` |
|---|---|
| `POST /publish` | `POST /v1/posts` with a Nostr account in `targets` (see below) |
| `POST /upload-media` | `POST /v1/media` (the same `multipart/form-data` with a `file` part) |
| `POST /api/media/video`, `GET /api/media/video/{id}` | `POST /v1/media`, `GET /v1/media/{id}` |
| `POST /api/media/compress` | `POST /v1/media:compress` |
| `POST /api/post` | `POST /v1/posts` |
| `POST /api/thread-preview` | `POST /v1/posts:preview` |
| `GET /api/posts`, `GET /api/posts/{id}` | `GET /v1/posts`, `GET /v1/posts/{id}` |
| `GET /api/posts/attention/count` | `GET /v1/posts?status=needs_attention` (a list, not a count) |
| `POST /api/posts/{id}/retry`, `…/relay-retry` | `POST /v1/posts/{id}:retry` (with `targets` or Nostr `deliveries` to narrow it) |
| `POST /api/posts/{id}/cancel` | `POST /v1/posts/{id}:cancel` |
| `POST /api/posts/{id}/reschedule`, `…/hide` | `PATCH /v1/posts/{id}` with `schedule.at` or `hidden` |
| (new) | `POST /v1/posts/{id}:resolve`, to settle an item whose outcome is `unknown` |
| `GET /api/posts/{id}/progress` | `GET /v1/posts/{id}/events`, `GET /v1/events` |
| `GET /api/config`, `GET /api/identity` | `GET /v1/instance`, `GET /v1/accounts` |
| `POST /api/resolve` | `POST /v1/resolve` |
| `POST /api/interact` | `POST /v1/posts` with `reply_to`, `quote` or `repost` |
| `POST /api/verify` | `POST /v1/verify` |
| `POST /api/translate` | `POST /v1/translate` |
| `/api/drafts…` | `/v1/drafts…` (drafts saved before the upgrade are converted on first start) |
| `/api/sync/relays`, `/api/sync/targets`, `/api/sync/scan`, `/api/sync/apply` | `GET`/`PUT /v1/accounts/{id}/nostr/relays`, `POST …/relays:scan`, `POST …/relays:sync` |
| `/api/tokens`, `/api/me` | `/v1/tokens`, `/v1/me` |
| `GET /api/public/feed` | `GET /v1/public/feed` (the same `PUBLIC_FEED_TOKEN`) |

What changed besides the paths:

- **Errors** are problem details with a `code`, not `{"error": "…"}`.
- **Posting is saved first and sent by the engine.** `POST /v1/posts`
  answers `202` at once; add `Prefer: wait=30` to get `201` with the
  outcome once the post settles (still `202`, `publishing`, if it hasn't
  by then, for example while a retry is due). A `201` comes whatever the
  platforms said, so read `status` instead of the HTTP code: `published`,
  `partial`, `failed`, or `needs_attention` (something gave up and is
  yours to check, such as a refused post). The old `/publish` answered
  `502` when no relay took the note.
- **Replies go only to the network of the post you reply to**; a quote
  posts natively there and as a link-quote on your other targets. The old
  "try anyway" override is gone.
- **Canceling** a post keeps it in the history as `canceled`; the old
  route deleted it.

#### Moving an n8n workflow off `/publish`

1. In the web app, open **Settings → Access** and make a token with
   `posts:write` and `media:write` (or keep the one n8n has).
2. Find your Nostr account's id once: `GET /v1/accounts` lists it
   (`acc_…`, with `"type": "nostr"`). It needs `accounts:read`, which the
   n8n token doesn't have: open `/v1/accounts` in the browser while signed
   in, or ask with a short-lived token that has it.
3. If the workflow uploads media, point the upload node at `POST /v1/media`.
   It takes the same `file` part; add an `alt` part for alt text. Keep the
   answer's `id` (`med_…`); you no longer pass `imeta` along, since the
   publisher builds it.
4. Change the publish node to `POST /v1/posts` with a JSON body:

   ```json
   {
     "content": {"text": "hello world", "media": [{"id": "med_…"}]},
     "targets": [{"account": "acc_…", "options": {"pow": 16}}]
   }
   ```

   and these headers: `Authorization: Bearer <token>`,
   `Idempotency-Key: "<a fresh UUID per post>"` (n8n can retry a node
   without posting twice), and `Prefer: wait=30` if the next node needs the
   outcome. Leave out `pow` to use `POW_DIFFICULTY_DEFAULT`. Only kind 1
   notes are posted; the old `kind` field has no equivalent.
5. Read the answer: `status` says how it went, and
   `targets[0].items[0].remote_id` is the note's event id. `targets[0].items[0].deliveries` lists each relay with its
   `status` and `message`, as the old `relays` list did.

The same two calls with `curl`:

```sh
MED=$(curl -s -X POST https://publisher.example/v1/media \
  -H "Authorization: Bearer $TOKEN" -F file=@photo.jpg -F alt='A sunset' | jq -r .id)
curl -s -X POST https://publisher.example/v1/posts \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -H "Idempotency-Key: \"$(uuidgen)\"" -H 'Prefer: wait=30' \
  -d '{"content":{"text":"hello world","media":[{"id":"'"$MED"'"}]},"targets":[{"account":"acc_…"}]}' \
  | jq '{status, event: .targets[0].items[0].remote_id, relays: .targets[0].items[0].deliveries}'
```

To post the same text to Bluesky, Mastodon or Threads from the same node,
add their accounts to `targets`.

### Automatic failure recovery

Every post is saved, with each part it will publish, before anything is sent.
Publishing then goes one part at a time: a thread posts in order on each
platform, and platforms post in parallel. So a post survives a crash or a
restart. What was still to send is sent when the publisher comes back, and a
thread carries on from the part it stopped at.

Failed parts are retried automatically with exponential backoff
(`AUTO_RETRY_BASE_DELAY` × 2 per attempt, capped at `AUTO_RETRY_MAX_DELAY`),
up to `AUTO_RETRY_MAX_ATTEMPTS` per part. A rate-limited platform is retried
no sooner than it asks. Failures a retry can't fix are not retried: an expired
token, content the platform refuses, invalid input. Nostr partials retry at
the per-relay level, rebroadcasting the same signed event, so no duplicate
note is created. `missed` scheduled posts are **not** auto-retried, because a
scheduling miss isn't a delivery failure; retry them manually.

**When a send may already have gone out.** The publisher can stop, or a
request can time out, after a platform received a post but before the answer
came back. What happens next depends on the platform:

- **Mastodon** drops a repeat with the same idempotency key for an hour, so
  the part is retried within that hour, counted from the first attempt that
  may have gone out (a server error counts: it can come after the post was
  saved).
- **Nostr** notes are signed and saved before the first send, and every
  attempt re-sends that same event. Relays dedupe it by id.
- **Bluesky**, **Threads** and Nostr reposts can't drop a repeat, and neither
  can Mastodon after the hour. The part is not retried: it is marked "may have been
  posted", and an alert asks you to check the platform and retry by hand if
  it isn't there.

When retries stop (the cap, or a failure that would repeat), the target is
marked **given up** and an operational alert fires once (via
`ALERT_WEBHOOK_URL`); a post's first-run alert names the targets that gave up.
Immediate-post failures also alert. The History tab
gains an **attention** filter (`GET /v1/posts?status=needs_attention`).
Manual retry (`POST /v1/posts/{id}:retry`) remains available on any target,
including given-up ones, and an item whose outcome is `unknown` is settled
by hand with `POST /v1/posts/{id}:resolve`.

### Stopping and restarting

On `SIGTERM` (`docker stop`) the publisher stops gracefully. `/readyz`
returns `503` (`accepting_work: false`) for 3 seconds so load balancers stop
sending traffic, then new connections are refused, and requests and parts
being published finish, for up to 25 seconds in all. Docker's default stop timeout is 10
seconds, so give the container 30 (`stop_grace_period: 30s` in compose,
`docker stop -t 30`). Anything still being sent when the time runs out is
handled on the next start as described above.

### Public feed

- `GET /v1/public/feed?limit=20` — latest public master posts as JSON for a
  homepage. Requires `Authorization: Bearer $PUBLIC_FEED_TOKEN`; returns `404`
  when `PUBLIC_FEED_TOKEN` is unset. Each item has `id`, `published_at` (first
  time the post went live on any platform), `text`, optional `media[]`, optional
  `interaction` (for quotes/reposts), and `links[]` — one `{platform, url,
  account, handle}` per account where the post is public and successfully
  published (`account` is the account's id; `handle` is absent until its
  network first answered). An account whose `public_feed` is off is left out. Replies,
  and any post with no public copy, are omitted. `limit` defaults to 20, max
  100.

When a feed-eligible post is published (immediately, on a scheduled fire, or on
a retry), publisher fires a signal-only `POST` to `FEED_WEBHOOK_URL`
(`{ "event": "post.published", "id", "published_at" }`) so the homepage can
re-fetch instead of polling. The body carries no content; treat it as a refresh
trigger.

### Web UI

The container embeds a mobile-first web UI (the crosspost composer with
drafts, post history, scheduled posts, relay tools, verify, and interact)
served from the root path. It talks to the publisher through `/v1` like any
other client. It asks you to sign in with a passkey or password, or with
your identity provider (OIDC); behind a reverse proxy alone it has no
sign-in of its own. See [Sign-in](#sign-in).

The **Settings** tab has two sections:

- **Accounts** lists your accounts by network, each with its status, its
  last check, and why one can't be used. Add an account there (Bluesky
  with an app password, Mastodon or Threads with a token, Nostr with your
  key or a new one made for you), rename it, turn it off, choose whether
  new posts pick it and whether its posts go to the public feed, change
  its settings or credentials, check it now, or delete it. Adding
  accounts needs a master key; see
  [Accounts and the master key](#accounts-and-the-master-key). On a new
  server with no accounts, Settings opens by itself and walks you
  through both.
- **Access** lists your passkeys and password, the browsers you are
  signed in on (sign any of them out), recent sign-ins and failed
  attempts, and your API tokens.

Adding or removing a way to sign in, setting the first password, creating
an API token, deleting an account and showing a Nostr key ask you to
confirm it's you first; changing the password asks for the current one
instead.

The composer picks **accounts**, not networks. They are grouped by
network, each with its avatar and handle; a network with several
accounts has **Select all**. An account that can't be posted to says why
under its name (not configured, turned off, …) and can't be ticked. A new
post selects the connected accounts marked **Picked for new posts** in
Settings → Accounts (`default_selected` in the API). A network can have
several accounts (two Mastodon accounts, say): each gets its own tab,
limit, wording and post.

The composer's **thread editor** has a tab for the shared text and one
per selected account. A tab is named by its network when that is your
only account there, else by the account's short handle (`@me`), or its
full handle when two of the same network would read the same. The shared text is a stack of cards, one per post
you break by hand: Ctrl/⌘+Enter (or ✂ split) starts a new post at the
cursor, Backspace at the start of a card joins it to the one above, and a
line of `---` becomes a break as soon as you start the next line. An
account's tab shows its thread as the server will post it, counted the way
that platform counts: solid lines are your breaks, dashed ones are splits
made to fit its limit. Tap a word and **Break here**, move a break with ↑
and ↓ (a sentence; Shift: a word) or by dragging it onto a word, and that
account gets its own breaks while its wording still follows the shared
text (**Reset to shared** undoes it). **Edit wording** gives it its own
text. Images go with the post they sit on: drag them between cards, or
use **Move to…**. Alt+1 opens Shared and Alt+2…Alt+9 the account tabs
in order; on the tab row, ← and →, Home and End move between tabs. On a
wide screen **Compare** (Alt+C) lines every account's thread up side by
side, each against its own limit. In History, hovering a part of a thread
marks where it came from in the text, each post shows the accounts it went
to, and with two or more accounts a menu shows one account's posts.

The **Interact** tab accepts a pasted post URL or Nostr identifier, previews
the source post, and reports which of reply/quote/repost the post allows.
**Repost** is one-click. **Reply** and **Quote** open the **Compose** tab in
interaction mode: a source banner over the full composer, so your reply or
quote gets the live preview, auto-threading, media, and per-account
overrides. A reply goes to the source post's network only, from one
account: when you have several connected there, pick which one looks the
post up and answers it. A quote also goes to your other selected accounts
as a link-quote.

### `GET /healthz`

Liveness: returns `200 OK` with body `ok` if the process is up. Probes
nothing.

### `GET /readyz`

Readiness: can this instance accept and persist work right now?

```json
{
  "status": "ok",
  "checks": {
    "database":      {"status": "ok", "critical": true,  "duration_ms": 1},
    "data_disk":     {"status": "ok", "critical": false, "duration_ms": 0},
    "video_workdir": {"status": "ok", "critical": false, "duration_ms": 0}
  }
}
```

| Check | Critical | Fails when |
|---|---|---|
| `database` | yes | SQLite doesn't answer, or its schema version isn't the one this build expects |
| `data_disk` | no | less than 256 MiB free on the database's filesystem |
| `video_workdir` | no | (video enabled only) the work directory isn't writable |
| `accepting_work` | yes | the publisher is shutting down |

`status` is `ok`, `degraded` (a non-critical check failed; still `200`), or
`unavailable` (a critical check failed; `503`). Failed checks carry a short
`error` class (`timeout`, `unreachable`, `schema mismatch`, `low space`, …);
details are logged once when a check starts failing and when it recovers.

Add `?external=1` to also check upstream reachability — `blossom`, and
`account:<id>` for each account that is turned on and open (its server: a
Nostr account's bootstrap relay, by NIP-11). These never make the
instance unavailable, are cached for 30 s, and send no credentials
(reachability only, not token validity). Earlier versions named these
checks `nostr_relay`, `bluesky`, `mastodon` and `threads`; a monitor that
matched those names must match `account:` names now (the ids are in
`GET /v1/accounts`).

`/healthz` and `/readyz` are open even when OIDC is enabled.

### CLI healthcheck mode

Invoked as `/publisher -healthcheck` inside the container — issues a GET to
`localhost:$PORT/readyz` (local checks only) and exits 0 when ready (`200`,
including `degraded`), 1 otherwise. The image declares it as its default
`HEALTHCHECK`, so `docker ps` shows health out of the box. To tune it in
compose:

```yaml
healthcheck:
  test: ["CMD", "/publisher", "-healthcheck"]
  interval: 30s
  timeout: 5s
  retries: 3
```

## Self-hosting

`publisher` runs as a single container. It holds your Nostr secret key and
signs events on request, so treat it like a credential store and read
[Security](#security) before exposing it.

### Prerequisites

- A writable directory for the SQLite archive (mounted at `DB_PATH`).
- For media (images and videos): a
  [Blossom](https://github.com/hzrd149/blossom) server URL
  (`BLOSSOM_URL`). Uploads to it are signed with the Nostr key, so it
  needs `NSEC_HEX` too. Without it, posts are text only.
- To add accounts in the web app: a master key (`publisher keygen` makes
  one; see [Accounts and the master key](#accounts-and-the-master-key)).
  Accounts set in the environment need none.
- Per network you want to post to, its credentials. In the web app
  (**Settings → Accounts**):
  - **Nostr:** your `nsec1…` key (or its hex form), or a new key made for
    you, and a home relay.
  - **Mastodon:** the server's address and an access token.
  - **Bluesky:** your handle (or DID, or email) and an
    [app password](https://bsky.app/settings/app-passwords); the server
    (PDS) is found from the handle.
  - **Threads:** a long-lived access token.

  Or in the environment, one account per network:
  - **Nostr:** a secret key as 64-char hex (`NSEC_HEX`). Convert your
    `nsec1…` once with [`nak`](https://github.com/fiatjaf/nak):
    `nak key decode nsec1…` gives the hex secret. The public key is
    derived from it.
  - **Mastodon:** instance URL + access token (both required together).
  - **Bluesky:** PDS URL, handle/identifier, and an app password (all three
    required together).
  - **Threads:** a long-lived access token + user id.

### Configuration

The server is configured by environment variables, and none is required:
a server with no account starts, and says so in its log. The
relay defaults point at the author's own infrastructure and should be
overridden. An account from the environment is turned on by its credential (`NSEC_HEX`,
`MASTODON_TOKEN`, `BLUESKY_APP_PASSWORD`, `THREADS_ACCESS_TOKEN`); a
credential set without the variables that go with it stops startup with an
error naming them, and so does `BLOSSOM_URL` without `NSEC_HEX`. Every invalid variable is
reported at once. Boolean variables take `true/false`, `1/0`, `yes/no` or
`on/off` in any case; any other value is an error, not the default.

| Var | Required | Default | Purpose |
|---|---|---|---|
| `NSEC_HEX` | for Nostr and media | — | Secret key as exactly 64 hex characters (**secret**). Convert `nsec1…` via `nak key decode`; if a tool printed it without its leading zeros, add them back |
| `OWNER_PUBKEY` | no | (derived) | 64-char hex pubkey. Optional: it is derived from `NSEC_HEX`. If set, it must match, or the process refuses to start |
| `BLOSSOM_URL` | for media | — | Blossom media server base URL, e.g. `https://blossom.example.com`. Unset: posts carry no media, and the composer hides the attach button. Posts made while it was set still fetch their media for a retry |
| `NIP65_BOOTSTRAP_RELAY` | no | `wss://relay.geoffrey.one` | Where to fetch the author's `kind:10002` relay list |
| `FALLBACK_RELAYS` | no | `wss://relay.geoffrey.one,wss://nos.lol,wss://relay.damus.io` | Used when NIP-65 returns no write-relays. Set it to a single space for none |
| `SYNC_RELAYS` | no | `wss://nos.lol,wss://relay.damus.io,wss://nostr.wine,wss://nostr.land,wss://relay.nostr.band,wss://purplepag.es,wss://relay.snort.social,wss://nostr.mom` | Default relay set for the relay-sync tool |
| `RELAY_CACHE_TTL` | no | `1h` | TTL for cached `kind:10002` lookups |
| `PUBLISH_TIMEOUT` | no | `10s` | Per-relay connect+publish timeout |
| `POW_DIFFICULTY_DEFAULT` | no | `16` | Default leading-zero-bit PoW target; `0` turns proof of work off (a negative value stops startup) |
| `POW_DIFFICULTY_MAX` | no | `28` | Hard cap on per-request PoW override (not negative) |
| `POW_TIMEOUT` | no | `30s` | Per-publish mining timeout |
| `SCHEDULE_GRACE` | no | `2h` | Grace window for firing overdue scheduled posts (must be positive) |
| `AUTO_RETRY_ENABLED` | no | `true` | Master switch for automatic retries (failed parts and relay deliveries) |
| `AUTO_RETRY_MAX_ATTEMPTS` | no | `6` | Attempts per part (and per relay) before giving up (at least 1; turn retries off with `AUTO_RETRY_ENABLED=false`) |
| `AUTO_RETRY_BASE_DELAY` | no | `2m` | First backoff interval |
| `AUTO_RETRY_MAX_DELAY` | no | `1h` | Backoff ceiling |
| `RETRIER_TICK` | no | `60s` | Relay-retry loop cadence |
| `PUBLISH_WORKERS` | no | `4` | Parts the background engine publishes at once (scheduled posts, retries) |
| `DB_PATH` | no | `/data/publisher.db` | SQLite archive path (mount a volume here) |
| `PUBLISHER_MASTER_KEY_FILE` | to add accounts in the app | — | A file holding the master key, such as a Docker secret (`/run/secrets/publisher_master_key`). Preferred over the variable, which `docker inspect` shows. See [Accounts and the master key](#accounts-and-the-master-key) |
| `PUBLISHER_MASTER_KEY` | (or the file) | — | The master key itself (**secret**): 32 bytes in base64, as `publisher keygen` prints it. Set this or `_FILE`, not both |
| `PUBLISHER_MASTER_KEY_PREVIOUS` | to change the key | — | The old key while you move to a new one (**secret**; or `PUBLISHER_MASTER_KEY_PREVIOUS_FILE`): at startup every saved credential moves to `PUBLISHER_MASTER_KEY`, all or nothing. Remove it afterwards |
| `MASTODON_TOKEN` | no | — | Mastodon access token (**secret**, enables Mastodon cross-post) |
| `MASTODON_BASE_URL` | with the token | — | Mastodon instance URL, e.g. `https://mastodon.social` |
| `BLUESKY_APP_PASSWORD` | no | — | Bluesky app password (**secret**, enables Bluesky cross-post) |
| `BLUESKY_IDENTIFIER` | with the password | — | Bluesky handle, DID or email |
| `BLUESKY_PDS_URL` | with the password | — | Bluesky PDS URL, e.g. `https://bsky.social` |
| `THREADS_ACCESS_TOKEN` | no | — | Threads long-lived token (**secret**, enables Threads cross-post) |
| `THREADS_USER_ID` | no | `me` | Threads user id |
| `ALERT_WEBHOOK_URL` | no | — | Webhook posted to on scheduled-post failure |
| `ALERT_WEBHOOK_USER` | no | `alertmanager` | Basic-auth user for the alert webhook |
| `ALERT_WEBHOOK_PASS` | no | — | Basic-auth password for the alert webhook (**secret**) |
| `PUBLIC_FEED_TOKEN` | no | — | Bearer token gating `GET /v1/public/feed`; unset disables the endpoint (**secret**) |
| `FEED_WEBHOOK_URL` | no | — | Signal-only webhook POSTed when a feed-eligible post is published |
| `FEED_WEBHOOK_TOKEN` | no | — | Bearer token sent on the feed webhook so the receiver can verify it (**secret**) |
| `PORT` | no | `8080` | Listen port |
| `LOG_LEVEL` | no | `info` | `debug` / `info` / `warn` / `error` |
| `PLC_DIRECTORY_URL` | no | `https://plc.directory` | did:plc resolver for Bluesky identity lookups |
| `VERIFY_HTTP_TIMEOUT` | no | `10s` | Per-request timeout for verification fetches |
| `PUBLIC_URL` | for sign-in | — | The address you open Publisher at, as an origin: `https://post.example.com` (no path), or `http://localhost[:port]`. Passkeys belong to its host name, so it must be a name, not an IP address. Needed for local sign-in; with OIDC it is the default base of `OIDC_REDIRECT_URL` |
| `AUTH_MODE` | no | (see [Sign-in](#sign-in)) | How you sign in: `local` (passkeys and an optional password kept by Publisher), `oidc` (an identity provider; needs `OIDC_ISSUER`) or `proxy` (no sign-in of its own). Unset: `oidc` when `OIDC_ISSUER` is set, else `local` for a database created by this version and `proxy` for an older one |
| `TRUSTED_PROXIES` | no | (none) | CSV of addresses or CIDRs (`172.16.0.0/12`) of your reverse proxies. The client's address, used to throttle wrong passwords and shown under Settings → Access, is the last `X-Forwarded-For` hop that isn't one of them; without it, the connection's own address |
| `SETUP_TOKEN_FILE` | no | (log) | Write the first-run setup link to this file (mode 0600, deleted once used) instead of the log. Its directory must exist |
| `OIDC_ISSUER` | no | (empty) | Issuer URL of the OpenID Provider (e.g. `https://auth.example.com`). Setting it signs you in with OIDC and makes the vars below required |
| `OIDC_CLIENT_ID` | yes* | — | Confidential client ID registered with the provider |
| `OIDC_CLIENT_SECRET` | yes* | — | Client secret (**secret**) |
| `OIDC_REDIRECT_URL` | yes* | `PUBLIC_URL/auth/callback` | Callback URL registered with the provider, e.g. `https://publisher.example.com/auth/callback`. Required when `PUBLIC_URL` is unset |
| `OIDC_ALLOWED_SUBJECTS` | yes*† | — | CSV of allowed OIDC subjects (`sub`) |
| `OIDC_ALLOWED_EMAILS` | yes*† | — | CSV of allowed email addresses. Only an email the provider marks verified (`email_verified`) matches; case doesn't matter. A provider that leaves the claim out (e.g. Entra ID) needs `OIDC_ALLOWED_SUBJECTS`; the refused login logs the subject to add |
| `OIDC_SCOPES` | no | `openid profile email` | Requested scopes (space- or comma-separated) |
| `OIDC_END_SESSION` | no | `true` | On logout, also end the provider session (RP-initiated logout) when the provider advertises `end_session_endpoint` |
| `SESSION_TTL` | no | `168h` | Server-side session lifetime (OIDC and local sign-in). Removing someone from the OIDC allowlist ends their sessions at once |
| `ALLOWED_HOSTS` | no | (empty) | CSV of host names the server answers for (e.g. `post.example.com`); `localhost`, `PUBLIC_URL`'s host and the health probes always pass, and other names get 421. Recommended in `proxy` mode: it stops DNS rebinding from a web page. List the names your scripts use too (`publisher:8080` from another container) |
| `PRIVATE_DESTINATIONS` | no | (none) | CSV of host names and private ranges (`mastodon.lan`, `192.168.1.0/24`, `100.64.0.0/10` for Tailscale) that accounts added in the app and relay sync may reach. Without it they reach public addresses only. See [Servers on your own network](#servers-on-your-own-network) |

\* Required only when `OIDC_ISSUER` is set.
† At least one of `OIDC_ALLOWED_SUBJECTS` / `OIDC_ALLOWED_EMAILS` must be non-empty when OIDC is enabled.

> **Note:** `OWNER_PUBKEY` is optional. When it is set it must be the public
> key derived from `NSEC_HEX`; the process exits at startup if they don't
> match, and setting it without `NSEC_HEX` is an error.

### Run with Docker

The image is published for `linux/amd64` and `linux/arm64` (Raspberry Pi 4/5
on a 64-bit OS, Ampere/Graviton servers, Apple-silicon hosts); Docker pulls the
right one automatically.

```bash
mkdir -p ./data
docker run -d --name publisher --stop-timeout 30 \
  -p 127.0.0.1:8080:8080 \
  -v "$PWD/data:/data" \
  -e NSEC_HEX=<64-char-hex-secret> \
  -e BLOSSOM_URL=https://blossom.example.com \
  -e NIP65_BOOTSTRAP_RELAY=wss://relay.example.com \
  -e FALLBACK_RELAYS=wss://relay.damus.io,wss://nos.lol \
  ghcr.io/geofox/publisher:latest
```

### Run with Docker Compose

```yaml
services:
  publisher:
    image: ghcr.io/geofox/publisher:latest
    container_name: publisher
    restart: unless-stopped
    # Time to finish what is being published on `docker stop` (default 10s).
    stop_grace_period: 30s
    ports:
      - "127.0.0.1:8080:8080"
    environment:
      # Encrypts the credentials of accounts you add in the web app; see
      # "Accounts and the master key". Leave it out to set accounts here only.
      - PUBLISHER_MASTER_KEY_FILE=/run/secrets/publisher_master_key
      - NSEC_HEX=${NSEC_HEX}
      - BLOSSOM_URL=https://blossom.example.com
      - NIP65_BOOTSTRAP_RELAY=wss://relay.example.com
      - FALLBACK_RELAYS=wss://relay.damus.io,wss://nos.lol
      - DB_PATH=/data/publisher.db
      # Optional cross-post platforms:
      # - MASTODON_BASE_URL=https://mastodon.example.com
      # - MASTODON_TOKEN=...
      # - BLUESKY_PDS_URL=https://bsky.social
      # - BLUESKY_IDENTIFIER=you.bsky.social
      # - BLUESKY_APP_PASSWORD=...
      # - THREADS_ACCESS_TOKEN=...
    secrets: [publisher_master_key]
    volumes:
      - ./data:/data
    healthcheck:
      test: ["CMD", "/publisher", "-healthcheck"]
      interval: 30s
      timeout: 5s
      retries: 3
      # The first start after an upgrade migrates the database before it
      # listens.
      start_period: 60s

secrets:
  publisher_master_key:
    file: ./publisher_master_key
```

### Accounts and the master key

An account comes from one of two places:

- **The web app** (**Settings → Accounts**), as many per network as you
  like. Publisher asks the network who the credentials belong to before
  it saves anything, so a wrong token or password is refused on the spot,
  with the network's own words, and the same account can't be added
  twice. Credentials are kept in the database, encrypted with your
  master key, and are never shown again, except a Nostr key, which you
  can show after confirming it's you. You change or delete the account
  there.
- **The environment** (`NSEC_HEX`, `MASTODON_TOKEN`, … above), one per
  network, read at every start; change it by editing the variables.
  Settings shows it read-only. **Move into the app** copies its settings
  and credentials into the database (encrypted) and keeps its history;
  from then on its variables are ignored, so remove them (a warning at
  startup names them while they are set). Keep `NSEC_HEX` while
  `BLOSSOM_URL` is set: it signs media uploads.

**Making the master key.** It is 32 random bytes in base64. Make one
with the image (`docker run --rm ghcr.io/geofox/publisher keygen`), the
binary (`publisher keygen`), or in the web app, whose first-run steps
make one in your browser and never send it anywhere. Give it to the
container as a file (a Docker secret, as in the compose example above):

```bash
printf '%s' '<the key>' > publisher_master_key && chmod 600 publisher_master_key
```

(Typed like this, the key is kept in your shell's history: clear it from
there, or write the file with an editor.) Or give it as
`PUBLISHER_MASTER_KEY` (which `docker inspect` shows). Publisher
reads it at startup, then removes it and the other credential variables
from its own environment, so the tools it runs (ffmpeg) don't inherit
them. A key file every user on the host can read gets a warning. If the container runs as another user than
root (`user:` in compose), make the file readable by that user.

**Keep a copy of the key somewhere safe, apart from the database.**
Without it, the credentials saved in the app can't be read: those
accounts are locked (their posts wait), the web app says so on every
tab, and nothing new can be saved until the key is back. A lost key
means adding those accounts again. Accounts from the environment keep
working without it (a Threads account goes back to
`THREADS_ACCESS_TOKEN`, which may have expired since: set a fresh one).
A *wrong* key (not the one they were saved with) locks a Threads account
from the environment too, since the token it renewed is the one that
works now: set the right key, or remove the key to go back to
`THREADS_ACCESS_TOKEN`.

**Changing the key.** Make a new one, set it as `PUBLISHER_MASTER_KEY`,
set the old one as `PUBLISHER_MASTER_KEY_PREVIOUS` (or the `_FILE`
forms), and restart. At startup every saved credential is moved to the
new key in one transaction; if any fails, nothing changes and the log
says why. Once the log says `saved credentials moved to the new key`,
remove `PUBLISHER_MASTER_KEY_PREVIOUS`.

**What the encryption protects.** A copy of the database (a backup, a
stolen disk) doesn't give away the credentials saved in the app, or the
Threads token Publisher renews. Each value is bound to its account and
to the server it is sent to: a value copied to another account, or an
account whose server address is changed in the database, fails to open,
and the account is locked rather than send its credentials elsewhere. It
doesn't protect against someone who can run commands in the container
or read its secrets: they have the key. Credentials set in the
environment are as safe as your environment. The log, events and API
answers never carry an account's credentials (except **Show key**, on
purpose), and the log scrubs anything that looks like one.

**Backups.** Back up the database and the key, separately. The
credentials in a backup open only with the key that was set when it was
taken: after changing the key, keep the old one as long as you keep
older backups.
With a key set, the first start of this version moves the Threads token
from its plaintext row into the encrypted store; the database backups
written by earlier upgrades (`publisher.db.pre-v*.bak`) still hold it in
plaintext, and the log says so. Delete them once this version works for
you.

#### Servers on your own network

An address you set in the app (an account's server, a Nostr relay, a
relay you sync to) reaches public addresses only, over `https://` or
`wss://`. A server on your own network (a Mastodon or PDS at home, a
relay on Tailscale) needs `PRIVATE_DESTINATIONS`: a CSV of host names
(`mastodon.lan`, `relay.tailnet-name.ts.net`) and private ranges
(`192.168.1.0/24`, `100.64.0.0/10`). A listed host name may resolve to
any private address; an address in a listed range may be reached by any
name. Only private ranges can be listed (10/8, 172.16/12, 192.168/16,
100.64/10, fc00::/7). Plain `http://` and `ws://` are accepted for a
listed host name, or an address in a listed range. Loopback, link-local,
clouds' metadata addresses (`169.254.169.254`, and `100.100.100.200` and
`fd00:ec2::254` even inside a listed range) and the server's own
addresses are never reached, and an error never says what a name
resolved to. Addresses you set in the
environment (`MASTODON_BASE_URL`, the relay variables) are yours and
aren't checked, except the relays relay sync compares with your home
relay.

### Database upgrades and backups

The SQLite schema is versioned. On start, publisher applies any pending
migrations (recorded in the `schema_migrations` table), each in its own
transaction. Before upgrading an existing database it writes a consistent
copy next to it, named after the version it is about to apply — for example
`/data/publisher.db.pre-v2.bak`. To roll back an upgrade, stop the container,
replace `publisher.db` with that file (and delete `publisher.db-wal` /
`publisher.db-shm`), and run the previous image.

An older image refuses to start on a database that a newer image has already
migrated (`database schema is newer than this build supports`), instead of
writing data the newer schema doesn't expect. Run the newer image again, or
restore the backup.

### Upgrading from 1.x to 2.0

2.0 replaces the HTTP API with `/v1` and moves publishing onto a durable
engine. No environment variable was removed or renamed. What to do:

1. **Move every API caller to `/v1` first** (n8n included): `/publish`,
   `/upload-media` and `/api/*` answer `410 Gone` in 2.0. See
   [Migrating from the pre-1.0 API](#migrating-from-the-pre-10-api).
   Existing tokens keep working with `posts:write media:write`.
2. **Move the public feed consumer** to `GET /v1/public/feed` (same token,
   same body; `limit` must now be 1 to 100). If a reverse proxy routes the
   feed path specially, change the path there too.
3. **Give the container 30 seconds to stop** (`stop_grace_period: 30s`,
   `docker stop -t 30`), and a healthcheck `start_period` long enough for
   the first start's migrations (60 s is plenty for a personal archive).
4. **Have room for a full copy of the database**: before migrating, the
   first start writes `publisher.db.pre-v1.bak` next to it.
5. **Upgrade:** stop the old container, start the new image, and watch the
   log for `applied database migration` (versions 1 to 8) and
   `drafts: converted to /v1`. Drafts' attachments that aren't on the
   `BLOSSOM_URL` server are dropped from them, each with a log line.
6. **Check:** `/v1/accounts` lists one account per configured platform,
   History shows your old posts, and a test post goes out.
7. **Log alerts:** the per-publish fields (`relays_succeeded`, …) are gone;
   alert on `engine: item not sent` with `give_up = true` (see
   [Logging](#logging)).

To roll back, stop the container, replace `publisher.db` with
`publisher.db.pre-v1.bak` (and delete `publisher.db-wal` and
`publisher.db-shm`), then run the 1.x image. Never run 1.x on the upgraded
database: 1.x has no schema check and would write to tables it doesn't
know. Posts made on 2.0 after the upgrade are lost from History by a
rollback (they stay on the platforms).

### Upgrading from 2.x

No environment variable was removed or renamed, and nothing needs to be
set before starting. Read the [CHANGELOG](CHANGELOG.md) first; the changes
that need action:

1. **Sign-in.** An existing install without OIDC keeps running behind your
   proxy alone (`AUTH_MODE=proxy`), with a "Set up sign-in" banner; to
   sign in with a passkey, set `AUTH_MODE=local` and `PUBLIC_URL` (see
   [Sign-in](#sign-in)). In proxy mode a token holding `admin` or
   `accounts:write` can no longer be created. **With OIDC,** creating a
   token in the web app (any token: it outlives the session) now sends
   you through your provider's login again: use Authelia 4.39 or later.
   A token can no longer create a token holding `admin`.

2. **API callers that split threads with `---`** (an n8n flow, say) must
   send `content.breaks` instead: `---` is plain text now.
3. **Monitors:** `/readyz?external=1` names its checks `account:<id>`
   instead of `nostr_relay`, `bluesky`, `mastodon` and `threads`. Alerts
   name the account ("Mastodon (@me@example.social)").
4. **Startup is stricter:** a boolean variable that isn't `1/true/yes/on`
   or `0/false/no/off`, a `NSEC_HEX` that isn't 64 hex characters, a
   negative `POW_*`, or `BLOSSOM_URL` without `NSEC_HEX` now stops startup
   with an error naming the variable.
5. **A master key** is optional: your accounts from the environment work
   without one. Set one to add accounts in the web app (see
   [Accounts and the master key](#accounts-and-the-master-key)); with it,
   the first start moves the Threads token Publisher renews into the
   encrypted store. The `publisher.db.pre-v*.bak` backups from earlier
   upgrades still hold that token in plaintext: delete them once the
   upgrade works for you.
6. **Relay sync to a relay on a private address** (`ws://192.168.1.5:7777`,
   a Tailscale name) needs that host or range in `PRIVATE_DESTINATIONS`
   (see [Servers on your own network](#servers-on-your-own-network));
   the home relay is reached as before.
7. **The Tokens tab is now Settings → Access**, next to Settings →
   Accounts.

Your accounts keep their ids (`acc_…` in `GET /v1/accounts`), so posts,
drafts and API callers that name them keep working; the relay-sync list
moves to your Nostr account. Before migrating, the first start writes a
copy named after the first version it applies (`publisher.db.pre-v9.bak`
from 2.0); see [Database upgrades and backups](#database-upgrades-and-backups).

### Build from source

Requires Go (version per `go.mod`):

```bash
go build -o publisher ./cmd/publisher
./publisher    # reads config from the environment
```

Or build the container image:

```bash
docker build -t publisher:dev .
# or both architectures at once (needs a buildx builder with the containerd
# image store or a docker-container driver; no QEMU required):
docker buildx build --platform linux/amd64,linux/arm64 -t publisher:dev .
```

### Security

The service holds your `NSEC` and signs events on request. **Do not expose
it directly to the internet.**

Publisher signs you in one of three ways (`AUTH_MODE`); see
[Sign-in](#sign-in). In every mode:

- Every `/v1` route takes either the browser session or a bearer token,
  and checks the token's scopes (see [the `/v1` API](#the-v1-api)).
  Tokens are created and revoked in the web app under **Settings →
  Access**.
  `GET /v1/openapi.json` is open.
- `/healthz`, `/readyz`, `/metrics`, and the feed-token-gated `GET
  /v1/public/feed` remain open (no session required). `/metrics` is
  intentionally unauthenticated so a Prometheus scraper can reach it
  without a session; restrict it at the network or proxy layer if that is
  a concern in your deployment.
- Some operations need you to confirm it's you, just now, in the browser:
  adding or removing a passkey, setting the first password or removing
  it, creating an API token (a token outlives the session that made
  it), deleting an account, and showing a Nostr key. Changing the
  password takes the current one instead. A token holding `admin` or
  `accounts:write` can only be made this way: a token can't
  (`403 session_required`), and behind the proxy alone it is refused
  (`403 sign_in_required`).
- Credentials of accounts added in the app are encrypted with the master
  key and never returned (a Nostr key only by **Show key**, which a
  token can't do), and the addresses you set reach public servers only,
  unless `PRIVATE_DESTINATIONS` lists yours; see
  [Accounts and the master key](#accounts-and-the-master-key).

**Single-tenant caveat:** the app signs with one operator key and posts to
the operator's own accounts. Sign-in gates who may operate it; it is not
per-user data isolation.

### Sign-in

| `AUTH_MODE` | Who signs you in | When it is the default |
|---|---|---|
| `local` | Publisher: a passkey, and a password if you want one | `OIDC_ISSUER` is unset and the database was created by this version (a new install) |
| `oidc` | Your identity provider (Authelia, Keycloak, …) | `OIDC_ISSUER` is set |
| `proxy` | Nobody: the reverse proxy in front is the only gate | `OIDC_ISSUER` is unset and the database is older (an upgrade) |

#### Local sign-in (passkeys)

1. Serve Publisher over https at a host name (`tailscale serve`, Caddy,
   Traefik, …) and set `PUBLIC_URL` to that address. Passkeys and the
   sign-in cookie need https (or `http://localhost`), and a passkey
   belongs to the host name, so open Publisher at that address only.
2. Start it. Until sign-in is set up, each start logs a one-time setup
   link, valid for an hour:
   `setup: open https://post.example.com/setup#… on the address you use
   for Publisher`. With `SETUP_TOKEN_FILE` the link goes to that file
   instead, and the log names the file.
3. Open the link and create a passkey (or a password, at least 12
   characters). You're signed in; setup is closed for good.

Add more passkeys (one per device, or a synced one; up to 20) and a
password under **Settings → Access**. Wrong passwords are throttled per address: 5
are free, then you wait 1, 2, 4, 8 and then 15 minutes between tries (a
passkey still works meanwhile). After 20 wrong passwords in a row from
anywhere, each try waits a few seconds (at most 30), except in a browser
you've signed in with before (it keeps a cookie for that, for 400 days).
Behind a reverse proxy, set `TRUSTED_PROXIES` so the throttle sees your
visitors' addresses, not the proxy's.

**Changing `PUBLIC_URL` to another host name?** Passkeys belong to the
old name and can't sign you in at the new one (**Settings → Access**
marks them).
With a password, sign in with it and add passkeys; without one, setup
opens again and the next start logs a setup link.

**Lost the setup link?** In the container: `docker exec publisher
/publisher setup-url` prints a new one (older ones stop working).

**Locked out?** `docker exec publisher /publisher owner reset-login`
removes every passkey and the password, signs every browser out,
forgets the browsers you signed in with and the failed attempts, lists
every active API token, and prints a new setup link. Add
`--revoke-tokens` to revoke those tokens too. Whoever can run commands in
the container already controls Publisher, so these need no password.

#### OIDC

1. Register a **confidential** client (with PKCE `S256`) with your OIDC
   provider and set the redirect URI to
   `https://<your-publisher-host>/auth/callback`.
2. Set the `OIDC_*` variables (see [Configuration](#configuration)) and
   `PUBLIC_URL`. Add your own OIDC subject and/or email to
   `OIDC_ALLOWED_SUBJECTS` / `OIDC_ALLOWED_EMAILS`.
3. To confirm it's you, Publisher sends you through your provider's login
   again (`prompt=login`, `max_age=0`) in a small window, and checks the
   ID token's `auth_time`. The provider must honour both: **Authelia 4.39
   or later** does (4.38 accepts `prompt=login` but doesn't ask again, so
   the confirmation fails).
4. For machine callers (e.g. an automation workflow), open the web app
   under **Settings → Access**, create a token, and configure the caller to send
   `Authorization: Bearer <token>` on its `/v1` requests (see
   [Migrating from the pre-1.0 API](#migrating-from-the-pre-10-api) for n8n).

#### Behind the proxy alone

With `AUTH_MODE=proxy` the web app and `/v1` are **not** authenticated by
the service itself: every request the proxy lets through can post,
manage drafts and accounts' labels, and create ordinary tokens. (On
`/v1`, a bearer token, when one is sent, is still checked.) Put it behind
an authenticating reverse proxy (Authelia, oauth2-proxy, Basic Auth) or
keep it on a private network / VPN, and set `ALLOWED_HOSTS` to the name
you reach it by. A Basic Auth `Authorization` header the proxy forwards
is ignored. The `docker run` and compose examples above bind port 8080 to
`127.0.0.1` (localhost only), so nothing is exposed until you
deliberately place a proxy in front of it. The web app shows a "Set up
sign-in" banner in this mode; tokens holding `admin` or `accounts:write`
can't be created here, and accounts can't be added, changed, shown,
moved into the app or deleted (only renamed, turned on or off, and
picked for new posts or the public feed).

## Logging

JSON-lines on stdout via `log/slog`. Each send attempt, and each item the
engine settles without sending (one left in flight by a restart, one it
won't repeat because the platform has forgotten its idempotency key),
writes one line, `engine: item sent` (info) or `engine: item not sent`
(warn), with:

- `post_id`, `platform`, `ordinal` (the thread part, from 0), `attempt`
- `status` (`published`, `partial`, `failed`, `unknown`), `latency_ms`
- `remote_id` when the platform answered with one (a Nostr event id)
- `endpoints_ok`, `endpoints_failed` for Nostr (relays that took the note)
- `error_class`, `error` and `give_up` when it wasn't sent

Recommended log alert: `msg = "engine: item not sent" and give_up = true`,
which is when a part stopped being retried.

## Not implemented

- Mention-aware NIP-65: doesn't fetch read-relays for `p`-tagged users. Only
  the author's own write-relays are resolved. Posts with mentions still
  reach those users *if* their read-relays overlap the author's write-set,
  which is the common case.
- Video imeta: BlurHash + dim are computed for `image/*` MIME types only.
  Video uploads still go through Blossom and the imeta tag still carries
  `url`, `m`, and `x`.
- Multi-identity: one NSEC per container. Run a second container for a
  second identity.
- Mirror to other Blossom servers: single-destination upload.

## License

[GPL-3.0-or-later](LICENSE).
