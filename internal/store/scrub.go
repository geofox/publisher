package store

import "regexp"

// secretKeyRe matches a JSON member whose value must never be kept: the
// name, then the value redacted. Upstream error codes ("code") are kept.
var secretKeyRe = regexp.MustCompile(`(?i)("(?:authorization|access_token|token|app_password|password|current_password|nsec|secret|secret_key|client_secret|app_secret|code_verifier|refresh_token|id_token|access_jwt|refresh_jwt|accessJwt|refreshJwt)"\s*:\s*")(?:\\.|[^"\\])*(")`)

// formSecretRe matches a credential in a query string or form body
// (access_token=…), as a url.Error prints the URL it failed on.
var formSecretRe = regexp.MustCompile(`(?i)((?:^|[?&;\s"'(])(?:access_token|client_secret|app_secret|password|app_password|refresh_token|id_token|code_verifier|code|token|secret|key)=)[^&\s"'#)]*`)

// bearerRe catches a bare "Bearer xxxx" anywhere.
var bearerRe = regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9._~+/=\-]+`)

// nsecRe catches a bech32 Nostr secret key anywhere.
var nsecRe = regexp.MustCompile(`(?i)\bnsec1[02-9ac-hj-np-z]{6,}\b`)

// Scrub redacts credentials from text that is kept or shown: logs,
// events, attempts and error messages.
func Scrub(s string) string {
	s = secretKeyRe.ReplaceAllString(s, `${1}REDACTED${2}`)
	s = formSecretRe.ReplaceAllString(s, `${1}REDACTED`)
	s = bearerRe.ReplaceAllString(s, "Bearer REDACTED")
	s = nsecRe.ReplaceAllString(s, "nsec1REDACTED")
	return s
}
