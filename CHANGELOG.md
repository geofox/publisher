# Changelog

Notable changes to Publisher. Releases are tagged `vX.Y.Z`; `main` publishes
the `edge` image.

## Unreleased

### Breaking

- **Thread breaks are positions, not `---`.** A post's text is split
  where `content.breaks` says (code point positions); `---` on its own
  line is now ordinary text. A caller that relied on `---` (an n8n flow,
  say) must send `breaks` instead, or leave the splitting to each
  network's limit. Media go with the post that holds them
  (`media[].at`); the old `media[].part` is refused with code `replaced`.
  Each account can have its own breaks and media positions
  (`targets[].thread`). Stored drafts are converted at the first start.

### Changed

- The web app edits a thread per network: shared cards, a tab per
  network with its own text and breaks, and a side-by-side comparison
  (Alt+C).
- An OIDC email allowlist (`OIDC_ALLOWED_EMAILS`) matches only an email
  the identity provider says is verified. Stored emails are cleared once,
  so an unverified one stops passing; a refused sign-in logs the subject
  to allow instead.
- Automatic retries no longer resend a failure older than the retry
  policy's whole span.
- After a restart, an item that was being sent is resent only when it
  never reached the network; otherwise it is marked unknown for you to
  check.
- Photos lose their EXIF, GPS and XMP metadata before upload, and a
  rotated JPEG is stored upright.
- Schedule times in the web app use the device's time zone.
- Nothing is required to start. `NSEC_HEX` is needed only for a Nostr
  account and `BLOSSOM_URL` only for media; a server with only Bluesky,
  say, starts and posts text. Without `BLOSSOM_URL`, uploads answer `503`
  ("no media store is configured"), `GET /v1/instance` says
  `features.media: false`, and the composer hides the attach button.
  Posts made while it was set still fetch their media for a retry.
  `BLOSSOM_URL` without `NSEC_HEX` stops startup (uploads are signed with
  the Nostr key). The startup log lists each account with its status, and
  warns about each one that isn't connected.
- Boolean environment variables (`AUTO_RETRY_ENABLED`, `NOSTR_PRIMARY_FANOUT`,
  `OIDC_END_SESSION`) accept `1/true/yes/on` and `0/false/no/off` in any
  case. Any other value now stops startup with an error naming the variable,
  instead of silently keeping the default.
- A configuration error at startup lists every invalid variable, not only
  the first.
- `OWNER_PUBKEY` is optional: the public key is derived from `NSEC_HEX`. When
  it is set it must still match, and setting it without `NSEC_HEX` is an
  error.
- A destination whose credentials are set without the address that goes
  with them now stops startup with an error naming the missing variable,
  where it used to fail every send: `MASTODON_TOKEN` needs
  `MASTODON_BASE_URL`, and `BLUESKY_APP_PASSWORD` needs `BLUESKY_IDENTIFIER`
  and `BLUESKY_PDS_URL`.
- Pasting a Bluesky or Mastodon post link when that network has no account
  says the network is not configured, instead of failing at sign-in.
- `NSEC_HEX` must be exactly 64 hex characters. A shorter value used to be
  padded with zeros in front, so `NSEC_HEX=1` signed with a key anyone can
  compute; now startup stops and says so. If a tool printed your key
  without its leading zeros, add them back.
- A negative `POW_DIFFICULTY_DEFAULT` or `POW_DIFFICULTY_MAX` stops startup
  with an error naming the variable; it used to mean 0 (no proof of work).
  Use `0` for that.
- Accounts are known by id everywhere. A post names accounts, not
  networks, in the engine, retries, the Nostr fan-out and recovery after a
  restart, so two accounts of one network can be sent to independently.
- The external readiness checks (`/readyz?external=1`) are named
  `account:<id>`, one per account that is turned on, instead of
  `nostr_relay`, `bluesky`, `mastodon` and `threads`; `blossom` stays.
  Update a monitor that matched the old names.
- Each Nostr account has its own relay-sync list, home relay and key. On
  upgrade the existing list moves to the Nostr account set by the
  environment.
- Each account shows its own profile (name, avatar, handle) in
  `GET /v1/accounts`; profiles are fetched in the background and never
  delay the answer. An account that is turned off isn't asked for its
  profile.
- A pasted post is looked up with your first connected account on its
  network (a reply with the account that replies); with every account on
  that network turned off it says none is connected.
- `GET /v1/public/feed` links carry `account` (the id of the account that
  posted) and its `handle`.
- A post can go to two accounts of one network (two Mastodon accounts,
  say), each counted by its own limits. `duplicate_type` is gone; the same
  account twice is still `duplicate`. A reply, quote or repost is made by
  one account on the original's network; a second one there is refused
  with `one_native_actor`.
- `GET /v1/accounts` says whether each account can be used (`status`,
  `status_reason`), its place in the app's order (`order`), its
  `remote_id` and `profile_url`, and the owner's `default_selected` and
  `public_feed` flags; `GET /v1/accounts/{id}` has an `ETag`.
- `POST /v1/resolve` takes `account`, the account to look the post up
  with.
- The `/v1/events` stream's `post` field is absent on the new account
  events, which only a caller with `accounts:read` receives.
- Alerts name the account, not only its network: "Mastodon
  (@me@example.social)" where they said "Mastodon". An alert rule that
  matches the exact text needs updating.

### Added

- `ALLOWED_HOSTS`: the host names the server answers for (others get
  `421`). Recommended when OIDC is off.
- `PATCH /v1/accounts/{id}` changes an account's label, turns it off or
  on, and sets whether new posts select it and whether its posts go to the
  public feed. It needs the new `accounts:write` scope, which `admin`
  doesn't include and only the signed-in owner can grant to a token. Each
  change is an `account.updated` event naming the fields that changed.

### Security

- Redirects from https to http are no longer followed for any network
  request a destination makes, and a request body (a sign-in, a post, an
  upload) is replayed on a 307 or 308 redirect only to the same host or one
  of its subdomains. A `MASTODON_BASE_URL` or `BLUESKY_PDS_URL` that
  redirects elsewhere now fails with "refused to send a request body to
  another host": set it to the address the server redirects to.

### Other changes

- Mastodon posts and uploads, and Bluesky media uploads, use HTTP/2 when
  the server offers it, like the other Bluesky and Threads requests already
  did, and Bluesky media uploads now go through `HTTPS_PROXY` when it is
  set, like the rest of Bluesky's requests. Connections that stay silent are checked
  with an HTTP/2 ping and closed if the server doesn't answer, so one dead
  connection can't stall every post to that server. Connecting to a server
  times out after 10 seconds (30 before, for some requests).
