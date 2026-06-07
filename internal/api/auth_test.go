package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

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
	t.Skip("enabled in Task 11 once /auth routes are wired behind the feature flag")
	mux := newAuthDisabledAPI(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("auth disabled: /auth/login code=%d want 404", rec.Code)
	}
}
