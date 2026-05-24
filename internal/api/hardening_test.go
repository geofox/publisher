package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSecurityHeaders(t *testing.T) {
	a := &API{}
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://post.example/", nil))

	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if csp := rec.Header().Get("Content-Security-Policy"); csp == "" {
		t.Error("Content-Security-Policy header missing")
	}
}

func TestCSRFGuard(t *testing.T) {
	a := &API{}

	// Cross-origin state-changing request → blocked before routing.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://post.example/api/post", nil)
	req.Header.Set("Origin", "http://evil.example")
	a.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin POST: code = %d, want 403", rec.Code)
	}

	// Same-origin Origin → allowed past the guard (handler 400s on the empty
	// body; the point is it is NOT a 403).
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "http://post.example/api/post", nil)
	req.Header.Set("Origin", "http://post.example")
	a.Routes().ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Error("same-origin POST was blocked as cross-origin")
	}

	// No Origin header (server-to-server, e.g. n8n) → allowed past the guard.
	rec = httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://post.example/api/post", nil))
	if rec.Code == http.StatusForbidden {
		t.Error("origin-less POST was blocked as cross-origin")
	}

	// Safe method with a cross-origin Origin → never blocked.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "http://post.example/", nil)
	req.Header.Set("Origin", "http://evil.example")
	a.Routes().ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Error("safe GET was blocked by the CSRF guard")
	}
}
