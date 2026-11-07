package store

import "github.com/geofox/publisher/internal/redact"

// Scrub redacts credentials from text that is kept or shown: logs,
// events, attempts and error messages (redact.Scrub).
func Scrub(s string) string { return redact.Scrub(s) }
