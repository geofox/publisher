package health

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"
)

// SchemaStore is the slice of *store.Store the database check needs.
type SchemaStore interface {
	Ping(ctx context.Context) error
	SchemaVersion(ctx context.Context) (int, error)
}

// Database checks that the database answers and is at the schema version this
// build expects. Critical: without it nothing can be persisted.
func Database(s SchemaStore, want int) Check {
	return Check{Name: "database", Critical: true, Run: func(ctx context.Context) error {
		if err := s.Ping(ctx); err != nil {
			return Fail("unreachable", err)
		}
		got, err := s.SchemaVersion(ctx)
		if err != nil {
			return Fail("query failed", err)
		}
		if got != want {
			return Fail("schema mismatch", fmt.Errorf("database at v%d, build expects v%d", got, want))
		}
		return nil
	}}
}

// MinFreeBytes is the free-space floor for the data and work directories.
const MinFreeBytes = 256 << 20 // 256 MiB

// DiskSpace checks that dir's filesystem has at least min bytes available.
// SQLite starts failing writes on a full disk long before the process dies.
// Not critical: reads still work, and the operator needs the UI to clean up.
// On platforms without statfs the check always passes.
func DiskSpace(name, dir string, min uint64) Check {
	return Check{Name: name, Run: func(context.Context) error {
		free, err := freeBytes(dir)
		if errors.Is(err, errUnsupported) {
			return nil
		}
		if err != nil {
			return Fail("unreadable", err)
		}
		if free < min {
			return Fail("low space", fmt.Errorf("%s: %d bytes free, want ≥ %d", dir, free, min))
		}
		return nil
	}}
}

// WritableDir checks that a file can be created and removed in dir.
func WritableDir(name, dir string) Check {
	return Check{Name: name, Run: func(context.Context) error {
		f, err := os.CreateTemp(dir, ".readyz-*")
		if err != nil {
			return Fail("not writable", err)
		}
		n := f.Name()
		_ = f.Close()
		if err := os.Remove(n); err != nil {
			return Fail("not writable", err)
		}
		return nil
	}}
}

// HTTPReachable checks that url answers HTTP at all: any response below 500
// counts (401/404 mean the server is up; we never send credentials). method
// is usually GET; HEAD falls back to GET on 405/501.
func HTTPReachable(name string, client *http.Client, method, url string, header http.Header) Check {
	if client == nil {
		client = &http.Client{}
	}
	return Check{Name: name, Timeout: 5 * time.Second, Run: func(ctx context.Context) error {
		code, err := doRequest(ctx, client, method, url, header)
		if err == nil && method == http.MethodHead && (code == http.StatusMethodNotAllowed || code == http.StatusNotImplemented) {
			code, err = doRequest(ctx, client, http.MethodGet, url, header)
		}
		if err != nil {
			if ctx.Err() != nil {
				return Fail("timeout", err)
			}
			return Fail("unreachable", err)
		}
		if code >= 500 {
			return Fail(fmt.Sprintf("http %d", code), nil)
		}
		return nil
	}}
}

func doRequest(ctx context.Context, client *http.Client, method, url string, header http.Header) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return 0, err
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("User-Agent", "publisher-readyz")
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

var errUnsupported = errors.New("free-space check unsupported on this platform")
