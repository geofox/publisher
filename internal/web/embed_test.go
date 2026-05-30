package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServesIndexAndAssets(t *testing.T) {
	h := Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `id="compose"`) {
		t.Fatalf("index: code=%d has shell? %v", rec.Code, strings.Contains(rec.Body.String(), `id="compose"`))
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/main.js", nil))
	if rec.Code != 200 {
		t.Errorf("main.js: code=%d, want 200", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/compose.js", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "/api/post") {
		t.Errorf("compose.js: code=%d references api? %v", rec.Code, strings.Contains(rec.Body.String(), "/api/post"))
	}
	// PWA assets (install standalone on iOS): manifest + apple-touch-icon must serve.
	for _, p := range []string{"/manifest.json", "/icon-180.png", "/icon-192.png", "/icon-512.png"} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != 200 {
			t.Errorf("%s: code=%d, want 200", p, rec.Code)
		}
	}
	// Redesign (iOS Liquid Glass) assets: the shared brand helper module and the
	// extracted brand-mark PNGs used by target rows / previews must serve.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/brands.js", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "brandTile") {
		t.Errorf("brands.js: code=%d exports brandTile? %v", rec.Code, strings.Contains(rec.Body.String(), "brandTile"))
	}
	for _, p := range []string{"/marks/nostr-mark.png", "/marks/threads-mark.png"} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != 200 {
			t.Errorf("%s: code=%d, want 200", p, rec.Code)
		}
	}
	// The shell now ships the light theme + bottom glass tab bar.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `content="light"`) || !strings.Contains(body, `class="tabbar"`) {
		t.Errorf("index: light theme? %v; bottom tab bar? %v",
			strings.Contains(body, `content="light"`), strings.Contains(body, `class="tabbar"`))
	}
}
