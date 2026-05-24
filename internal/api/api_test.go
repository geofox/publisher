package api

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"fiatjaf.com/nostr"

	"github.com/geofox/publisher/internal/dispatch"
	"github.com/geofox/publisher/internal/media"
	"github.com/geofox/publisher/internal/store"
)

type fakeDispatcher struct{}

func (fakeDispatcher) Post(ctx context.Context, spec dispatch.PostSpec) *store.Post {
	return &store.Post{ID: "p1", Status: "success", Platforms: spec.Platforms}
}

func (fakeDispatcher) Retry(ctx context.Context, id string, platforms []string) (*store.Post, error) {
	return &store.Post{ID: id, Status: "success"}, nil
}
func (fakeDispatcher) RetryRelay(ctx context.Context, id, relay string) (*store.Post, error) {
	return &store.Post{ID: id, Status: "success"}, nil
}
func (fakeDispatcher) Schedule(ctx context.Context, spec dispatch.PostSpec, at time.Time) (*store.Post, error) {
	return &store.Post{ID: "sch", Status: "scheduled"}, nil
}

func TestAPIPost(t *testing.T) {
	a := &API{Dispatch: fakeDispatcher{}}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	spec, _ := json.Marshal(map[string]any{
		"master_text": "hi", "platforms": []string{"nostr", "mastodon"},
	})
	_ = mw.WriteField("spec", string(spec))
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/post", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["post_id"] != "p1" {
		t.Errorf("resp = %v", resp)
	}
}

func TestAPIPostTooManyImages(t *testing.T) {
	a := &API{Dispatch: fakeDispatcher{}}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	spec, _ := json.Marshal(map[string]any{"master_text": "hi", "platforms": []string{"nostr"}})
	_ = mw.WriteField("spec", string(spec))
	for i := 0; i < 5; i++ {
		fw, _ := mw.CreateFormFile("image", "x.png")
		_, _ = fw.Write([]byte("img"))
	}
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/post", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("5 images: code = %d, want 400", rec.Code)
	}
}

func TestAPIPostMissingSpec(t *testing.T) {
	a := &API{Dispatch: fakeDispatcher{}}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("notspec", "{}")
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/post", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing spec: code = %d, want 400", rec.Code)
	}
}

func TestAPIPostMediaError(t *testing.T) {
	// Blossom stub that always 500s → media.Process fails → handler returns 502.
	blossom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer blossom.Close()
	sk := nostr.Generate()
	a := &API{Dispatch: fakeDispatcher{}}
	a.media = media.New(blossom.URL, sk, nostr.GetPublicKey(sk))

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	spec, _ := json.Marshal(map[string]any{"master_text": "hi", "platforms": []string{"nostr"}})
	_ = mw.WriteField("spec", string(spec))
	fw, _ := mw.CreateFormFile("image", "x.png")
	_, _ = fw.Write([]byte("img"))
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/post", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("media error: code = %d, want 502", rec.Code)
	}
}
