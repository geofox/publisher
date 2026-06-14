package threads

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRefreshToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("grant_type") != "th_refresh_token" || r.URL.Query().Get("access_token") != "old" {
			t.Errorf("bad query: %s", r.URL.RawQuery)
		}
		w.Write([]byte(`{"access_token":"new-token","token_type":"bearer","expires_in":5183944}`))
	}))
	defer srv.Close()

	c := New("old", "")
	c.RefreshURL = srv.URL
	tok, ttl, err := c.RefreshToken(context.Background(), "old")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "new-token" {
		t.Errorf("token = %q", tok)
	}
	if ttl < 59*24*time.Hour {
		t.Errorf("ttl = %s, want ~60d", ttl)
	}
}

func TestRefreshTokenError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"bad token"}}`))
	}))
	defer srv.Close()
	c := New("old", "")
	c.RefreshURL = srv.URL
	if _, _, err := c.RefreshToken(context.Background(), "old"); err == nil {
		t.Error("expected error on non-200")
	}
}

func TestRefreshTokenErrorRedactsToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		// Simulate the API echoing the token back in the error body.
		w.Write([]byte(`{"error":"invalid request: access_token=SECRET%2BTOKEN%2FWITH%3DCHARS"}`))
	}))
	defer srv.Close()
	c := New("SECRET+TOKEN/WITH=CHARS", "")
	c.RefreshURL = srv.URL
	_, _, err := c.RefreshToken(context.Background(), "SECRET+TOKEN/WITH=CHARS")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "SECRET+TOKEN/WITH=CHARS") || strings.Contains(err.Error(), "SECRET%2BTOKEN%2FWITH%3DCHARS") {
		t.Errorf("token leaked into error: %v", err)
	}
}

func TestRefreshTokenTransportErrorRedactsToken(t *testing.T) {
	c := New("SECRET+TOKEN/WITH=CHARS", "")
	// Point at a closed port so HTTP.Do fails at the transport layer; the
	// resulting *url.Error embeds the full URL (token in query).
	c.RefreshURL = "http://127.0.0.1:1/refresh_access_token"
	_, _, err := c.RefreshToken(context.Background(), "SECRET+TOKEN/WITH=CHARS")
	if err == nil {
		t.Fatal("expected transport error")
	}
	if strings.Contains(err.Error(), "SECRET+TOKEN/WITH=CHARS") || strings.Contains(err.Error(), "SECRET%2BTOKEN%2FWITH%3DCHARS") {
		t.Errorf("token leaked into transport error: %v", err)
	}
}

func TestRefreshTokenZeroExpiresIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"access_token":"t","expires_in":0}`))
	}))
	defer srv.Close()
	c := New("old", "")
	c.RefreshURL = srv.URL
	if _, _, err := c.RefreshToken(context.Background(), "old"); err == nil {
		t.Error("expected error on expires_in=0")
	}
}

func TestSetTokenReflectedInRequests(t *testing.T) {
	var mu sync.Mutex
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = r.Header.Get("Authorization")
		mu.Unlock()
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := New("orig", "")
	c.BaseURL = srv.URL
	c.SetToken("updated")
	var out map[string]any
	if err := c.do(context.Background(), http.MethodGet, "/x", &out); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := gotAuth
	mu.Unlock()
	if got != "Bearer updated" {
		t.Errorf("auth = %q, want Bearer updated", got)
	}
}
