package api

import (
	"strings"
	"testing"
)

// ?status= takes any number of values; each becomes a SQL variable, and
// past SQLite's limit (32766) the list answers 500 instead of 400.
func TestListPostsManyStatuses(t *testing.T) {
	e := newV1Env(t, true)
	s := strings.TrimSuffix(strings.Repeat("failed,", 33000), ",")
	r := e.do(req{method: "GET", path: "/v1/posts?status=" + s, badRequest: true})
	if r.Code >= 500 {
		t.Fatalf("GET /v1/posts with 33000 statuses: %d %s", r.Code, r.Body.String())
	}
}
