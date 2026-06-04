package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/progress"
	"github.com/geofox/publisher/internal/store"
)

func TestProgressSSEStreamsInFlight(t *testing.T) {
	reg := progress.NewRegistry()
	h := reg.Create("p1", []string{"bluesky"}, "")
	a := &API{Progress: reg}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/posts/p1/progress", nil)
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)

	go func() {
		h.Platform("bluesky", progress.StatusSuccess, "posted", "https://x")
		reg.Finish("p1", progress.StatusSuccess, 5*time.Second) // retain so Get still finds hub
		cancel()
	}()
	a.Routes().ServeHTTP(rr, req)

	if rr.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("missing SSE content type: %q", rr.Header().Get("Content-Type"))
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"status":"success"`) || !strings.Contains(body, "data:") {
		t.Fatalf("stream missing terminal snapshot: %q", body)
	}
}

func TestProgressSSEUnknownID404(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	a := &API{Progress: progress.NewRegistry(), Store: db}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/posts/nope/progress", nil)
	a.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}

func TestProgressSSEReplaysFinishedFromStore(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	_ = db.SavePost(&store.Post{
		ID:        "p2",
		Platforms: []string{"bluesky", "nostr"},
		Status:    "success",
		Targets: []store.Target{
			{Platform: "bluesky", Status: "success", RemoteURL: "https://bsky.app/p2"},
			{Platform: "nostr", Status: "success"},
		},
	})

	a := &API{Progress: progress.NewRegistry(), Store: db}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/posts/p2/progress", nil)
	a.Routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("missing SSE content type: %q", rr.Header().Get("Content-Type"))
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"post_id":"p2"`) {
		t.Fatalf("replay missing post_id: %q", body)
	}
	if !strings.Contains(body, "data:") {
		t.Fatalf("replay missing SSE data prefix: %q", body)
	}
}
