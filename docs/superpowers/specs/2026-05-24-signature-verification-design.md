# Signature Verification — Design

**Date:** 2026-05-24
**Status:** Approved (design); pending implementation plan
**Branch:** `feature/signature-verification`

## 1. Overview & scope

A credential-free verification tool inside publisher. The user pastes a Nostr
event (raw JSON) or enters a Bluesky/Mastodon post URL; publisher fetches
whatever it needs and reports **who actually signed/vouched for the content**,
with an explicit **assurance level**, and optionally checks that signer against
an **expected identity** the user supplies.

This is the inverse of publisher's existing job (signing and broadcasting the
owner's posts). It is **read-only** and **never touches the owner's signing
key** or any platform credential.

**Purpose:** verify arbitrary third-party posts — impersonation detection and
provenance, not a self-audit of the owner's own pipeline.

### In scope (v1)

- Nostr: verify a pasted event JSON (offline, self-contained).
- Bluesky: full cryptographic proof — commit signature + MST inclusion.
- Mastodon: origin authority, plus FEP-8b32 object integrity proof when present.
- Report the actual signer; optionally match against a user-supplied expected
  identity.
- Surfaces: `POST /api/verify` JSON endpoint + a "verify" tab in the SPA.

### Out of scope (v1)

- Fetching Nostr events from relays by `nevent`/`note` (paste-only).
- Verifying attached media/blobs.
- Batch verification.
- FEP-8b32 `eddsa-rdfc-2022` cryptosuite (RDF canonicalization) — see §4.

## 2. Architecture

New isolated package `internal/verify`, with **no dependency** on the
credential-based publishing packages (`internal/nostr`, `internal/bluesky`,
`internal/mastodon`). Those packages are built around the owner's credentials
(nsec, app password, token); verification operates on arbitrary authors with no
credentials, so it gets its own home and clean boundaries.

```
internal/verify/
  verify.go      // Verdict/Check types, Verifier interface, dispatcher + auto-detect
  nostr.go       // Nostr event verifier (offline, self-contained)
  bluesky.go     // ATProto verifier (indigo: identity + repo CAR/MST)
  mastodon.go    // ActivityPub verifier (origin authority + FEP-8b32 JCS)
  safehttp.go    // SSRF-guarded http.Client (shared by bluesky/mastodon/nip05)
  testdata/      // captured CAR + AP/actor fixtures
  *_test.go
```

```go
type Verifier interface {
    Verify(ctx context.Context, in Input) Verdict
}
```

The dispatcher in `verify.go` inspects `Input`, selects the platform verifier
(or honors an explicit override), and returns a uniform `Verdict`. It is wired
into `internal/api` via a new `/api/verify` handler, and a fourth "verify" tab
is added to the SPA. `cmd/publisher` constructs the verifier (with the
SSRF-guarded client and the PLC directory URL from config) and injects it into
the `API` struct, mirroring how `Dispatch`/`Sync`/`Store` are injected today.

## 3. The `Verdict` data model (uniform across platforms)

```go
type Status string // "verified" | "failed" | "error"

type Verdict struct {
    Platform  string   `json:"platform"`             // nostr|bluesky|mastodon
    Status    Status   `json:"status"`
    Assurance string   `json:"assurance,omitempty"`  // "cryptographic" | "origin"
    Signer    *Signer  `json:"signer,omitempty"`     // who actually signed/vouched
    Expected  *Match   `json:"expected,omitempty"`   // present only if user supplied one
    Content   *Excerpt `json:"content,omitempty"`    // what was verified
    Checks    []Check  `json:"checks"`               // ordered, transparent sub-steps
    Warnings  []string `json:"warnings,omitempty"`
    Error     string   `json:"error,omitempty"`      // populated only when Status=="error"
}

type Check struct {
    Name   string `json:"name"`   // e.g. "signature_valid", "mst_inclusion", "handle_binding"
    Result string `json:"result"` // "pass" | "fail" | "skip"
    Detail string `json:"detail,omitempty"`
}

// Signer carries platform-specific identity fields; unused fields are omitted.
type Signer struct {
    // nostr
    PubkeyHex string `json:"pubkey_hex,omitempty"`
    Npub      string `json:"npub,omitempty"`
    // bluesky
    DID            string `json:"did,omitempty"`
    Handle         string `json:"handle,omitempty"`
    HandleVerified *bool  `json:"handle_verified,omitempty"`
    PDS            string `json:"pds,omitempty"`
    // mastodon
    ActorURI   string `json:"actor_uri,omitempty"`
    Acct       string `json:"acct,omitempty"` // user@domain
    OriginHost string `json:"origin_host,omitempty"`
}

type Match struct {
    Provided string `json:"provided"`
    Matches  bool   `json:"matches"`
    Detail   string `json:"detail,omitempty"`
}

type Excerpt struct {
    Text      string `json:"text,omitempty"`
    CreatedAt string `json:"created_at,omitempty"`
    Kind      string `json:"kind,omitempty"` // nostr kind / bsky collection
}
```

### Tri-state verdict (the central design decision)

"Could not verify" must be distinct from "verified it and it's forged":

- **`verified`** — authentic; the correct key signed this.
- **`failed`** — the check completed and the signature/inclusion is **invalid**
  → tampering or impersonation. A strong, alarming signal.
- **`error`** — the check **could not complete** (network down, DID
  unresolvable, post deleted, malformed input). This is **not** evidence of
  forgery and must never be presented as one.

Collapsing `error` into `failed` would be a security-UX bug: a flaky network
would scream "FORGED" and train the user to ignore the tool.

`assurance` is the second honesty lever: `cryptographic` (Nostr, Bluesky,
Mastodon-with-FEP-8b32) vs `origin` (plain Mastodon — "the actor's domain
vouches for this," not a portable signature). The `Checks` list makes every
sub-step visible so the verdict is auditable rather than a black box.

## 4. Per-platform verification flows

### Nostr (`nostr.go`) — fully offline

1. Parse pasted JSON into `nostr.Event` (`fiatjaf.com/nostr`).
2. `event.CheckID()` — recompute the id from the canonical serialization;
   mismatch → `failed` (`id_matches` check) — content was tampered.
3. `event.VerifySignature()` — Schnorr signature over the id by `event.PubKey`;
   invalid → `failed` (`signature_valid` check).
4. Signer = `{ pubkey_hex, npub }`. Content excerpt = `{ text, created_at, kind }`.
5. **Expected match:** if the user supplies an `npub`/hex/NIP-05, resolve it
   (`nip19.Decode` for npub/hex, `nip05.QueryIdentifier` for `name@domain`) to a
   pubkey and compare to `event.PubKey`.

Assurance is always `cryptographic`. No network unless an expected NIP-05 must
be resolved.

### Bluesky (`bluesky.go`) — full cryptographic proof via indigo

1. Parse URL → `at://{did|handle}/app.bsky.feed.post/{rkey}`. Accept both
   `https://bsky.app/profile/{handle|did}/post/{rkey}` and raw `at://` URIs
   (`atproto/syntax`).
2. `directory.Lookup(ctx, atid)` → DID document, PDS endpoint, handle. This
   performs **bidirectional handle↔DID verification by default** →
   `handle_binding` check; result feeds `Signer.HandleVerified`.
3. Fetch `{pds}/xrpc/com.atproto.sync.getRecord?did=…&collection=app.bsky.feed.post&rkey=…`
   → CAR bytes (SSRF-guarded client, size-capped).
4. `repo.VerifyCommitSignatureFromCar(ctx, dir, car)` → verifies the commit
   signature against the DID's `atproto` signing key (secp256k1/p256) →
   `commit_signature` check.
5. `repo.LoadRepoFromCAR` → confirm the record at `(collection, rkey)` is
   present in the MST under the verified commit root → `mst_inclusion` check.
   (Exact inclusion-check call to be confirmed against the pinned indigo version
   during implementation.)
6. Signer = `{ did, handle, handle_verified, pds }`. **Expected match:** user
   supplies a handle or DID → compare.

Assurance is `cryptographic`. A deleted record (commit verifies but record not
in MST) → `failed` with `mst_inclusion` failing.

### Mastodon (`mastodon.go`) — origin authority + FEP-8b32 if present

1. Fetch the URL with `Accept: application/activity+json` (SSRF-guarded);
   follow to the AP object representation.
2. Parse `id`, `attributedTo` (actor URI), `published`, `content`.
3. **Origin authority:** confirm the object `id`'s host matches the actor's host
   and that host served the object; fetch the actor →
   `acct = preferredUsername@host` → `origin_authority` check. Assurance =
   `origin`.
4. **FEP-8b32:** if a `DataIntegrityProof` is present with cryptosuite
   `eddsa-jcs-2022`, resolve `verificationMethod` against the actor's
   `assertionMethod` key, JCS-canonicalize (RFC 8785), verify Ed25519 →
   `integrity_proof` check; assurance upgraded to `cryptographic`.
5. Signer = `{ actor_uri, acct, origin_host }`. **Expected match:** user supplies
   `@user@domain` → compare to `acct`.

**FEP-8b32 scope cut:** the spec allows two cryptosuites. `eddsa-jcs-2022` uses
JCS (RFC 8785), which is tractable. `eddsa-rdfc-2022` requires RDF dataset
canonicalization (URDNA2015), a large and gnarly algorithm. v1 supports the JCS
variant only; an `rdfc` proof is reported as "present but unsupported" → falls
back to `origin` assurance with a warning, rather than shipping a fragile RDF
normalizer.

## 5. Input handling & auto-detection

```go
type Input struct {
    Raw      string // pasted event | URL | at:// uri
    Platform string // optional explicit override
    Expected string // optional expected identity
}
```

Detection order:

1. Explicit `Platform` always wins.
2. Parses as JSON containing `id`/`pubkey`/`sig` → **Nostr**.
3. `at://` URI, or URL host `bsky.app` / `*.bsky.app` → **Bluesky**.
4. Any other `https://` URL → **Mastodon/ActivityPub**.
5. A pasted `nevent`/`note1…` → rejected in v1 with a clear "paste the full
   event JSON" message.

## 6. API surface

`POST /api/verify`, JSON in / JSON out, behind the existing CSRF +
security-header middleware, body-capped (512 KB).

```json
// request
{ "input": "<pasted event | URL | at:// uri>", "platform": "", "expected": "" }

// response: the Verdict from §3
```

- HTTP 200 for any **completed** verdict, including `Status:"failed"`.
- HTTP 400 for malformed/unparseable input.
- HTTP 502/504 only when the verifier itself errored on network/timeout
  (mirrors `Status:"error"`).

## 7. Web UI (the "verify" tab)

A fourth tab in `index.html` plus a `verify.js` module, matching the existing
vanilla-JS, no-framework, dark-terminal aesthetic (like `tools.js`). Layout:

- One input (textarea/URL) for the event/URL.
- An optional "expected identity" field.
- A "Verify" button.
- A result card rendering: a status chip (✓ verified / ✗ failed / ⚠ error),
  the assurance badge (`cryptographic` / `origin`), the resolved signer, the
  expected-match line when provided, the content excerpt, and an expandable list
  of the individual `Checks`.

Honors the existing strict CSP (no inline scripts/styles; same-origin assets
only). Added to the `data-view` tab switching in the existing SPA shell.

## 8. Security considerations

- **SSRF guard (`safehttp.go`):** verification fetches *arbitrary
  user-controlled hosts* (Mastodon instances, PDS endpoints from DID docs,
  NIP-05 domains). The shared `http.Client` validates the **resolved IP** on
  every dial and after each redirect, rejecting
  loopback/private/link-local/ULA/unspecified ranges. Response bodies are
  size-capped; redirect count is bounded; per-request timeout from config.
- **No key access:** `internal/verify` receives no secret config — it is
  structurally incapable of touching `NSEC_HEX` / app passwords / tokens.
- **DoS bounds:** CAR/response size caps; context timeouts on every outbound
  call; request body cap on the endpoint.
- Reuses the existing CSP / CSRF / security-header middleware unchanged.

## 9. Error handling

- Every external failure (network, DNS, timeout, unresolvable DID, deleted
  post, 5xx from a PDS/instance) maps to `Status:"error"` with a human message
  in `Error` — never `failed`.
- Parse failures of user input → HTTP 400.
- A completed check that fails cryptographically → `Status:"failed"` with the
  specific failing `Check` named.
- Each platform verifier is wrapped so a panic cannot take down the handler.

## 10. Testing strategy

- **Nostr:** table tests, fully offline — known-good event (passes),
  byte-flipped content (`id_matches` fails), byte-flipped sig
  (`signature_valid` fails), expected-match hit/miss.
- **Bluesky:** captured real CAR + DID-document **fixtures** in `testdata/`;
  verify offline against a fake `identity.Directory`; tampered CAR → `failed`;
  deleted-record CAR → `mst_inclusion` fail.
- **Mastodon:** AP object + actor JSON fixtures served by `httptest`;
  origin-host match/mismatch; a FEP-8b32 (JCS) signed fixture → `cryptographic`;
  unsigned → `origin`.
- **SSRF:** unit tests with an injected resolver asserting private IPs are
  refused (including after a redirect).
- **Dispatcher:** table tests over representative inputs (JSON event, bsky.app
  URL, at:// URI, mastodon URL, nevent rejection).

## 11. Dependencies & risks

- **New dependency:** `github.com/bluesky-social/indigo` (atproto SDK subset:
  `atproto/atcrypto`, `atproto/repo`, `atproto/repo/mst`, `atproto/identity`,
  `atproto/syntax`). Measured impact: binary ~13.3 MB → ~18–22 MB; full module
  graph grows to ~248 modules (~+50 require entries). Accepted in the
  Bluesky-deps decision (B1) as the price of canonical, well-tested
  Merkle/signature code for a security feature.
- **Risk — FEP-8b32 fixtures:** real-world JCS-signed Mastodon posts are scarce
  (Mastodon does not emit them as of early 2026). Test against another
  fediverse implementation's output or a hand-constructed fixture; treat the JCS
  path as best-effort with strong coverage on the canonicalization step.
- **Risk — indigo API drift:** pin the exact version in `go.mod`. Verification
  entrypoints (`VerifyCommitSignatureFromCar`, `LoadRepoFromCAR`,
  `Directory.Lookup`) confirmed against `v0.0.0-20260520161040-0eb7e0ea71bc`.
- **Risk — Bluesky DID/handle resolution does DNS/HTTP:** covered by the SSRF
  guard and timeouts.

## 12. Configuration (new env vars)

| Var | Required | Default | Purpose |
|---|---|---|---|
| `PLC_DIRECTORY_URL` | no | `https://plc.directory` | DID PLC resolver for Bluesky identity lookups |
| `VERIFY_HTTP_TIMEOUT` | no | `10s` | Per-request timeout for verification fetches |

No new secrets. Verification is enabled unconditionally (no credentials
required).

## Decision log

- **Purpose:** verify arbitrary posts (impersonation/provenance), not self-audit.
- **Identity model:** always report the actual signer; optionally match against a
  user-supplied expected identity.
- **Bluesky depth:** full cryptographic proof (commit signature + MST inclusion).
- **Mastodon:** origin authority + FEP-8b32 (JCS variant) when present, clearly
  labeled via `assurance`.
- **Surface:** web UI tab + JSON API.
- **Architecture:** isolated `internal/verify` package (Approach A).
- **Bluesky crypto deps:** indigo atproto SDK (Approach B1).
- **Verdict shape:** tri-state (`verified`/`failed`/`error`) + assurance level +
  transparent `Checks`.
