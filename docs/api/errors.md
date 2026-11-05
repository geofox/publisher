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
scope (`admin` includes every scope).

## cross_origin

**403.** A state-changing request came from a web page on another site
(its `Origin` header names another host). This blocks cross-site request
forgery against a signed-in browser. Scripts and servers send no `Origin`
and are not affected.

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
token that would outlive it). For a reply, quote or repost the source URL
can be `unrecognized`, `not_supported` (Threads), `not_configured` or
`unresolvable` (not found, deleted, private, or not yet known to your
server); `not_allowed` and `missing_source_account` concern the action
and its targets. The tools add `unsupported_language` (translate) and
`unknown_relay` (relay sync: a relay that isn't one of the account's).

## invalid_parameter

**400.** A query parameter has a value the operation does not accept, such
as `limit=0`. `errors[]` names the parameter.

## invalid_cursor

**400.** `cursor` is not one this server issued. Pass `next_cursor` from the
previous page unchanged; cursors are opaque.

## invalid_state

**409.** The request is valid but the resource is not in a state that
allows it: rescheduling a post that isn't scheduled, hiding one still to be
sent, canceling a post with nothing left to cancel, or retrying one with
nothing to retry. `detail` says which.

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

## unreadable_upload

**400.** An upload's body could not be read: the connection dropped, or a
`multipart/form-data` body is malformed. Send the file again.

## unavailable

**503.** A dependency the operation needs is not there: no database, or a
feature that is not configured. Retry later or check the server's
configuration.

## upstream_failed

**502.** A service the server depends on failed: media storage (Blossom)
refused or could not take the file, or the network of a post you reply to,
quote or repost could not be reached (a timeout, a 5xx, a rate limit).
Retry later; for media, check the media storage configuration.

## insufficient_storage

**507.** The server ran out of disk space while receiving an upload. Try
again later; the server's owner needs to free space.

## internal

**500.** The server failed. The error is logged; retrying with the same
`Idempotency-Key` is safe, as failed responses are not stored.
