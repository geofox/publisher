# Changelog

Notable changes to Publisher. Releases are tagged `vX.Y.Z`; `main` publishes
the `edge` image.

## Unreleased

### Changed

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
