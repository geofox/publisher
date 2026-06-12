package threads

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"net/http"
	"net/http/httptest"
)

// TestPostVideoCreatesVideoContainer verifies that a video Post uses
// media_type=VIDEO with the correct video_url.
func TestPostVideoCreatesVideoContainer(t *testing.T) {
	var createQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("/me/threads", func(w http.ResponseWriter, r *http.Request) {
		createQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "vcre1"})
	})
	mux.HandleFunc("/vcre1", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "FINISHED"})
	})
	mux.HandleFunc("/me/threads_publish", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "vmed1"})
	})
	mux.HandleFunc("/vmed1", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"permalink": "https://www.threads.net/@me/post/vp1"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New("tok", "me")
	c.BaseURL = srv.URL
	c.PollInterval = time.Millisecond

	res, err := c.Post(context.Background(), Post{
		Text:  "watch this",
		Video: &Video{URL: "https://blossom/clip.mp4"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.RemoteID != "vmed1" {
		t.Errorf("RemoteID = %q, want vmed1", res.RemoteID)
	}
	if !strings.Contains(createQuery, "media_type=VIDEO") {
		t.Errorf("create query missing media_type=VIDEO: %q", createQuery)
	}
	if !strings.Contains(createQuery, "video_url=") {
		t.Errorf("create query missing video_url: %q", createQuery)
	}
}

// TestPostVideoAltTextSetOnContainer verifies that a non-empty Alt is forwarded
// as alt_text on the VIDEO container query.
func TestPostVideoAltTextSetOnContainer(t *testing.T) {
	var createQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("/me/threads", func(w http.ResponseWriter, r *http.Request) {
		createQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "valt1"})
	})
	mux.HandleFunc("/valt1", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "FINISHED"})
	})
	mux.HandleFunc("/me/threads_publish", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "vmed_alt"})
	})
	mux.HandleFunc("/vmed_alt", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"permalink": "https://t/p"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New("tok", "me")
	c.BaseURL = srv.URL
	c.PollInterval = time.Millisecond

	_, err := c.Post(context.Background(), Post{
		Text:  "watch",
		Video: &Video{URL: "https://blossom/clip.mp4", Alt: "a short clip"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(createQuery, "alt_text=a+short+clip") && !strings.Contains(createQuery, "alt_text=a%20short%20clip") {
		t.Fatalf("create query must contain alt_text: %q", createQuery)
	}
}

// TestPostVideoUsesVideoPollTimeout verifies that a video post uses
// VideoPollTimeout instead of PollTimeout. We set PollTimeout=0 so a text
// post would fail immediately; the video post must succeed with a generous
// VideoPollTimeout.
func TestPostVideoUsesVideoPollTimeout(t *testing.T) {
	pollCount := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/me/threads", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "vc2"})
	})
	mux.HandleFunc("/vc2", func(w http.ResponseWriter, r *http.Request) {
		pollCount++
		if pollCount < 2 {
			// IN_PROGRESS on first poll
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "IN_PROGRESS"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "FINISHED"})
	})
	mux.HandleFunc("/me/threads_publish", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "vmed2"})
	})
	mux.HandleFunc("/vmed2", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"permalink": "https://t/p"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New("tok", "me")
	c.BaseURL = srv.URL
	c.PollInterval = time.Millisecond
	c.PollTimeout = 0                    // text posts would time out immediately
	c.VideoPollTimeout = 5 * time.Second // generous for video

	_, err := c.Post(context.Background(), Post{
		Text:  "clip",
		Video: &Video{URL: "https://blossom/clip.mp4"},
	})
	if err != nil {
		t.Fatalf("video post must succeed with VideoPollTimeout: %v", err)
	}
}
