package health

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func ok(name string, critical bool) Check {
	return Check{Name: name, Critical: critical, Run: func(context.Context) error { return nil }}
}

func failing(name string, critical bool, err error) Check {
	return Check{Name: name, Critical: critical, Run: func(context.Context) error { return err }}
}

func TestOverallStatus(t *testing.T) {
	secret := errors.New("open /data/publisher.db: read-only file system")
	cases := []struct {
		name   string
		checks []Check
		want   string
	}{
		{"all ok", []Check{ok("a", true), ok("b", false)}, StatusOK},
		{"non-critical fails", []Check{ok("a", true), failing("b", false, secret)}, StatusDegraded},
		{"critical fails", []Check{failing("a", true, secret), ok("b", false)}, StatusUnavailable},
		{"both fail", []Check{failing("a", true, secret), failing("b", false, secret)}, StatusUnavailable},
		{"no checks", nil, StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := (&Checker{Local: tc.checks}).Run(context.Background(), false)
			if rep.Status != tc.want {
				t.Fatalf("status = %q, want %q (%+v)", rep.Status, tc.want, rep.Checks)
			}
			for name, r := range rep.Checks {
				if strings.Contains(r.Error, "/data") {
					t.Errorf("%s: raw error leaked into the report: %q", name, r.Error)
				}
			}
		})
	}
}

func TestErrorClasses(t *testing.T) {
	c := &Checker{Local: []Check{
		failing("classed", false, Fail("schema mismatch", errors.New("v1 vs v2"))),
		failing("plain", false, errors.New("boom")),
		{Name: "slow", Timeout: 50 * time.Millisecond, Run: func(ctx context.Context) error {
			time.Sleep(5 * time.Second) // ignores ctx on purpose
			return nil
		}},
		{Name: "panics", Run: func(context.Context) error { panic("nope") }},
	}}
	start := time.Now()
	rep := c.Run(context.Background(), false)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Run took %v; a check ignoring its context must not hold up the report", d)
	}
	want := map[string]string{"classed": "schema mismatch", "plain": "error", "slow": "timeout", "panics": "error"}
	for name, class := range want {
		if got := rep.Checks[name]; got.Status != CheckFail || got.Error != class {
			t.Errorf("%s = %+v, want fail/%s", name, got, class)
		}
	}
}

func TestExternalOnlyOnRequestAndCached(t *testing.T) {
	var calls atomic.Int32
	now := time.Unix(1000, 0)
	var mu sync.Mutex
	c := &Checker{
		Local: []Check{ok("database", true)},
		External: []Check{{Name: "blossom", Run: func(context.Context) error {
			calls.Add(1)
			time.Sleep(20 * time.Millisecond)
			return errors.New("down")
		}}},
		ExternalTTL: 30 * time.Second,
		now:         func() time.Time { mu.Lock(); defer mu.Unlock(); return now },
	}

	rep := c.Run(context.Background(), false)
	if _, present := rep.Checks["blossom"]; present || calls.Load() != 0 {
		t.Fatalf("external check ran without being requested: %+v", rep)
	}

	// Concurrent callers share one probe.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rep := c.Run(context.Background(), true)
			if rep.Status != StatusDegraded || rep.Checks["blossom"].Status != CheckFail {
				t.Errorf("rep = %+v; a failing external check must degrade, not fail, readiness", rep)
			}
		}()
	}
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Fatalf("external probe ran %d times for concurrent callers, want 1", n)
	}

	c.Run(context.Background(), true)
	if n := calls.Load(); n != 1 {
		t.Fatalf("cached result not reused: %d probes", n)
	}
	mu.Lock()
	now = now.Add(31 * time.Second)
	mu.Unlock()
	c.Run(context.Background(), true)
	if n := calls.Load(); n != 2 {
		t.Fatalf("stale cache not refreshed: %d probes", n)
	}
}

type fakeStore struct {
	pingErr error
	version int
}

func (f fakeStore) Ping(context.Context) error                 { return f.pingErr }
func (f fakeStore) SchemaVersion(context.Context) (int, error) { return f.version, nil }

func TestDatabaseCheck(t *testing.T) {
	cases := []struct {
		s    fakeStore
		want string
	}{
		{fakeStore{version: 3}, ""},
		{fakeStore{version: 2}, "schema mismatch"},
		{fakeStore{pingErr: errors.New("disk I/O error"), version: 3}, "unreachable"},
	}
	for _, tc := range cases {
		chk := Database(tc.s, 3)
		if !chk.Critical {
			t.Fatal("database check must be critical")
		}
		rep := (&Checker{Local: []Check{chk}}).Run(context.Background(), false)
		if got := rep.Checks["database"].Error; got != tc.want {
			t.Errorf("%+v: error class = %q, want %q", tc.s, got, tc.want)
		}
	}
}

func TestWritableDirAndDiskSpace(t *testing.T) {
	dir := t.TempDir()
	c := &Checker{Local: []Check{
		WritableDir("w_ok", dir),
		WritableDir("w_missing", filepath.Join(dir, "nope")),
		DiskSpace("d_ok", dir, 0),
		DiskSpace("d_low", dir, 1<<62),
	}}
	rep := c.Run(context.Background(), false)
	want := map[string]string{"w_ok": "", "w_missing": "not writable", "d_ok": "", "d_low": "low space"}
	for name, class := range want {
		if got := rep.Checks[name].Error; got != class {
			t.Errorf("%s: error = %q, want %q", name, got, class)
		}
	}
	if m, _ := filepath.Glob(filepath.Join(dir, ".readyz-*")); len(m) != 0 {
		t.Errorf("writable check left files behind: %v", m)
	}
}

func TestHTTPReachable(t *testing.T) {
	var gotAccept, gotMethod atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept.Store(r.Header.Get("Accept"))
		gotMethod.Store(r.Method)
		if r.Header.Get("Authorization") != "" {
			t.Error("readiness probes must not send credentials")
		}
		switch r.URL.Path {
		case "/ok":
			w.WriteHeader(200)
		case "/notfound":
			w.WriteHeader(404)
		case "/broken":
			w.WriteHeader(503)
		case "/nohead":
			if r.Method == http.MethodHead {
				w.WriteHeader(405)
				return
			}
			w.WriteHeader(200)
		}
	}))
	defer srv.Close()
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()

	c := &Checker{Local: []Check{
		HTTPReachable("ok", srv.Client(), http.MethodGet, srv.URL+"/ok", nil),
		HTTPReachable("notfound", srv.Client(), http.MethodGet, srv.URL+"/notfound", nil),
		HTTPReachable("broken", srv.Client(), http.MethodGet, srv.URL+"/broken", nil),
		HTTPReachable("nohead", srv.Client(), http.MethodHead, srv.URL+"/nohead", nil),
		HTTPReachable("down", nil, http.MethodGet, closedURL, nil),
	}}
	rep := c.Run(context.Background(), false)
	want := map[string]string{"ok": "", "notfound": "", "broken": "http 503", "nohead": "", "down": "unreachable"}
	for name, class := range want {
		if got := rep.Checks[name].Error; got != class {
			t.Errorf("%s: error = %q, want %q", name, got, class)
		}
	}

	relay := NostrRelay("relay", srv.Client(), strings.Replace(srv.URL, "http://", "ws://", 1)+"/ok")
	rep = (&Checker{Local: []Check{relay}}).Run(context.Background(), false)
	if rep.Checks["relay"].Status != CheckOK {
		t.Fatalf("relay check = %+v", rep.Checks["relay"])
	}
	if gotAccept.Load() != "application/nostr+json" || gotMethod.Load() != http.MethodGet {
		t.Errorf("NIP-11 probe sent %v %v", gotMethod.Load(), gotAccept.Load())
	}
}
