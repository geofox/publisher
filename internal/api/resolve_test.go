package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/geofox/publisher/internal/resolve"
)

type fakeResolver struct {
	ref *resolve.SourceRef
	err error
}

func (f fakeResolver) Resolve(context.Context, string) (*resolve.SourceRef, error) {
	return f.ref, f.err
}

func TestAPIResolveReturnsSourceRef(t *testing.T) {
	a := &API{Resolve: fakeResolver{ref: &resolve.SourceRef{
		Platform: "bluesky",
		Preview:  resolve.Preview{AuthorName: "Alice", Text: "hi"},
		Caps:     resolve.Caps{Quote: resolve.Cap{Allowed: false, Reason: "disabled"}},
	}}}
	body, _ := json.Marshal(map[string]string{"input": "https://bsky.app/x"})
	req := httptest.NewRequest(http.MethodPost, "/api/resolve", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var out resolve.SourceRef
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Platform != "bluesky" || out.Caps.Quote.Allowed {
		t.Fatalf("unexpected: %s", rec.Body.String())
	}
}

func TestAPIResolveErrorIsClientError(t *testing.T) {
	a := &API{Resolve: fakeResolver{err: resolve.ErrThreadsUnsupported}}
	body, _ := json.Marshal(map[string]string{"input": "https://threads.net/x"})
	req := httptest.NewRequest(http.MethodPost, "/api/resolve", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
}
