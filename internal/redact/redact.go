// Package redact keeps credentials out of text that is kept or shown:
// logs, events, attempts, alerts and error messages.
package redact

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// secretKeyRe matches a JSON member whose value must never be kept: the
// name, then the value redacted. Upstream error codes ("code") are kept.
var secretKeyRe = regexp.MustCompile(`(?i)("(?:authorization|access_token|token|app_password|password|current_password|nsec|secret|secret_key|client_secret|app_secret|code_verifier|refresh_token|id_token|access_jwt|refresh_jwt|accessJwt|refreshJwt|master_key)"\s*:\s*")(?:\\.|[^"\\])*(")`)

// formSecretRe matches a credential in a query string or form body
// (access_token=…), as a url.Error prints the URL it failed on.
var formSecretRe = regexp.MustCompile(`(?i)((?:^|[?&;\s"'(])(?:access_token|client_secret|app_secret|password|app_password|refresh_token|id_token|code_verifier|code|token|secret|secret_key|key)=)[^&\s"'#)]*`)

// bearerRe catches a bare "Bearer xxxx" anywhere.
var bearerRe = regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9._~+/=\-]+`)

// nsecRe catches a bech32 Nostr secret key anywhere.
var nsecRe = regexp.MustCompile(`(?i)\bnsec1[02-9ac-hj-np-z]{6,}\b`)

// appPasswordRe catches a Bluesky app password's shape (xxxx-xxxx-xxxx-xxxx,
// lowercase letters and digits), wherever an upstream echoes one.
var appPasswordRe = regexp.MustCompile(`\b[a-z0-9]{4}-[a-z0-9]{4}-[a-z0-9]{4}-[a-z0-9]{4}\b`)

// Scrub redacts credentials from s.
func Scrub(s string) string {
	s = secretKeyRe.ReplaceAllString(s, `${1}REDACTED${2}`)
	s = formSecretRe.ReplaceAllString(s, `${1}REDACTED`)
	s = bearerRe.ReplaceAllString(s, "Bearer REDACTED")
	s = nsecRe.ReplaceAllString(s, "nsec1REDACTED")
	s = appPasswordRe.ReplaceAllString(s, "REDACTED")
	return s
}

// MaxUpstream is how much of an upstream answer an error message keeps.
const MaxUpstream = 300

// Upstream is an upstream server's error body as an error message may
// carry it: one line, credentials redacted, at most MaxUpstream bytes, and
// nothing of an HTML page (a proxy's error page says nothing useful and
// may echo the request).
func Upstream(body string) string {
	b := strings.TrimSpace(body)
	if b == "" {
		return ""
	}
	if b[0] == '<' {
		return "(an HTML page)"
	}
	var sb strings.Builder
	space := false
	for _, r := range b {
		switch {
		case r == utf8.RuneError || unicode.IsControl(r) || unicode.IsSpace(r):
			space = true
		default:
			if space && sb.Len() > 0 {
				sb.WriteByte(' ')
			}
			space = false
			sb.WriteRune(r)
		}
	}
	s := Scrub(sb.String())
	if len(s) <= MaxUpstream {
		return s
	}
	cut := MaxUpstream
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
