package api

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/dispatch"
	"github.com/geofox/publisher/internal/store"
)

type schedFakeDispatch struct{ scheduled bool }

func (s *schedFakeDispatch) Post(ctx context.Context, spec dispatch.PostSpec) *store.Post {
	return &store.Post{ID: "imm", Status: "success", Platforms: spec.Platforms}
}
func (s *schedFakeDispatch) Retry(ctx context.Context, id string, platforms []string) (*store.Post, error) {
	return &store.Post{ID: id, Status: "success"}, nil
}
func (s *schedFakeDispatch) RetryRelay(ctx context.Context, id, relay string) (*store.Post, error) {
	return &store.Post{ID: id, Status: "success"}, nil
}
func (s *schedFakeDispatch) Schedule(ctx context.Context, spec dispatch.PostSpec, at time.Time) (*store.Post, error) {
	s.scheduled = true
	return &store.Post{ID: "sch", Status: "scheduled", ScheduledAt: &at, Platforms: spec.Platforms}, nil
}

func postSpecReq(a *API, spec map[string]any) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	b, _ := json.Marshal(spec)
	_ = mw.WriteField("spec", string(b))
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/post", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)
	return rec
}

func doJSON(a *API, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)
	return rec
}

func TestAPISchedulesWhenFuture(t *testing.T) {
	fd := &schedFakeDispatch{}
	a := &API{Dispatch: fd}
	at := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	rec := postSpecReq(a, map[string]any{"master_text": "hi", "platforms": []string{"nostr"}, "scheduled_at": at})
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if !fd.scheduled {
		t.Error("Schedule was not called")
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["status"] != "scheduled" {
		t.Errorf("resp = %v", resp)
	}
}

func TestAPIImmediateWhenNoTime(t *testing.T) {
	fd := &schedFakeDispatch{}
	a := &API{Dispatch: fd}
	rec := postSpecReq(a, map[string]any{"master_text": "hi", "platforms": []string{"nostr"}})
	if rec.Code != 200 || fd.scheduled {
		t.Fatalf("immediate expected: code=%d scheduled=%v", rec.Code, fd.scheduled)
	}
}

func TestAPIRejectsPastTime(t *testing.T) {
	a := &API{Dispatch: &schedFakeDispatch{}}
	at := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	rec := postSpecReq(a, map[string]any{"master_text": "hi", "platforms": []string{"nostr"}, "scheduled_at": at})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("past time code=%d, want 400", rec.Code)
	}
}

func TestCancelAndReschedule(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	a := &API{Store: db, Dispatch: &schedFakeDispatch{}}
	at := time.Now().Add(time.Hour).UTC()
	pending := &store.Post{
		ID: "p", CreatedAt: time.Now().UTC(), Platforms: []string{"nostr"}, Source: "web",
		Status: "scheduled", ScheduledAt: &at,
		Targets: []store.Target{{Platform: "nostr", Status: "scheduled", FieldsJSON: "{}"}},
	}
	if err := db.SavePost(pending); err != nil {
		t.Fatal(err)
	}

	future := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	futureStr := future.Format(time.RFC3339)
	r := doJSON(a, http.MethodPost, "/api/posts/p/reschedule", `{"scheduled_at":"`+futureStr+`"}`)
	if r.Code != 200 {
		t.Fatalf("reschedule code=%d body=%s", r.Code, r.Body.String())
	}
	var resp struct {
		ScheduledAt time.Time `json:"scheduled_at"`
	}
	_ = json.Unmarshal(r.Body.Bytes(), &resp)
	if !resp.ScheduledAt.Equal(future) {
		t.Errorf("reschedule body scheduled_at = %v, want %v", resp.ScheduledAt, future)
	}
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	if r := doJSON(a, http.MethodPost, "/api/posts/p/reschedule", `{"scheduled_at":"`+past+`"}`); r.Code != http.StatusBadRequest {
		t.Errorf("past reschedule code=%d, want 400", r.Code)
	}
	if r := doJSON(a, http.MethodPost, "/api/posts/p/cancel", ""); r.Code != 200 {
		t.Errorf("cancel code=%d", r.Code)
	}
	if r := doJSON(a, http.MethodPost, "/api/posts/p/cancel", ""); r.Code != http.StatusConflict {
		t.Errorf("re-cancel code=%d, want 409", r.Code)
	}
	// Reschedule after cancel → the post is gone → 409.
	if r := doJSON(a, http.MethodPost, "/api/posts/p/reschedule", `{"scheduled_at":"`+futureStr+`"}`); r.Code != http.StatusConflict {
		t.Errorf("reschedule-after-cancel code=%d, want 409", r.Code)
	}
}

func TestHideEndpoint(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	a := &API{Store: db, Dispatch: &schedFakeDispatch{}}

	// terminal post → hideable (200)
	done := &store.Post{ID: "d", CreatedAt: time.Now().UTC(), Platforms: []string{"nostr"}, Source: "web",
		Status: "failed", Targets: []store.Target{{Platform: "nostr", Status: "failed"}}}
	if err := db.SavePost(done); err != nil {
		t.Fatal(err)
	}
	if r := doJSON(a, http.MethodPost, "/api/posts/d/hide", ""); r.Code != 200 {
		t.Fatalf("hide terminal code=%d body=%s", r.Code, r.Body.String())
	}
	// pending post → 409
	at := time.Now().Add(time.Hour).UTC()
	pend := &store.Post{ID: "p", CreatedAt: time.Now().UTC(), Platforms: []string{"nostr"}, Source: "web",
		Status: "scheduled", ScheduledAt: &at, Targets: []store.Target{{Platform: "nostr", Status: "scheduled"}}}
	if err := db.SavePost(pend); err != nil {
		t.Fatal(err)
	}
	if r := doJSON(a, http.MethodPost, "/api/posts/p/hide", ""); r.Code != http.StatusConflict {
		t.Errorf("hide pending code=%d, want 409", r.Code)
	}
	// missing post → 409
	if r := doJSON(a, http.MethodPost, "/api/posts/nope/hide", ""); r.Code != http.StatusConflict {
		t.Errorf("hide missing code=%d, want 409", r.Code)
	}
}
