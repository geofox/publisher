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
