package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/store"
)

func TestAttentionCountEndpoint(t *testing.T) {
	// Same setup pattern as list_filter_test.go: real temp store + &API{Store: db}.
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mk := func(id, status string) {
		_ = db.SavePost(&store.Post{ID: id, CreatedAt: time.Now().UTC(),
			Platforms: []string{"bluesky"}, Source: "test", Status: status,
			Targets: []store.Target{{Platform: "bluesky", Status: status}}})
	}
	mk("c1", "failed")
	mk("c2", "partial")
	mk("c3", "success")

	a := &API{Store: db}
	req := httptest.NewRequest("GET", "/api/posts/attention/count", nil)
	w := httptest.NewRecorder()
	a.Routes().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	var body struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Count != 2 {
		t.Errorf("count = %d, want 2", body.Count)
	}
}

func TestListPostsQueryParams(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// seed: 2 success, 1 failed
	now := time.Now().UTC()
	_ = db.SavePost(&store.Post{
		ID: "ok1", CreatedAt: now, Platforms: []string{"nostr"}, Source: "web", Status: "success",
		MasterText: "success post one",
		Targets:    []store.Target{{Platform: "nostr", Status: "success"}},
	})
	_ = db.SavePost(&store.Post{
		ID: "ok2", CreatedAt: now, Platforms: []string{"nostr"}, Source: "web", Status: "success",
		MasterText: "success post two",
		Targets:    []store.Target{{Platform: "nostr", Status: "success"}},
	})
	_ = db.SavePost(&store.Post{
		ID: "fail1", CreatedAt: now, Platforms: []string{"nostr"}, Source: "web", Status: "failed",
		MasterText: "failed post",
		Targets:    []store.Target{{Platform: "nostr", Status: "failed"}},
	})

	a := &API{Store: db}
	mux := a.Routes()

	t.Run("status=sent returns only success posts", func(t *testing.T) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/posts?status=sent", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status=sent: code=%d", rec.Code)
		}
		var posts []store.Post
		if err := json.NewDecoder(rec.Body).Decode(&posts); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(posts) != 2 {
			t.Errorf("status=sent: got %d posts, want 2", len(posts))
		}
		for _, p := range posts {
			if p.Status != "success" {
				t.Errorf("status=sent: got post with status %q", p.Status)
			}
		}
	})

	t.Run("limit=1 returns 1 post", func(t *testing.T) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/posts?limit=1", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("limit=1: code=%d", rec.Code)
		}
		var posts []store.Post
		if err := json.NewDecoder(rec.Body).Decode(&posts); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(posts) != 1 {
			t.Errorf("limit=1: got %d posts, want 1", len(posts))
		}
	})

	t.Run("bogus params return 200 with defaults", func(t *testing.T) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/posts?limit=abc&status=bogus", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("bogus params: code=%d, want 200", rec.Code)
		}
		var posts []store.Post
		if err := json.NewDecoder(rec.Body).Decode(&posts); err != nil {
			t.Fatalf("decode: %v", err)
		}
		// bogus status = no filter → all 3 posts returned
		if len(posts) != 3 {
			t.Errorf("bogus params: got %d posts, want 3 (default no-filter)", len(posts))
		}
	})
}
