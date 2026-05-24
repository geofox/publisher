package store

import "regexp"

// keys whose values must never be archived
var secretKeyRe = regexp.MustCompile(`(?i)("(?:authorization|access_token|token|app_password|password|nsec|secret|access_jwt|refresh_jwt|accessJwt|refreshJwt)"\s*:\s*")(?:\\.|[^"\\])*(")`)

// also catch bare "Bearer xxxx" anywhere
var bearerRe = regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9._\-]+`)

func Scrub(s string) string {
	s = secretKeyRe.ReplaceAllString(s, `${1}REDACTED${2}`)
	s = bearerRe.ReplaceAllString(s, "Bearer REDACTED")
	return s
}
