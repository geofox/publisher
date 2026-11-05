package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/auth"
	"github.com/geofox/publisher/internal/health"
	"github.com/geofox/publisher/internal/store"
)

func readyz(t *testing.T, h http.Handler, method, target string) (int, health.Report, http.Header) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	var rep health.Report
	if method != http.MethodHead {
		if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
			t.Fatalf("%s %s: body %q: %v", method, target, rec.Body.String(), err)
		}
	}
	return rec.Code, rep, rec.Header()
}

func TestReadyzStatusCodes(t *testing.T) {
	db, err := storeOpen(t)
	if err != nil {
		t.Fatal(err)
	}
	broken := errors.New("blossom down")
	cases := []struct {
		name     string
		local    []health.Check
		wantCode int
		want     string
	}{
		{"real database ok", []health.Check{health.Database(db, store.LatestSchemaVersion())}, 200, health.StatusOK},
		{"schema mismatch", []health.Check{health.Database(db, store.LatestSchemaVersion()+1)}, 503, health.StatusUnavailable},
		{"non-critical failure", []health.Check{
			health.Database(db, store.LatestSchemaVersion()),
			{Name: "video_workdir", Run: func(context.Context) error { return broken }},
		}, 200, health.StatusDegraded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &API{Store: db, Ready: &health.Checker{Local: tc.local}}
			code, rep, hdr := readyz(t, a.Routes(), http.MethodGet, "/readyz")
			if code != tc.wantCode || rep.Status != tc.want {
				t.Fatalf("code=%d status=%q, want %d %q (%+v)", code, rep.Status, tc.wantCode, tc.want, rep)
			}
			if hdr.Get("Cache-Control") != "no-store" {
				t.Errorf("Cache-Control = %q", hdr.Get("Cache-Control"))
			}
			if code, _, _ := readyz(t, a.Routes(), http.MethodHead, "/readyz"); code != tc.wantCode {
				t.Errorf("HEAD code = %d, want %d", code, tc.wantCode)
			}
		})
	}
}

func TestReadyzExternalParam(t *testing.T) {
	ran := 0
	a := &API{Ready: &health.Checker{
		External: []health.Check{{Name: "bluesky", Run: func(context.Context) error { ran++; return nil }}},
	}}
	_, rep, _ := readyz(t, a.Routes(), http.MethodGet, "/readyz")
	if _, ok := rep.Checks["bluesky"]; ok || ran != 0 {
		t.Fatal("external checks ran without ?external=1")
	}
	_, rep, _ = readyz(t, a.Routes(), http.MethodGet, "/readyz?external=1")
	if rep.Checks["bluesky"].Status != health.CheckOK || ran != 1 {
		t.Fatalf("external check not run with ?external=1: %+v", rep)
	}
}

func TestReadyzOpenWithAuthEnabled(t *testing.T) {
	issuer := newMockOIDC(t)
	au, err := auth.New(context.Background(), auth.Config{
		Issuer: issuer, ClientID: "client-1", ClientSecret: "s",
		RedirectURL: "https://app/auth/callback",
	})
	if err != nil {
		t.Fatal(err)
	}
	db, err := storeOpen(t)
	if err != nil {
		t.Fatal(err)
	}
	a := &API{Store: db, Auth: au, Allowlist: auth.NewAllowlist([]string{"sub-1"}, nil), SessionTTL: time.Hour,
		Ready: &health.Checker{Local: []health.Check{health.Database(db, store.LatestSchemaVersion())}}}
	if code, rep, _ := readyz(t, a.Routes(), http.MethodGet, "/readyz"); code != 200 || rep.Status != health.StatusOK {
		t.Fatalf("/readyz with auth on: code=%d rep=%+v", code, rep)
	}
	// A path trick must not turn the open gate into a bypass for gated routes.
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz/../index.html", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/readyz/../index.html got past the session gate: %d", rec.Code)
	}
}
