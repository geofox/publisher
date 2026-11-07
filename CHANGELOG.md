# Changelog

Notable changes to Publisher. Releases are tagged `vX.Y.Z`; `main` publishes
the `edge` image.

## 3.0.0 (unreleased)

Settings and accounts in the web app. Read
[Upgrading from 2.x](README.md#upgrading-from-2x) before you upgrade.

### Breaking

- **`enabled` on an account is now only your switch.** It used to be
  false for a network whose variables weren't set; after the upgrade
  every account starts with `enabled: true`, and `status` says whether it
  can be used. A caller that picked targets with `enabled` must use
  `status == "connected"` instead, or a post naming an account that
  isn't configured is refused (`account_disabled`).

- **Sign-in modes (`AUTH_MODE`).** A new install without OIDC signs you
  in with a passkey (`local`) and needs `PUBLIC_URL`; an existing one
  without OIDC keeps running behind your proxy alone (`proxy`) until you
  set `AUTH_MODE=local`. In proxy mode a token holding `admin` or
  `accounts:write` can't be created any more (`403 sign_in_required`).
  With OIDC, creating a token in the web app (any token) sends you
  through your provider's login again (Authelia 4.39 or later). A token
  can no longer create a token holding `admin` (`403 session_required`).
- **Relay sync reaches only public relays unless you list a private
  one.** The relays a scan or sync compares with the home relay (your
  NIP-65 ones and those added under Tools) are addresses you set in the
  app, so they now go through the same guard as accounts added in the
  app: a relay on a private address (`ws://192.168.1.5:7777`, a
  Tailscale name) needs `PRIVATE_DESTINATIONS`, and loopback, link-local
  and the server's own addresses are never reached. The home relay is
  dialed as before.
- **The Nostr relay defaults are public relays.** Unset,
  `NIP65_BOOTSTRAP_RELAY` (the home relay: the relay list is read there,
  and every post goes there too) is now `wss://relay.damus.io`, and
  `FALLBACK_RELAYS` (used when the relay list can't be read or is empty)
  is `wss://nos.lol,wss://relay.damus.io,wss://relay.primal.net`. Both
  used to name this project's author's relay, `wss://relay.geoffrey.one`.
  A Nostr account from the environment that relied on them gets its
  relay list from relay.damus.io and posts there; to keep the old
  behaviour, set `NIP65_BOOTSTRAP_RELAY=wss://relay.geoffrey.one` and
  `FALLBACK_RELAYS=wss://relay.geoffrey.one,wss://nos.lol,wss://relay.damus.io`.
  Accounts added in the app keep the relays they were saved with.

### Changed

- The web app has a **Sign out** button again (under Settings → Access). It signs
  out with fetch: a form post from the app, whose `Referrer-Policy` is
  `no-referrer`, sends `Origin: null`, which the cross-site guard
  refused; that is why the old button was turned off.
- The web app picks accounts, not networks. The composer lists each
  account (grouped by network, with its avatar and handle, and why one
  can't be used); the thread editor's tabs (each with its own text and
  breaks), Compare columns, counts, limits and the preview are per
  account; History names each post's accounts and filters by
  account; Interact picks the account that replies when a network has
  several; Tools shows the relays of the Nostr account you pick. Alt+1 is
  Shared and Alt+2…Alt+9 the account tabs, and the arrow keys move along
  the tab row. A new post selects the connected accounts with
  `default_selected`. Unsaved work the browser kept from before is
  restored onto the account that stands for each network; one whose
  account can't post now comes back selected and says why (a network
  where you have no account is left out, saying so).
- **Retries off** (`AUTO_RETRY_ENABLED=false`, or Settings → Publishing)
  now holds every waiting failure, including the retries scheduled
  before they were turned off: those wait for **Retry** by hand. First
  tries, posts waiting for an account and retries by hand still go.
  Turned on again, they go at their time, without a restart.
- Nothing is required to start. `NSEC_HEX` is needed only for a Nostr
  account; a server with only Bluesky, say, starts and posts text.
  Without a media store (Settings → Media, or `BLOSSOM_URL`), uploads
  answer `503` ("no media store is set"), `GET /v1/instance` says
  `features.media: false`, and the composer hides the attach button.
  Posts made while one was set still fetch their media for a retry. The
  startup log lists each account with its status, and warns about each
  one that isn't connected.
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
- A post can't name an account that isn't connected (`account_disabled`,
  saying why). When an account stops being usable after a post was
  accepted, a locked account's items wait for it, each wait an
  `item.waiting` event with the reason, and any other account's items fail
  (retry them by hand once it's back). Canceling the post ends a waiting
  item.
- An item the engine gives up unsent because the network may already have
  it (its idempotency key expired) is now an `item.unknown` event; it used
  to change without one.
- The `/v1/events` stream's `post` field is absent on the new account
  events, which only a caller with `accounts:read` receives.
- Alerts name the account, not only its network: "Mastodon
  (@me@example.social)" where they said "Mastodon". An alert rule that
  matches the exact text needs updating.

### Added

- **Connect Mastodon and Threads accounts by signing in** at the network
  (Settings → Accounts → Add an account), in a new tab that sends you
  back to `<PUBLIC_URL>/oauth/callback/<network>`; the first tab
  finishes by itself. Mastodon needs nothing set up: Publisher registers
  itself on your server (PKCE on 4.3 and later). Threads goes through a
  Meta app you make once, set in **Settings → Integrations → Threads
  app**, which shows the redirect URI to register. An account added this
  way reconnects by signing in again (**Reconnect**, or **Sign in
  again** in its settings), and signing in as another account is
  refused. API: `POST /v1/accounts:connect`, `POST
  /v1/accounts/{id}:reconnect`, `GET /v1/connect-flows/{id}` and
  `:choose`; `GET /v1/destinations` gives each sign-in method its
  `redirect_uri` and why it can't start yet (`unavailable`); the
  instance settings `threads.app_id` and `threads.app_secret`.
- **When Meta's permission ends.** A Threads account's check reports
  the end of the 90 days Meta grants (`health.reconnect_by`); the card
  shows it, and an alert comes a week ahead, once a day, to sign in
  again.

- **Sign-in without OIDC** (`AUTH_MODE=local`): passkeys for
  `PUBLIC_URL`'s host and an optional password (argon2id, throttled per
  address; `TRUSTED_PROXIES` names the proxies whose `X-Forwarded-For`
  is believed). First-run setup by a one-time link in the log (or
  `SETUP_TOKEN_FILE`); `publisher setup-url` makes a new one, and
  `publisher owner reset-login [--revoke-tokens]` recovers a locked-out
  owner. After 20 wrong passwords in a row from anywhere, a short delay
  (at most 30 s) applies, except in a browser the owner signed in with
  before. Up to 20 passkeys. Passkeys made for an earlier `PUBLIC_URL`
  host are shown but don't count; with no other way in, setup opens
  again.
- **Confirm it's you:** adding or removing a way to sign in, setting the
  first password and creating an API token from the browser need a fresh
  passkey, password or OIDC login (a `Publisher-Reauth` nonce, single
  use, five minutes; `403 reauth_required` names the operation, and the
  account (`target`) when deleting an account or showing its key, which
  need it too).
  Changing the password takes the current one instead.
- **Settings → Access** (was the Tokens tab): passkeys, password, the
  browsers you're signed in on (sign any out), recent sign-ins and failed
  attempts, and API tokens. `/v1/me/sign-in`, `/v1/me/passkeys`,
  `/v1/me/password`, `/v1/me/sessions` and `/v1/me/sign-in-events`, for a
  browser session only. `GET /v1/instance` reports `auth: local` and, in
  proxy mode, `sign_in_recommended`.
- `PATCH /v1/accounts/{id}` changes an account's label, turns it off or
  on, and sets whether new posts select it and whether its posts go to the
  public feed. It needs the new `accounts:write` scope, which `admin`
  doesn't include and only the signed-in owner can grant to a token. Each
  change is an `account.updated` event naming the fields that changed. A
  turned-off account still renews its Threads token, so it works when
  turned on again.
- The composer selects, for a new post, the networks with a connected
  account that new posts select (`default_selected`), and posts from that
  account. The Tools tab says relay sync needs a Nostr account instead of
  showing an error.
- **Accounts added in the app** (`POST /v1/accounts`, with a master key):
  a Bluesky app password (the PDS is found from the handle or DID when
  left out), a Mastodon or Threads token, or a Nostr key (typed, or made
  for you and shown once with `:reveal`). Each is opened and asked who it
  is before anything is stored: refused credentials come back on the
  field with the network's own words, and the same network account can't
  be added twice. Accounts added in the app can change their settings
  and credentials (`PATCH`; a new server address needs the credentials
  again; a new handle with the same app password keeps the saved
  password while the server stays the same, and finds its server again
  unless you give it), be checked now (`:check`) and be deleted
  (re-authenticated; their scheduled posts are canceled, their history
  stays). An account from the environment can be moved into the app
  (`:adopt`); its variables are then ignored, and a startup warning
  names them while they are set. One whose variables are gone (`status:
  not_configured`) can be deleted; setting them again later makes a new
  account. `GET /v1/accounts` adds `auth_method`, `config`, which
  `credentials` are set (never their values), `locked_fields`, `health`,
  and `?include=deleted`. At most 10 accounts added or changed a minute.
  Behind the reverse proxy alone (`AUTH_MODE=proxy`) none of this is
  allowed; labels and flags still are.
- **Settings → Accounts** in the web app: your accounts by network, with
  their status, last check and why one can't be used; add one (network,
  then how it connects, then a form built from the network's fields,
  with the less common ones under Advanced), and rename, turn off, pick
  for new posts or the public feed, change settings and credentials,
  check now, show a Nostr key or delete it. A Nostr key made for you is
  shown right away, after confirming it's you, and the dialog stays
  until you tick that you saved it. An account from the environment
  shows its settings read-only, with **Move into the app** (or Delete,
  once its variables are gone). When saved
  credentials can't be opened (the master key is missing or another
  one), every tab says so. A new server opens Settings by itself, once:
  a master key made in the browser with the commands to give it to the
  container (skippable), then the first account. The composer with no
  account points to Settings.
- `publisher keygen` prints a new master key and the compose snippet to
  set it as a Docker secret.
- `PRIVATE_DESTINATIONS`: host names and private ranges that accounts
  added in the app (and relay sync) may reach; without it they reach
  public addresses only, and loopback, link-local and the server's own
  addresses never.
- **Settings in the app:** Settings → Composing (languages, the DeepL
  key), Publishing (retries, the scheduled-post grace), Integrations
  (the public feed's token and webhook, the alert webhook, each with a
  test) and Media, saved in the database and applied at once, without a
  restart (`GET/PATCH /v1/settings` and `/v1/instance/settings`, with
  `If-Match`; `:copy-from-env`, `:generate`, `:test`). A variable that is
  set still wins: the setting shows locked with its name, and **Copy
  into the app** saves its value so the variable can go (checked as a
  value typed in the app is; an address is copied with its secret, and
  `BLOSSOM_URL` with the store it chooses; the setting then reports
  `also_saved`). A secret from the environment is only sent to an
  address from the environment. Secrets are
  encrypted with the master key, never shown again, and refused behind
  the reverse proxy alone; one Publisher makes (a feed token) is shown
  once. The new scopes `settings:read`, `settings:write` and
  `instance:admin` are granted by name, not by `admin`.
- **Account sets** (`/v1/account-sets`, Settings → Composing): named
  selections of accounts, shown in the composer over the accounts; the
  default set starts every new post (else each account's **Picked for
  new posts**).
- **Each account's defaults** (`defaults` on `PATCH /v1/accounts/{id}`:
  a content warning, languages, the network's options such as
  Mastodon's visibility): a new post on the account starts with them in
  the composer, and a post sent through the API takes them unless it
  sets its own (a preview lists them, `defaults_applied`).
- **Media on this server's disk** (Settings → Media; `MEDIA_DIR`,
  default `media` beside the database): files named by their SHA-256,
  served at `/media/` without sign-in under `default-src 'none';
  sandbox`, with a quota and a free-space floor (`507`). Threads and
  Nostr fetch media by address, so they need **Serve media publicly**,
  which turns on only once a probe fetched at `PUBLIC_URL/media/` comes
  back (no redirect, no `401`); a sign-in in front must let `/media/`
  through. Until then, media there are refused for those networks.
- **A Blossom server set in the app**, with its own signing key (made or
  pasted in Settings → Media; allow its `npub` on the server). Uploads
  signed with it name the server (BUD-11). `BLOSSOM_URL` no longer needs
  `NSEC_HEX` to start: an upload says to make a key. Settings → Media's
  test asks the server (BUD-06) or writes and reads back the disk
  (fetched at `PUBLIC_URL/media/` when served publicly: other bytes fail
  as `wrong_answer`). A server set in the app is reached as any address
  set there (`PRIVATE_DESTINATIONS`), and the addresses it answers stay
  on its own origin; media are fetched again for a retry the same way.
  `/readyz` follows the store in effect (`blossom`, and `media_disk` for
  this server's disk).
- **Settings → System** (`GET /v1/instance/system`): the version, how you
  sign in, the master key, the health checks, and the variables the
  server started with; secrets only as set, and no user name, password,
  query or fragment from an address.
- **Export and import** (Settings → System). An export is a file of the
  accounts added in the app with their credentials, what you chose for
  the accounts from the environment, your account sets, and the settings
  saved in the app (the server's included), sealed with a passphrase of
  24 words that Publisher makes, shows once and doesn't keep
  (AES-256-GCM, the key from argon2id). An import reads the file with
  its passphrase, shows what it would do (each account and the server
  its credentials would go to, each set and setting), and does it once
  you confirm, within 10 minutes. It goes only into a server without
  accounts added in the app; what the environment sets there wins, and
  each account is checked as one added by hand and asked who it is
  (stored either way, marked when its network refused or didn't
  answer). Both ask you to confirm it's you and need a signed-in
  browser; the import needs the master key. API: `POST /v1/export`,
  `POST /v1/import` and `POST /v1/import:confirm`; at most 5 exports or
  files read a minute.

### Security

- Redirects from https to http are no longer followed for any network
  request a destination makes, and a request body (a sign-in, a post, an
  upload) is replayed on a 307 or 308 redirect only to the same host or one
  of its subdomains. A `MASTODON_BASE_URL` or `BLUESKY_PDS_URL` that
  redirects elsewhere now fails with "refused to send a request body to
  another host": set it to the address the server redirects to.
- A Nostr account's relays (publishing, the NIP-65 lookup, relay sync's
  home relay) are dialed through the account's network policy, as every
  other network request is; they used the Nostr library's own client.
- No request follows a redirect with a `Referer` (Go sends the previous
  address, a token in its query included), and a request to an address
  you set in the app that carries a token in its address doesn't follow
  a redirect to another server. Clouds' metadata addresses are never
  reached, even inside a range `PRIVATE_DESTINATIONS` lists
  (`100.100.100.200`, `fd00:ec2::254`), and IPv4-compatible IPv6
  addresses (`::a.b.c.d`), `192.0.0.0/24` and Azure's `168.63.129.16`
  count as private.
- A connection refused because of its address no longer says what the
  name resolved to ("dial tcp: refused to connect to a private or local
  address"): the error can reach the owner, and the server's internal
  addresses are not theirs to learn.
- **Credentials encrypted at rest** (`PUBLISHER_MASTER_KEY`, or
  `PUBLISHER_MASTER_KEY_FILE`; `publisher keygen` makes one). With a key
  set, the Threads token Publisher renews moves at the first start from
  its plaintext row into the encrypted secret store, the old row is
  overwritten, and the write-ahead log is truncated; the log then warns
  that the `.pre-v*.bak` backups made by earlier upgrades still hold it
  in plaintext. Each value is bound to its account and to the server it
  is sent to: a value copied to another row, or an account whose server
  address is edited in the database, fails to open, and the account is
  locked with a reason rather than send its credentials elsewhere. A
  renewed token saved for an account from the environment applies only
  while the environment's credentials and server are the ones it came
  from. `PUBLISHER_MASTER_KEY_PREVIOUS` rotates every stored value to the
  new key in one transaction at startup (all or nothing). Without a key,
  or with a key that doesn't match what is stored, accounts from the
  environment work as before; `GET /v1/instance` reports `secrets.status`
  (`none`, `ok`, `missing`, `mismatch`) and `/readyz` a non-critical
  `secrets` check. One exception: a Threads account from the
  environment whose renewed token was saved with another key is locked
  until that key is set (or the key is removed, to go back to
  `THREADS_ACCESS_TOKEN`), and says so.
- The log scrubs credentials from every string and error it prints (a
  token in a failed request's URL, an `nsec1` key, a Bluesky app
  password), not only where a caller remembered to. Alerts are scrubbed
  before they are sent. An upstream server's error answer is quoted in
  errors on one line and at most 300 bytes, and an HTML error page is
  named rather than copied.

### Other changes

- Mastodon posts and uploads, and Bluesky media uploads, use HTTP/2 when
  the server offers it, like the other Bluesky and Threads requests already
  did, and Bluesky media uploads now go through `HTTPS_PROXY` when it is
  set, like the rest of Bluesky's requests. Connections that stay silent are checked
  with an HTTP/2 ping and closed if the server doesn't answer, so one dead
  connection can't stall every post to that server. Connecting to a server
  times out after 10 seconds (30 before, for some requests).
- The web app's tabs answer as soon as they show: one picked while the
  app is still reading the server opens once it has (the click used to
  be lost).
- The composer sends each value its cards hold, an empty one too, so a
  default cleared on a post (a content warning, a visibility) isn't
  filled in again by the account's defaults.
- An alert's `generatorURL` is this server's `PUBLIC_URL` (empty when
  unset); it was a fixed address.
- The composer's author is "You" until your accounts are read, and
  Tools shows the home relay of the account it syncs; both named this
  project's author.

## 2.1.0 (2026-11-06)

This changelog was started after 2.1.0; these are the changes of that
release (#36) that 3.0's notes would otherwise repeat.

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

### Added

- `ALLOWED_HOSTS`: the host names the server answers for (others get
  `421`). Recommended when OIDC is off.
