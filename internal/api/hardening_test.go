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

// TestCSRFGuardTrustsAppBaseURL covers the reverse-proxy case: the browser's
// Origin is the public host, but the proxy forwards an internal Host upstream,
// so r.Host never matches Origin. The guard must validate against the
// server-configured public origin (AppBaseURL) instead of the proxy-dependent
// Host — otherwise every same-origin POST (sign-out included) is wrongly blocked.
func TestCSRFGuardTrustsAppBaseURL(t *testing.T) {
	a := &API{AppBaseURL: "https://publisher.example"}

	// Same-origin POST as the browser sees it, but r.Host is the internal
	// upstream the proxy forwarded → must NOT be blocked.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://internal-upstream:8080/api/post", nil)
	req.Header.Set("Origin", "https://publisher.example")
	a.Routes().ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Error("proxied same-origin POST (Origin matches AppBaseURL) was blocked as cross-origin")
	}

	// A genuinely foreign Origin is still blocked even with AppBaseURL set.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "http://internal-upstream:8080/api/post", nil)
	req.Header.Set("Origin", "https://evil.example")
	a.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("foreign-origin POST with AppBaseURL set: code = %d, want 403", rec.Code)
	}
}
