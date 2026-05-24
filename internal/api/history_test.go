package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/geofox/publisher/internal/store"
)

func TestHistoryEndpoints(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	_ = db.SavePost(&store.Post{ID: "p1", Platforms: []string{"nostr"}, Status: "success",
		Targets: []store.Target{{Platform: "nostr", Status: "success"}}})

	a := &API{Store: db}
	// list
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/posts", nil))
	if rec.Code != 200 {
		t.Fatalf("list code %d", rec.Code)
	}
	// detail
	rec = httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/posts/p1", nil))
	if rec.Code != 200 {
		t.Fatalf("detail code %d body %s", rec.Code, rec.Body.String())
	}
}

func TestGetPostNotFound(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	a := &API{Store: db}
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/posts/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing id: code = %d, want 404", rec.Code)
	}
}
