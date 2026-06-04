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

	"github.com/geofox/publisher/internal/dispatch"
	"github.com/geofox/publisher/internal/progress"
	"github.com/geofox/publisher/internal/store"
)

// idCapturingDispatcher satisfies the Dispatcher interface; PostWithID records
// the id it was called with, signals the onPost channel, and returns success.
type idCapturingDispatcher struct {
	onPost func()
}

func (d *idCapturingDispatcher) Post(_ context.Context, spec dispatch.PostSpec) *store.Post {
	return &store.Post{ID: "p-sync", Status: "success", Platforms: spec.Platforms}
}
func (d *idCapturingDispatcher) PostWithID(_ context.Context, id string, spec dispatch.PostSpec) *store.Post {
	rec := &store.Post{ID: id, Status: "success", Platforms: spec.Platforms}
	if d.onPost != nil {
		d.onPost()
	}
	return rec
}
func (d *idCapturingDispatcher) Retry(_ context.Context, _ string, _ []string) (*store.Post, error) {
	return nil, nil
}
func (d *idCapturingDispatcher) RetryRelay(_ context.Context, _, _ string) (*store.Post, error) {
	return nil, nil
}
func (d *idCapturingDispatcher) Schedule(_ context.Context, _ dispatch.PostSpec, _ time.Time) (*store.Post, error) {
	return nil, nil
}
func (d *idCapturingDispatcher) Interact(_ context.Context, _ dispatch.InteractSpec) *store.Post {
	return nil
}
func (d *idCapturingDispatcher) InteractWithID(_ context.Context, id string, _ dispatch.InteractSpec) *store.Post {
	return &store.Post{ID: id, Status: "success"}
}

func TestAPIPostReturnsPostIDImmediately(t *testing.T) {
	reg := progress.NewRegistry()
	done := make(chan struct{})
	disp := &idCapturingDispatcher{onPost: func() { close(done) }}
	a := &API{Dispatch: disp, Progress: reg}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	spec, _ := json.Marshal(map[string]any{
		"master_text": "hi", "platforms": []string{"bluesky"},
	})
	_ = mw.WriteField("spec", string(spec))
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/post", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	a.Routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}

	var out struct {
		PostID string `json:"post_id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.PostID == "" || out.Status != "running" {
		t.Fatalf("expected post_id+running, got %+v", out)
	}

	// Wait for the dispatch goroutine to run.
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("dispatch goroutine did not run within 1s")
	}
}
