# publisher

A self-hosted, multi-protocol social publisher. Cross-posts to **Nostr,
Bluesky, Mastodon, and Threads** from a single HTTP API, with a SQLite
archive, per-platform retry, scheduled posting, and an embedded mobile web
UI. It signs Nostr events itself (NIP-13 PoW, NIP-65 outbox resolution,
NIP-92 `imeta` media via Blossom) and holds the signing key locally.

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

### `POST /publish`

```json
{
  "text":  "hello world",
  "kind":  1,
  "imeta": ["imeta", "url ...", "m ...", "dim ...", "x ...", "blurhash ..."],
  "pow":   16
}
```

**Size cap: 256 KB.** Enforced by `http.MaxBytesReader` around the JSON
decoder. Over-cap requests get `413 Request Entity Too Large` cleanly.
Note: even within the cap, relays have their own server-side max-event-size
policies (often ~64 KB content); the cap is a defense-in-depth bound, not a
guarantee the resulting event lands.

All fields except `text` are optional.

- `kind` defaults to `1` (text note)
- `imeta` is attached as a tag; pass through from `/upload-media` response
- `pow` is clamped to `[0, POW_DIFFICULTY_MAX]`; falls back to
  `POW_DIFFICULTY_DEFAULT` if unset
- **If `imeta` contains a URL and that URL isn't in `text`, the URL is
  appended to `text` with a `\n\n` separator** before signing. NIP-92 imeta
  annotates a URL the client expects to find in `content`; clients (Damus,
  Amethyst, YakiHonne, primal) render media from URLs in content and use
  imeta for hints (blurhash placeholder, dim). Append is idempotent — no-op
  if the URL is already present anywhere in `text`.

Response:

```json
{
  "event_id": "<64-char hex>",
  "kind":     1,
  "mined_ms": 12,
  "relays": [
    {"url": "wss://relay.example.com", "ok": true,  "message": ""},
    {"url": "wss://nos.lol",           "ok": true,  "message": ""},
    {"url": "wss://relay.damus.io",    "ok": false, "message": "rate-limited"}
  ]
}
```

HTTP 200 if at least one relay accepted. HTTP 502 if zero relays accepted.
HTTP 4xx for malformed requests or signing failure.

### `POST /upload-media`

`multipart/form-data` with a `file` field. Signs a BUD-01 `kind:24242`
upload-auth event and PUTs to Blossom.

**Size cap: 64 MB.** Enforced by a `Content-Length` pre-check (fast 413 for
well-formed clients) plus `http.MaxBytesReader` on `r.Body` (catches
chunked-encoding tricks). The multipart parser spills to `/tmp` over 8 MB to
bound RAM; the full body is still loaded into memory by `io.ReadAll` for
sha256 + image-decode, so peak per-upload RAM is ~64 MB.

Response:

```json
{
  "url":      "https://blossom.example.com/<sha256>",
  "sha256":   "<64-char hex>",
  "size":     12345,
  "mime":     "image/jpeg",
  "dim":      "1920x1080",
  "blurhash": "L9AS...",
  "imeta": [
    "imeta",
    "url https://blossom.example.com/<sha256>",
    "m image/jpeg",
    "dim 1920x1080",
    "x <sha256>",
    "blurhash L9AS..."
  ]
}
```

`dim` and `blurhash` are omitted for non-image MIME types (the imeta tag in
that case carries only `url`, `m`, and `x`).

### `POST /api/verify`

Verify that a post is authentically signed by the claimed author. Read-only;
uses no credentials and never touches the signing key.

```json
{ "input": "<pasted Nostr event JSON | Bluesky/Mastodon post URL | at:// URI>",
  "platform": "",   // optional: nostr|bluesky|mastodon (else auto-detected)
  "expected": "" }  // optional: npub / handle / @user@domain to match the signer
```

Response is a verdict with a tri-state `status` (`verified` / `failed` /
`error`), an `assurance` level (`cryptographic` for Nostr, Bluesky, and
FEP-8b32-signed Mastodon posts; `origin` for plain Mastodon), the resolved
`signer`, an optional `expected` match, and a transparent `checks` list.

- **Nostr:** verifies the event id and Schnorr signature (offline).
- **Bluesky:** verifies the repo commit signature and the record's MST inclusion
  proof, fetched from the author's PDS.
- **Mastodon:** verifies origin authority (the actor's domain serves the object),
  plus a FEP-8b32 `eddsa-jcs-2022` integrity proof when present.
- **Threads:** same ActivityPub origin-authority check as Mastodon, for
  `threads.com`/`threads.net` URLs (Threads has no native per-post signature and
  emits no FEP-8b32 proofs, so it never exceeds `origin` assurance). Only works
  for fediverse-federated Threads accounts whose posts expose an ActivityPub
  representation; a normal Threads web URL serves HTML, not ActivityPub, and
  returns `error`.

`status` semantics: `verified` = authentic; `failed` = the check completed and
the signature/inclusion is invalid (tampering/impersonation); `error` = the
check could not complete (network, unresolvable identity, deleted post) — NOT a
forgery signal.

HTTP 200 for any completed verdict (including `failed`); 400 for malformed
input; 413 if the body exceeds 512 KB; 502 when the verifier itself could not
complete (network/timeout).

### `POST /api/thread-preview`

Preview how a draft would split into a per-platform reply-chain (read-only; no
posting). Used by the compose preview.

```json
{ "text": "<master draft>", "platforms": ["bluesky","mastodon","threads","nostr"], "number": true }
```

Returns one entry per platform with the computed `segments` (each ≤ that
platform's limit — Bluesky 300 graphemes, Mastodon/Threads 500, Nostr
unlimited), the `count`, and any `warnings` (e.g. a URL longer than the limit
had to be hard-split). With `number`, a chain of ≥2 segments gets per-platform
` k/n` counters. Manual `---` lines in the text force breaks.

**Size cap: 256 KB.** Over-cap requests get `413 Request Entity Too Large`.

**Threaded posting:** when a draft exceeds a platform's limit (or contains `---`
break markers), the post is delivered as a native reply-chain per platform —
Bluesky/Mastodon/Threads wrap to their limits, Nostr stays one note unless
explicitly marked. Media and (for Nostr) `imeta` ride on the head segment only.
A chain that fails mid-way is recorded as `partial`; **resume** from history
re-posts only the not-yet-sent segments (already-delivered segments are never
re-sent). With `number`, segments get per-platform ` k/n` counters.

### `POST /api/resolve`

Resolve a pasted post URL (Bluesky/Mastodon) or a Nostr identifier
(`nevent`/`note`/`nostr:`/hex) into a preview + capability report (read-only;
no posting). Used by the **Interact** tab.

Body: `{ "input": "<url-or-nostr-id>" }`. Returns `{ platform, ref, preview,
caps }` where `caps.{reply,quote,repost}` each carry `{allowed, reason}`.
Threads URLs return 400 (the API has no URL→id lookup). Resolution failures
(not found, unfederated, bad input) return 400 with a reason.

### `POST /api/interact`

Reply to, repost, or quote a resolved source post. **`multipart/form-data`** with
a `spec` JSON field + optional `image` files (mirrors `/api/post`). The `spec`:
`{ action, platform, ref, source_url, source_author, source_preview:{author,text,
media:[{url,alt}]}, text, overrides, fanout, number, force, images:[{alt}] }` —
`ref` is the `ref` object returned by `/api/resolve`.

`reply` and `repost` act on the source `platform`; `reply`/`quote` commentary that
exceeds a platform's limit is **auto-threaded** (head = the native reply/quote,
tail = a reply-chain), with `number` adding `k/n` counters. `quote` (and a fanned-
out `reply`) does the native action on the source and a **fan-out reproduction** on
each `fanout` platform: your commentary + an attributed copy of the original's
text + re-hosted media + the source URL (the original isn't native there). Source
media is downloaded through an SSRF-guarded client and re-uploaded; failures are
skipped. `force: true` overrides a blocked capability (Bluesky may then silently
drop it). Returns the resulting `store.Post` (one threaded target per platform).
Used by the **Interact** tab.

### Web UI & `/api`

The container embeds a mobile-first web UI (the crosspost composer, post
history, scheduled posts, relay tools, verify, and interact) served from the
root path, backed by a JSON `/api/*` surface. These endpoints are
unauthenticated by the service itself — see [Security](#security).

The **Interact** tab accepts a pasted post URL or Nostr identifier, previews
the source post, and reports which of reply/quote/repost the post allows.
**Repost** is one-click. **Reply** and **Quote** open the **Compose** tab in
interaction mode — a source banner over the full composer, so your reply/quote
gets the live preview, auto-threading, media, and per-platform overrides. The
source platform is locked on (native reply/quote); selecting your other
platforms fans the action out as a reproduction (your commentary + the
original's text + re-hosted media + link). Blocked actions show a "try anyway"
override in the banner.

### `GET /healthz`

Returns `200 OK` with body `ok` if the process is up. Used by the Docker
healthcheck; does not probe Blossom or relays.

### CLI healthcheck mode

Invoked as `/publisher -healthcheck` inside the container — issues a GET to
`localhost:$PORT/healthz` and exits 0/1. Wire into compose:

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

- A Nostr keypair. You need the secret key as 64-char hex (`NSEC_HEX`) and
  the matching public key as 64-char hex (`OWNER_PUBKEY`). Derive both from
  your `nsec1…` once with [`nak`](https://github.com/fiatjaf/nak):
  `nak key decode nsec1…` gives the hex secret (`NSEC_HEX`) and
  `nak key public nsec1…` gives the matching hex pubkey (`OWNER_PUBKEY`).
- A [Blossom](https://github.com/hzrd149/blossom) server URL for media
  uploads (`BLOSSOM_URL`).
- A writable directory for the SQLite archive (mounted at `DB_PATH`).
- Optional, per platform you want to cross-post to:
  - **Mastodon:** instance URL + access token.
  - **Bluesky:** PDS URL, handle/identifier, and an app password.
  - **Threads:** a long-lived access token + user id.

### Configuration

All configuration is via environment variables. Only the first three are
**required**; the relay/Blossom defaults point at the author's own
infrastructure and should be overridden.

| Var | Required | Default | Purpose |
|---|---|---|---|
| `NSEC_HEX` | yes | — | 64-char hex secret key (**secret**). Convert `nsec1…` via `nak key decode` |
| `OWNER_PUBKEY` | yes | — | 64-char hex pubkey; must match `NSEC_HEX` or the process refuses to start |
| `BLOSSOM_URL` | yes | — | Blossom media server base URL, e.g. `https://blossom.example.com` |
| `NIP65_BOOTSTRAP_RELAY` | no | `wss://relay.geoffrey.one` | Where to fetch the author's `kind:10002` relay list |
| `FALLBACK_RELAYS` | no | `wss://relay.geoffrey.one,wss://nos.lol,wss://relay.damus.io` | Used when NIP-65 returns no write-relays |
| `SYNC_RELAYS` | no | `wss://nos.lol,wss://relay.damus.io,wss://nostr.wine,wss://nostr.land,wss://relay.nostr.band,wss://purplepag.es,wss://relay.snort.social,wss://nostr.mom` | Default relay set for the relay-sync tool |
| `RELAY_CACHE_TTL` | no | `1h` | TTL for cached `kind:10002` lookups |
| `PUBLISH_TIMEOUT` | no | `10s` | Per-relay connect+publish timeout |
| `POW_DIFFICULTY_DEFAULT` | no | `16` | Default leading-zero-bit PoW target |
| `POW_DIFFICULTY_MAX` | no | `28` | Hard cap on per-request PoW override |
| `POW_TIMEOUT` | no | `30s` | Per-publish mining timeout |
| `SCHEDULE_GRACE` | no | `2h` | Grace window for firing overdue scheduled posts |
| `DB_PATH` | no | `/data/publisher.db` | SQLite archive path (mount a volume here) |
| `MASTODON_BASE_URL` | no | — | Mastodon instance URL (enables Mastodon cross-post) |
| `MASTODON_TOKEN` | no | — | Mastodon access token (**secret**) |
| `BLUESKY_PDS_URL` | no | — | Bluesky PDS URL (enables Bluesky cross-post) |
| `BLUESKY_IDENTIFIER` | no | — | Bluesky handle / identifier |
| `BLUESKY_APP_PASSWORD` | no | — | Bluesky app password (**secret**) |
| `THREADS_ACCESS_TOKEN` | no | — | Threads long-lived token (**secret**, enables Threads cross-post) |
| `THREADS_USER_ID` | no | `me` | Threads user id |
| `ALERT_WEBHOOK_URL` | no | — | Webhook posted to on scheduled-post failure |
| `ALERT_WEBHOOK_USER` | no | `alertmanager` | Basic-auth user for the alert webhook |
| `ALERT_WEBHOOK_PASS` | no | — | Basic-auth password for the alert webhook (**secret**) |
| `PORT` | no | `8080` | Listen port |
| `LOG_LEVEL` | no | `info` | `debug` / `info` / `warn` / `error` |
| `PLC_DIRECTORY_URL` | no | `https://plc.directory` | did:plc resolver for Bluesky identity lookups |
| `VERIFY_HTTP_TIMEOUT` | no | `10s` | Per-request timeout for verification fetches |

> **Note:** `OWNER_PUBKEY` must be the public key derived from `NSEC_HEX`;
> the process exits at startup if they don't match.

### Run with Docker

```bash
mkdir -p ./data
docker run -d --name publisher \
  -p 127.0.0.1:8080:8080 \
  -v "$PWD/data:/data" \
  -e NSEC_HEX=<64-char-hex-secret> \
  -e OWNER_PUBKEY=<64-char-hex-pubkey> \
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
    ports:
      - "127.0.0.1:8080:8080"
    environment:
      - NSEC_HEX=${NSEC_HEX}
      - OWNER_PUBKEY=${OWNER_PUBKEY}
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
    volumes:
      - ./data:/data
    healthcheck:
      test: ["CMD", "/publisher", "-healthcheck"]
      interval: 30s
      timeout: 5s
      retries: 3
```

### Build from source

Requires Go (version per `go.mod`):

```bash
go build -o publisher ./cmd/publisher
./publisher    # reads config from the environment
```

Or build the container image:

```bash
docker build -t publisher:dev .
```

### Security

The service holds your `NSEC` and signs events on request. The embedded web
UI and the write endpoints (`/api/*`, `/publish`, `/upload-media`) are
**not** authenticated by the service itself. **Do not expose it directly to
the internet.** Put it behind an authenticating reverse proxy (Authelia,
oauth2-proxy, Basic Auth) or keep it on a private network / VPN. The
`docker run` and compose examples above bind port 8080 to `127.0.0.1`
(localhost only), so nothing is exposed until you deliberately place a proxy
in front of it.

## Logging

JSON-lines on stdout via `log/slog`. Important fields per publish:

- `event_id`
- `kind`
- `pow_target`, `pow_actual`, `mined_ms`
- `relays_resolved` (the NIP-65 outbox result)
- `relays_succeeded`, `relays_failed`

Recommended log alert: `relays_succeeded == 0`.

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
