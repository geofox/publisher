package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/dispatch"
	"github.com/geofox/publisher/internal/store"
)

type fakeInteractDispatcher struct{ got dispatch.InteractSpec }

func (f *fakeInteractDispatcher) Post(context.Context, dispatch.PostSpec) *store.Post { return nil }
func (f *fakeInteractDispatcher) Retry(context.Context, string, []string) (*store.Post, error) {
	return nil, nil
}
func (f *fakeInteractDispatcher) RetryRelay(context.Context, string, string) (*store.Post, error) {
	return nil, nil
}
func (f *fakeInteractDispatcher) Schedule(context.Context, dispatch.PostSpec, time.Time) (*store.Post, error) {
	return nil, nil
}
func (f *fakeInteractDispatcher) Interact(_ context.Context, spec dispatch.InteractSpec) *store.Post {
	f.got = spec
	return &store.Post{ID: "x1", Status: "success", Interaction: &store.Interaction{Action: spec.Action},
		Targets: []store.Target{{Platform: spec.SourcePlatform, Status: "success", RemoteURL: "u"}}}
}

func TestAPIInteractForwardsSpec(t *testing.T) {
	fd := &fakeInteractDispatcher{}
	a := &API{Dispatch: fd}
	body, _ := json.Marshal(map[string]any{
		"action": "quote", "platform": "bluesky",
		"ref":           map[string]any{"uri": "at://x", "cid": "cidx"},
		"source_url":    "https://bsky.app/x", "source_author": "@a",
		"text":          "hi", "fanout": []string{"mastodon"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/interact", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if fd.got.Action != "quote" || fd.got.SourcePlatform != "bluesky" || fd.got.Ref.URI != "at://x" {
		t.Fatalf("spec not forwarded: %+v", fd.got)
	}
	if len(fd.got.Fanout) != 1 || fd.got.Fanout[0] != "mastodon" {
		t.Errorf("fanout not forwarded: %+v", fd.got.Fanout)
	}
}

func TestAPIInteractRejectsBadAction(t *testing.T) {
	a := &API{Dispatch: &fakeInteractDispatcher{}}
	body, _ := json.Marshal(map[string]any{"action": "bogus", "platform": "bluesky"})
	req := httptest.NewRequest(http.MethodPost, "/api/interact", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
}
