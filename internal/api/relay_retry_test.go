package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/dispatch"
	"github.com/geofox/publisher/internal/store"
)

type relayFakeDispatch struct{ calledID, calledRelay string }

func (f *relayFakeDispatch) Post(ctx context.Context, spec dispatch.PostSpec) *store.Post {
	return &store.Post{ID: "x"}
}
func (f *relayFakeDispatch) Retry(ctx context.Context, id string, platforms []string) (*store.Post, error) {
	return &store.Post{ID: id}, nil
}
func (f *relayFakeDispatch) RetryRelay(ctx context.Context, id, relay string) (*store.Post, error) {
	f.calledID, f.calledRelay = id, relay
	return &store.Post{ID: id, Status: "success"}, nil
}
func (f *relayFakeDispatch) Schedule(ctx context.Context, spec dispatch.PostSpec, at time.Time) (*store.Post, error) {
	return &store.Post{ID: "sch", Status: "scheduled"}, nil
}

func TestRelayRetryEndpoint(t *testing.T) {
	fd := &relayFakeDispatch{}
	a := &API{Dispatch: fd}
	mux := a.Routes()

	body := strings.NewReader(`{"platform":"nostr","relay":"wss://relay.damus.io"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/posts/p1/relay-retry", body)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if fd.calledID != "p1" || fd.calledRelay != "wss://relay.damus.io" {
		t.Errorf("dispatch got id=%q relay=%q", fd.calledID, fd.calledRelay)
	}
}

func TestRelayRetryRejectsNonNostr(t *testing.T) {
	a := &API{Dispatch: &relayFakeDispatch{}}
	mux := a.Routes()
	req := httptest.NewRequest(http.MethodPost, "/api/posts/p1/relay-retry",
		strings.NewReader(`{"platform":"mastodon","relay":"x"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("non-nostr code = %d, want 400", rec.Code)
	}
}

func TestRelayRetryMissingRelay(t *testing.T) {
	a := &API{Dispatch: &relayFakeDispatch{}}
	mux := a.Routes()
	req := httptest.NewRequest(http.MethodPost, "/api/posts/p1/relay-retry",
		strings.NewReader(`{"platform":"nostr"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing relay code = %d, want 400", rec.Code)
	}
}

func TestRelayRetryInvalidJSON(t *testing.T) {
	a := &API{Dispatch: &relayFakeDispatch{}}
	mux := a.Routes()
	req := httptest.NewRequest(http.MethodPost, "/api/posts/p1/relay-retry",
		strings.NewReader(`not-json`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid json code = %d, want 400", rec.Code)
	}
}
