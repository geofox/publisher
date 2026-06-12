package mastodon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestPostVideoUploadsAndPolls verifies that a video Post uploads media via
// v2/media, polls /api/v1/media/:id (returning 206 twice then 200), then
// POSTs the status with the media_id attached.
func TestPostVideoUploadsAndPolls(t *testing.T) {
	pollCount := 0
	var statusPosted bool
	mux := http.NewServeMux()

	// v2/media: async upload returns 202 with id
	mux.HandleFunc("/api/v2/media", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "999"})
	})

	// polling endpoint: 206 x2, then 200
	mux.HandleFunc("/api/v1/media/999", func(w http.ResponseWriter, r *http.Request) {
		pollCount++
		if pollCount <= 2 {
			w.WriteHeader(http.StatusPartialContent)
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "999"})
	})

	mux.HandleFunc("/api/v1/statuses", func(w http.ResponseWriter, r *http.Request) {
		statusPosted = true
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "st1", "url": "https://m/@me/st1"})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	cl := New(srv.URL, "tok")
	cl.pollInterval = time.Millisecond // speed up test

	res, err := cl.Post(context.Background(), Post{
		Text:  "watch this",
		Video: &Video{Bytes: []byte("videodata"), Alt: "a clip"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !statusPosted {
		t.Error("status POST must be called after polling completes")
	}
	if pollCount < 3 {
		t.Errorf("expected at least 3 polls (2×206 + 1×200), got %d", pollCount)
	}
	if res.RemoteID != "st1" || res.RemoteURL != "https://m/@me/st1" {
		t.Errorf("unexpected result: %+v", res)
	}
}

// TestPostVideoMediaErrorReturnsError verifies that a non-206/non-200 status
// from the polling endpoint causes Post to return an error.
func TestPostVideoMediaErrorReturnsError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/media", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "err1"})
	})
	mux.HandleFunc("/api/v1/media/err1", func(w http.ResponseWriter, r *http.Request) {
		// 422 = processing failure
		w.WriteHeader(http.StatusUnprocessableEntity)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cl := New(srv.URL, "tok")
	cl.pollInterval = time.Millisecond
	if _, err := cl.Post(context.Background(), Post{
		Text:  "x",
		Video: &Video{Bytes: []byte("videodata")},
	}); err == nil {
		t.Error("expected error when polling returns 422")
	}
}
