package unfurl

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newPageServer serves an OG page at /post, a thumb at /t.jpg, and counts
// page hits (for cache assertions).
func newPageServer(t *testing.T, hits *int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/post", func(w http.ResponseWriter, r *http.Request) {
		*hits++
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head>
			<meta property="og:title" content="T"/>
			<meta property="og:description" content="D"/>
			<meta property="og:image" content="/t.jpg"/>
		</head><body></body></html>`))
	})
	mux.HandleFunc("/t.jpg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("jpegbytes"))
	})
	mux.HandleFunc("/plain", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("not html"))
	})
	mux.HandleFunc("/notitle", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head></head><body></body></html>`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newTestService(srv *httptest.Server) *Service {
	return &Service{HTTP: srv.Client(), PLCDirectory: srv.URL, cache: map[string]cacheEntry{}}
}

func TestUnfurlBuildsCardAndCaches(t *testing.T) {
	hits := 0
	srv := newPageServer(t, &hits)
	s := newTestService(srv)
	c, err := s.Unfurl(context.Background(), srv.URL+"/post")
	if err != nil {
		t.Fatal(err)
	}
	if c.Title != "T" || c.Description != "D" {
		t.Fatalf("card: %+v", c)
	}
	if c.ThumbURL != srv.URL+"/t.jpg" || string(c.ThumbData) != "jpegbytes" || c.ThumbMime != "image/jpeg" {
		t.Fatalf("thumb: url=%q mime=%q len=%d", c.ThumbURL, c.ThumbMime, len(c.ThumbData))
	}
	if _, err := s.Unfurl(context.Background(), srv.URL+"/post"); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("expected 1 page fetch (cache hit on second), got %d", hits)
	}
}

func TestUnfurlErrors(t *testing.T) {
	hits := 0
	srv := newPageServer(t, &hits)
	s := newTestService(srv)
	if _, err := s.Unfurl(context.Background(), srv.URL+"/plain"); err == nil {
		t.Fatal("expected error for non-html content type")
	}
	if _, err := s.Unfurl(context.Background(), srv.URL+"/notitle"); err == nil {
		t.Fatal("expected error for page without any title")
	}
	if _, err := s.Unfurl(context.Background(), "ftp://example.com/x"); err == nil {
		t.Fatal("expected error for non-http scheme")
	}
}

func TestUnfurlNegativeCache(t *testing.T) {
	hits := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { hits++; http.Error(w, "nope", 500) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	s := newTestService(srv)
	_, _ = s.Unfurl(context.Background(), srv.URL+"/x")
	_, _ = s.Unfurl(context.Background(), srv.URL+"/x")
	if hits != 1 {
		t.Fatalf("expected failure to be negative-cached, got %d fetches", hits)
	}
}

func TestUnfurlSSRFGuardBlocksLoopback(t *testing.T) {
	s := New() // production client — SSRF-guarded
	s.HTTP.Timeout = 2 * time.Second
	_, err := s.Unfurl(context.Background(), "http://127.0.0.1:1/x")
	if err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("expected blocked non-public address error, got %v", err)
	}
}
