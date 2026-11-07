# /v1 error catalogue

Every `/v1` error is an [RFC 9457](https://www.rfc-editor.org/rfc/rfc9457.html)
problem, sent as `application/problem+json`:

```json
{
  "type": "https://github.com/geofox/publisher/blob/main/docs/api/errors.md#validation_failed",
  "title": "Validation failed",
  "status": 422,
  "detail": "the request has invalid fields",
  "instance": "/v1/tokens",
  "code": "validation_failed",
  "errors": [
    {"pointer": "/scopes/0", "code": "unknown_scope", "detail": "one of posts:read, …"}
  ]
}
```

- `code` is stable: match on it, not on `title` or `detail`, which are
  for people and may change.
- `type` is this page plus `#` and the code.
- `errors[]`, when present, lists each problem with one part of the
  request: `pointer` is a JSON Pointer (RFC 6901) into the request body,
  or `parameter` names a query parameter. Each has its own `code` and a
  `detail`.

The codes follow, one section each. A test keeps this list in step with
the server.

## not_found

**404.** No such route, or no such resource for this caller. A resource
that belongs to another owner is reported as not found, never as
forbidden.

## method_not_allowed

**405.** The path exists but not with this method. The `Allow` header
lists the methods it takes.

## unauthenticated

**401.** No credentials, or credentials the server does not accept: an
`Authorization` header that is not `Bearer <token>`, or a token that is
unknown, revoked or expired. The `WWW-Authenticate` header says which
(`error="invalid_token"` for a bad token, per RFC 6750). Sign in, or send a
valid token.

## insufficient_scope

**403.** The token is valid but lacks the route's scope. The
`WWW-Authenticate` header names the scope needed. Create a token with that
scope. `admin` includes every scope except `accounts:write`, which must
be granted by name; only the signed-in owner (not a token) can create a
token holding it.

## reauth_required

**403.** The operation needs a fresh sign-in: adding or removing a way to
sign in, setting the first password, creating a token from a browser
session (any token: it outlives the session), deleting an account,
showing an account's key, or changing the server's secret settings
(`instance.secrets`). Re-authenticate with `POST /auth/reauth`
(a passkey or the password; with OIDC, a fresh login through
`/auth/reauth/oidc`), then send the nonce it returns in the
`Publisher-Reauth` header. The problem's `operation` names what to
re-authenticate for; `account.delete` and `account.reveal` also take the
account's id as `target`, which the problem names too. A nonce is good once, for five minutes, for
that operation (and target) and that browser session.

## session_required

**403.** Only the owner signed in in a browser can do this: a token
can't, whatever its scopes. Managing sign-in methods and sessions, and
re-authenticating, are browser-only.

## sign_in_required

**403.** The server runs behind a reverse proxy without its own sign-in
(`AUTH_MODE=proxy`), and this operation needs to know it is the owner:
managing sign-in, creating a token that holds `admin`, and managing
credentials (adding an account, changing its credentials, adopting one
from the environment, showing a key, deleting one), whoever asks, a
token included. Set up sign-in (`AUTH_MODE=local` with `PUBLIC_URL`, or
OIDC).

## last_sign_in_method

**409.** Removing this passkey or the password would leave no usable way
to sign in (a passkey made for another address doesn't count). Add
another passkey or set a password first.

## gone

**410.** The thing asked for no longer exists and won't come back: the
first-run setup is done (each `/setup/…` route answers this afterwards;
the page at `/setup` answers 410 with a short HTML page saying so).

## rate_limited

**429.** Too many requests: wrong passwords from your address (or, from a
browser that hasn't signed in before, from many places), too many passkey
requests or failed passkeys, or password checks already running; more
than 10 accounts added or changed in a minute; an account checked again
within 10 seconds. The `Retry-After` header says how many seconds to
wait. A passkey is never held back by wrong passwords.

## cross_origin

**403.** A state-changing request came from a web page on another site
(its `Origin` header names another host). This blocks cross-site request
forgery against a signed-in browser. Scripts and servers send no `Origin`
and are not affected.

## misdirected

**421.** The request's `Host` is not one this server serves: the server
has `ALLOWED_HOSTS` set and the name isn't on it. This stops a web page
that points its own name at the server (DNS rebinding) from using the app
as if it were the same site. Add the name to `ALLOWED_HOSTS`.

## invalid_json

**400.** The body is empty, is not JSON, has a field the operation does
not define, has a value of the wrong type, or has data after the JSON
value. `detail` says which.

## unsupported_media_type

**415.** The operation takes JSON and the request's `Content-Type` is not
`application/json` (parameters such as `charset` are fine) or is missing.
It is required: a cross-site form can't send it, which backs up the CSRF
check for signed-in browsers.

## too_large

**413.** The body is larger than the operation accepts (1 MiB for JSON
bodies; see `GET /v1/instance` for the limits).

## validation_failed

**422.** The body is well-formed JSON but some values are not acceptable.
`errors[]` lists every invalid field, so a client can show them all at once.
Field codes include `required`, `too_long`, `unknown_scope`,
`invalid_time`, `in_past` and `exceeds_caller` (a token tried to create a
token that would outlive it, or one holding `accounts:write`). For a reply, quote or repost the source URL
can be `unrecognized`, `not_supported` (Threads), `not_configured` (none
of your accounts on the post's network is connected) or
`unresolvable` (not found, deleted, private, or not yet known to your
server); `not_allowed` and `missing_source_account` concern the action
and its targets, and `one_native_actor` a second target on the post's own
network (the post exists once there, so one of your accounts acts on it;
send from the others separately). A target's account that can't send
now is `account_disabled`; `detail` says why (turned off, not
configured, locked, needs to be connected again). The same account twice
is `duplicate`. `POST /v1/resolve` with `account` adds, on `/account`,
`unknown_account` (not one of yours), `not_allowed` (on another network)
and `account_disabled`. `PATCH /v1/accounts/{id}` uses `required`,
`too_long` and `invalid` (a control character, or a value of the wrong
type, `null` included). Adding or changing an account (`/type`,
`/method`, `/config/…`, `/credentials/…`) adds `unknown_field` (not a
field of the type or method), `not_allowed` (an unknown type or method,
a method this version can't use, a private or local address the operator
didn't allow, or a new key on an existing account), `not_found` (a
Bluesky handle or DID that leads to no account), `too_many` and
`out_of_range` (a list or a number beyond its field's bounds; `invalid`
covers a value of the wrong form, an address included), and `required`
on `/credentials` when a new method or server address needs the
credentials again. The tools add `unsupported_language` (translate) and
`unknown_relay` (relay sync: a relay that isn't one of the account's).
Resolving an unknown item (`:resolve`) adds `unknown_item`, and for its
`url` `not_found` (no such post), `not_own` (another account's post),
`duplicate` (another part of the same thread) and `not_permitted` (the
account's token can't look posts up, e.g. Mastodon without `read:search`).
Drafts add `unknown_media` and `unknown_account` (media or accounts that
aren't yours), and posting one (`draft` on `POST /v1/posts`) adds
`unknown_draft`, `draft_changed` (saved again meanwhile) and
`not_permitted` (the token lacks `drafts:write`).
Thread positions (`content.breaks`, `media[].at`, `targets[].thread`) add
`out_of_range` (beyond the text), `not_ascending`, `inside_grapheme` (a
break that would cut a character in two), `too_many`, `length_mismatch`
(`thread.media_at` doesn't have one position per media) and `replaced`
(`media[].part`, which `at` replaced). A text a platform can't take even
when split, such as more distinct links than Threads allows in one post,
is `refused_by_destination`. Media a network can't take are
`media_unsupported`, including media on this server's disk for a network
that fetches media by address (Nostr, Threads) while the disk doesn't
serve them publicly. Turning `media.public` on adds `not_reachable` at
`/media/public` when the probe at `PUBLIC_URL/media/` didn't come back
whole; `detail` says what was answered.

## invalid_parameter

**400.** A query parameter has a value the operation does not accept, such
as `limit=0`. `errors[]` names the parameter.

## invalid_cursor

**400.** `cursor` is not one this server issued. Pass `next_cursor` from the
previous page unchanged; cursors are opaque.

## invalid_state

**409.** The request is valid but the resource is not in a state that
allows it: rescheduling a post that isn't scheduled, hiding one still to be
sent, canceling a post with nothing left to cancel, retrying one with
nothing to retry, or a relay operation on a Nostr account that isn't
connected now (or while a scan or sync of its relays is running), a
passkey request that expired, was used, or is another session's,
checking an account that isn't open, showing a key an account doesn't
have, or adopting an account already in the app (or one the environment
no longer configures). `detail` says which.

## idempotency_key_invalid

**400.** The `Idempotency-Key` header is malformed. Send one key: a quoted
string of 1 to 255 visible ASCII characters (`Idempotency-Key: "6f1c2b"`),
as the IETF draft specifies; a bare value without quotes or spaces is also
accepted.

## idempotency_key_in_use

**409.** An earlier request with this key is still being processed. Wait
(`Retry-After` suggests how long) and send the request again with the same
key: you will get that request's response.

## idempotency_key_reused

**422.** This key was already used, by this caller, for a different
request (another body or another route). A key names one request: use a new
key for a new request. Keys are kept for 24 hours.

## precondition_failed

**412.** The `If-Match` header names a version the resource is no longer
at: it changed since you read it (a draft saved from another tab, or an
account's settings changed by another client).
The response's `ETag` is the current version. Read the resource again,
merge, and retry with the new ETag, or send `If-Match: *` to overwrite.

## unreadable_upload

**400.** An upload's body could not be read: the connection dropped, or a
`multipart/form-data` body is malformed. Send the file again.

## unavailable

**503.** A dependency the operation needs is not there: no database, or a
feature that is not configured. Retry later or check the server's
configuration.

## upstream_failed

**502.** A service the server depends on failed: media storage (Blossom)
refused or could not take the file, the network of a post you reply to,
quote or repost could not be reached (a timeout, a 5xx, a rate limit),
or the network of an account being added or changed didn't answer.
Nothing was stored. Retry later; for media, check the media storage
configuration.

## credentials_rejected

**422.** The network refused the credentials of an account being added or
changed: a wrong or revoked app password or token, or one without the
rights Publisher needs. `errors[]` has one entry on `/credentials` whose
`detail` is the network's own words (at most 200 characters, without
anything you sent). Nothing was stored.

## duplicate_account

**409.** You already have this network account (the same type and remote
id); `account_id` names it. Change that one instead of adding it again.

## locked_by_env

**409.** Environment variables configure this account, so the app can't
change its configuration or credentials, delete it, or show its key.
Change the variables (and restart), or adopt the account into the app
first (`POST /v1/accounts/{id}:adopt`).

## identity_changed

**409.** The new credentials belong to another account on the network
than the one this account is. Add that one as a new account instead.

## master_key_required

**409.** Saving credentials needs a master key, and none is set. Set
`PUBLISHER_MASTER_KEY` (or `PUBLISHER_MASTER_KEY_FILE`; `publisher
keygen` makes one) and restart, then retry.

## master_key_mismatch

**409.** The stored credentials were saved with another master key than
the one set now, so new ones aren't saved (the database never mixes
keys). Set the master key they were saved with (or keep it as
`PUBLISHER_MASTER_KEY_PREVIOUS` while rotating) and restart.

## connect_failed

**502.** Signing in at a network couldn't be prepared, or the account
couldn't be made from what it answered (`POST /v1/accounts:connect`,
`:reconnect`, a connect flow's `:choose`). `connect_error` says why, as
one of the fixed codes a connect flow's `error` uses: `exchange_failed`
(the network didn't answer as expected), `issuer_mismatch` (the
server's sign-in metadata names another address than the server's),
`denied`, `expired`, `state_invalid`, `session_mismatch`,
`identity_changed`, `duplicate_account`, `master_key_required`. The
network's own words are only in the server's log. Check the server's
address, then try again.

## insufficient_storage

**507.** The server ran out of disk space while receiving an upload, or
media kept on its disk would pass the quota set in Settings, Media or
the free space it always leaves (1 GiB, or 5 % of the volume). `detail`
says which. Try again later; the server's owner needs to free space or
raise the quota.

## internal

**500.** The server failed. The error is logged; retrying with the same
`Idempotency-Key` is safe, as failed responses are not stored.
