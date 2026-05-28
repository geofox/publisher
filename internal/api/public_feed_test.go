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

func feedTestStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, 5, 20, 9, 0, 0, 0, time.UTC)
	if err := db.SavePost(&store.Post{
		ID: "p1", Platforms: []string{"nostr"}, Status: "success", MasterText: "hello",
		Targets: []store.Target{{Platform: "nostr", Status: "success", RemoteURL: "https://njump.me/x",
			Attempts: []store.Attempt{{AttemptNo: 1, Status: "success", AttemptedAt: ts}}}},
	}); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestPublicFeedDisabledWhenNoToken(t *testing.T) {
	a := &API{Store: feedTestStore(t)} // PublicFeedToken == ""
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/public/feed", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404 when token unset", rec.Code)
	}
}

func TestPublicFeedRejectsBadToken(t *testing.T) {
	a := &API{Store: feedTestStore(t), PublicFeedToken: "secret"}
	for _, h := range []string{"", "Bearer wrong", "secret"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/public/feed", nil)
		if h != "" {
			req.Header.Set("Authorization", h)
		}
		a.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("auth %q: code = %d, want 401", h, rec.Code)
		}
	}
}

func TestPublicFeedReturnsItems(t *testing.T) {
	a := &API{Store: feedTestStore(t), PublicFeedToken: "secret"}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/public/feed", nil)
	req.Header.Set("Authorization", "Bearer secret")
	a.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Version int `json:"version"`
		Posts   []struct {
			ID    string `json:"id"`
			Text  string `json:"text"`
			Links []struct {
				Platform string `json:"platform"`
				URL      string `json:"url"`
			} `json:"links"`
		} `json:"posts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Version != 1 || len(resp.Posts) != 1 || resp.Posts[0].ID != "p1" {
		t.Fatalf("resp = %+v, want version 1 + one post p1", resp)
	}
	if len(resp.Posts[0].Links) != 1 || resp.Posts[0].Links[0].URL != "https://njump.me/x" {
		t.Fatalf("links = %+v, want one nostr njump link", resp.Posts[0].Links)
	}
}
