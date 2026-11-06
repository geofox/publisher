package destination

import (
	"net/http"
	"testing"
	"time"
)

// Mastodon's 429 (rack-attack) carries X-RateLimit-Reset as an ISO 8601
// time and no Retry-After; the wait is lost.
func TestMastodonRateLimitResetParsed(t *testing.T) {
	h := http.Header{}
	h.Set("X-RateLimit-Reset", time.Now().Add(20*time.Minute).UTC().Format("2006-01-02T15:04:05.000000Z07:00"))
	e := Classify(&HTTPError{Op: "/api/v1/statuses", StatusCode: 429, Header: h})
	if e.Class != ClassRateLimited || e.RetryAfter < 19*time.Minute {
		t.Errorf("class %q RetryAfter %s, want rate_limited ~20m", e.Class, e.RetryAfter)
	}
}
