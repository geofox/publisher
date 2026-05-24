package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/geofox/publisher/internal/relaysync"
	"github.com/geofox/publisher/internal/store"
)

type fakeSyncer struct {
	scanned, appliedDir string
	gotTargets          []relaysync.Target
}

func (f *fakeSyncer) Scan(ctx context.Context, targets []relaysync.Target) []relaysync.RelayDiff {
	f.scanned = "yes"
	f.gotTargets = targets
	return []relaysync.RelayDiff{{URL: "wss://t1", Group: "secondary", MissingAtHome: 2, Status: "ok"}}
}
func (f *fakeSyncer) Apply(ctx context.Context, targets []relaysync.Target, direction string) []relaysync.ApplyResult {
	f.appliedDir = direction
	f.gotTargets = targets
	return []relaysync.ApplyResult{{URL: "wss://t1", Direction: direction, Published: 2, Status: "ok"}}
}

func newSyncAPI(t *testing.T, sy Syncer) http.Handler {
	t.Helper()
	db, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	a := &API{Store: db, Sync: sy, HomeRelay: "wss://relay.geoffrey.one"}
	return a.Routes()
}

func TestSyncRelaysAddListRemove(t *testing.T) {
	mux := newSyncAPI(t, &fakeSyncer{})
	req := httptest.NewRequest(http.MethodPost, "/api/sync/relays", strings.NewReader(`{"url":"wss://extra.relay"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "extra.relay") {
		t.Fatalf("add: code=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/sync/relays", strings.NewReader(`{"url":"ws://x.onion"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("overlay add code=%d want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/sync/relays", strings.NewReader(`{"url":"wss://extra.relay"}`)))
	if rec.Code != 200 {
		t.Errorf("remove code=%d", rec.Code)
	}
}

func TestSyncScanAndApply(t *testing.T) {
	fs := &fakeSyncer{}
	mux := newSyncAPI(t, fs)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/sync/scan", nil))
	if rec.Code != 200 || fs.scanned != "yes" {
		t.Fatalf("scan: code=%d scanned=%q", rec.Code, fs.scanned)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/sync/apply", strings.NewReader(`{"direction":"pull"}`)))
	if rec.Code != 200 || fs.appliedDir != "pull" {
		t.Fatalf("apply: code=%d dir=%q", rec.Code, fs.appliedDir)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/sync/apply", strings.NewReader(`{"direction":"sideways"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad direction code=%d want 400", rec.Code)
	}
}

func TestSyncApplyRelaysFilter(t *testing.T) {
	fs := &fakeSyncer{}
	mux := newSyncAPI(t, fs)
	for _, u := range []string{"wss://t1", "wss://t2"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/sync/relays", strings.NewReader(`{"url":"`+u+`"}`)))
		if rec.Code != 200 {
			t.Fatalf("add %s: code=%d", u, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/sync/apply", strings.NewReader(`{"direction":"pull","relays":["wss://t1"]}`)))
	if rec.Code != 200 {
		t.Fatalf("apply: code=%d", rec.Code)
	}
	if len(fs.gotTargets) != 1 || fs.gotTargets[0].URL != "wss://t1" {
		t.Errorf("filtered targets = %v, want [wss://t1]", fs.gotTargets)
	}
}

func TestSyncRelayAddNormalizesTrailingSlash(t *testing.T) {
	mux := newSyncAPI(t, &fakeSyncer{})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/sync/relays", strings.NewReader(`{"url":"wss://nos.lol/"}`)))
	if rec.Code != 200 {
		t.Fatalf("add: code=%d", rec.Code)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sync/relays", nil))
	body := rec.Body.String()
	if strings.Contains(body, "nos.lol/") || !strings.Contains(body, "wss://nos.lol") {
		t.Errorf("trailing slash not normalized: %s", body)
	}
}

func TestSyncTargetsAndScanFilter(t *testing.T) {
	fs := &fakeSyncer{}
	mux := newSyncAPI(t, fs)
	for _, u := range []string{"wss://t1", "wss://t2"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/sync/relays", strings.NewReader(`{"url":"`+u+`"}`)))
		if rec.Code != 200 {
			t.Fatalf("add %s: code=%d", u, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sync/targets", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "wss://t1") || !strings.Contains(rec.Body.String(), "wss://t2") {
		t.Fatalf("targets: code=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/sync/scan", strings.NewReader(`{"relays":["wss://t1"]}`)))
	if rec.Code != 200 {
		t.Fatalf("scan: code=%d", rec.Code)
	}
	if len(fs.gotTargets) != 1 || fs.gotTargets[0].URL != "wss://t1" {
		t.Errorf("scan filtered targets = %v, want [wss://t1]", fs.gotTargets)
	}
}
