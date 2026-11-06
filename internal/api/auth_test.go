package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/auth"
	"github.com/geofox/publisher/internal/store"
)

func storeOpen(t *testing.T) (*store.Store, error) {
	t.Helper()
	return store.Open(t.TempDir() + "/t.db")
}

func newAuthDisabledAPI(t *testing.T) http.Handler {
	t.Helper()
	db, err := storeOpen(t)
	if err != nil {
		t.Fatal(err)
	}
	a := &API{Store: db}
	return a.Routes()
}

func TestLoginDisabledReturns404(t *testing.T) {
	mux := newAuthDisabledAPI(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("auth disabled: /auth/login code=%d want 404", rec.Code)
	}
}

func TestRequireSessionPageVsXHR(t *testing.T) {
	db, _ := storeOpen(t)
	a := &API{Store: db, Mode: ModeOIDC, Allowlist: auth.NewAllowlist([]string{"sub-1"}, nil)}
	h := a.requireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Page request, no cookie → 302 to /auth/login.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept", "text/html")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/auth/login" {
		t.Fatalf("page: code=%d loc=%s", rec.Code, rec.Header().Get("Location"))
	}

	// XHR request, no cookie → 401 JSON.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/posts", nil)
	req.Header.Set("Accept", "application/json")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("xhr: code=%d want 401", rec.Code)
	}

	// Valid session cookie → 200.
	u, _ := db.UpsertUser("sub-1", "", "")
	raw, _ := db.CreateSession(u.ID, time.Hour)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/posts", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: raw})
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid session: code=%d want 200", rec.Code)
	}
}

func TestRoutesGatesOffWhenAuthDisabled(t *testing.T) {
	mux := newAuthDisabledAPI(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz code=%d", rec.Code)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/posts", nil))
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("/v1/posts should be open when auth disabled (the proxy gates)")
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("auth/login should 404 when disabled, got %d", rec.Code)
	}
}
