package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	a := &API{Store: db}
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

func TestRequireAPIToken(t *testing.T) {
	db, _ := storeOpen(t)
	a := &API{Store: db}
	h := a.requireAPIToken(scopePostsWrite, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	_, raw, _ := db.CreateAPIToken("n8n")

	// No bearer → 401.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/publish", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: code=%d want 401", rec.Code)
	}
	// Valid bearer → 200.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/publish", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid token: code=%d want 200", rec.Code)
	}
	// The scheme is case-insensitive.
	rec = httptest.NewRecorder()
	req.Header.Set("Authorization", "bearer "+raw)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("lowercase scheme: code=%d want 200", rec.Code)
	}
	// A token without posts:write (or admin) is refused: scopes bind the
	// old machine routes too.
	for scopes, want := range map[string]int{"accounts:read": 403, "media:write": 403, "admin": 200} {
		_, raw, _ := db.CreateScopedToken("t", []string{scopes}, time.Time{})
		rec = httptest.NewRecorder()
		req.Header.Set("Authorization", "Bearer "+raw)
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("token with %s: code=%d want %d", scopes, rec.Code, want)
		}
	}
}

// TestMachineRoutesCheckScopes goes through the real gates: /publish wants
// posts:write and /upload-media media:write.
func TestMachineRoutesCheckScopes(t *testing.T) {
	e := newV1Env(t, true)
	media := e.token(scopeMediaWrite)
	for path, want := range map[string]int{"/publish": 403, "/upload-media": 400} {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
		r.Header.Set("Authorization", "Bearer "+media)
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, r)
		if rec.Code != want {
			t.Errorf("%s with a media:write token: %d, want %d", path, rec.Code, want)
		}
	}
}

func TestTokenCRUDHandlers(t *testing.T) {
	db, _ := storeOpen(t)
	a := &API{Store: db}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tokens", strings.NewReader(`{"name":"n8n"}`))
	a.handleCreateToken(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"token"`) {
		t.Fatalf("create: code=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	a.handleListTokens(rec, httptest.NewRequest(http.MethodGet, "/api/tokens", nil))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"token"`) {
		t.Fatalf("list leaked secret or failed: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "n8n") {
		t.Fatalf("list missing token name: %s", rec.Body.String())
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
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/posts", nil))
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("api/posts should be open when auth disabled")
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("auth/login should 404 when disabled, got %d", rec.Code)
	}
}

func TestRevokeUnknownTokenIs404(t *testing.T) {
	db, _ := storeOpen(t)
	a := &API{Store: db}
	req := httptest.NewRequest(http.MethodDelete, "/api/tokens/does-not-exist", nil)
	req.SetPathValue("id", "does-not-exist")
	rec := httptest.NewRecorder()
	a.handleRevokeToken(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("revoke unknown: code=%d want 404", rec.Code)
	}
}
