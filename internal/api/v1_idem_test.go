package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/store"
)

func TestParseIdempotencyKey(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{`"abc"`, "abc", true},
		{` "a b" `, "a b", true},
		{`"a\"b\\c"`, `a"b\c`, true},
		{`abc-123_X.y`, "abc-123_X.y", true},
		{`""`, "", false},
		{`"abc`, "", false},
		{`"a"b"`, "", false},
		{`"a\nb"`, "", false},
		{"\"a\tb\"", "", false},
		{`"é"`, "", false},
		{`a b`, "", false},
		{`a,b`, "", false},
		{``, "", false},
		{`"` + strings.Repeat("k", 255) + `"`, strings.Repeat("k", 255), true},
		{`"` + strings.Repeat("k", 256) + `"`, "", false},
		{strings.Repeat("k", 256), "", false},
	}
	for _, c := range cases {
		got, ok := parseIdempotencyKey(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("parse(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// idemEnv wraps a counting create handler in v1Auth and withIdempotency,
// as mountV1 does for an Idempotent route.
type idemEnv struct {
	h     http.Handler
	runs  atomic.Int32
	st    *store.Store
	block chan struct{} // when set, the handler waits on it
	start chan struct{} // closed when a blocked handler starts
	reply func(w http.ResponseWriter, n int32)
}

func newIdemEnv(t *testing.T) *idemEnv {
	t.Helper()
	st, err := storeOpen(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e := &idemEnv{st: st}
	a := &API{Store: st}
	create := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := e.runs.Add(1)
		if e.block != nil {
			close(e.start)
			<-e.block
		}
		if e.reply != nil {
			e.reply(w, n)
			return
		}
		w.Header().Set("Location", fmt.Sprintf("/v1/things/%d", n))
		w.Header().Set("X-Not-Replayed", "1")
		writeV1(w, http.StatusCreated, map[string]any{"n": n})
	})
	mux := http.NewServeMux()
	mux.Handle("POST /v1/things", v1Recover(a.v1Auth(v1Gate{}, a.withIdempotency(nil, create))))
	mux.Handle("POST /v1/others", v1Recover(a.v1Auth(v1Gate{}, a.withIdempotency(nil, create))))
	e.h = mux
	return e
}

func (e *idemEnv) post(path, key, body, bearer string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	return rec
}

func TestIdempotencyReplay(t *testing.T) {
	e := newIdemEnv(t)
	first := e.post("/v1/things", `"k1"`, `{"a":1}`, "")
	if first.Code != 201 || first.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("first = %d %v", first.Code, first.Header())
	}
	again := e.post("/v1/things", `"k1"`, `{"a":1}`, "")
	if again.Code != 201 || again.Body.String() != first.Body.String() ||
		again.Header().Get("Location") != "/v1/things/1" || again.Header().Get("Idempotent-Replayed") != "true" ||
		again.Header().Get("Content-Type") != "application/json" || again.Header().Get("X-Not-Replayed") != "" {
		t.Fatalf("replay = %d %v %s", again.Code, again.Header(), again.Body.String())
	}
	if e.runs.Load() != 1 {
		t.Fatalf("handler ran %d times, want 1", e.runs.Load())
	}
	// The bare form names the same key.
	if r := e.post("/v1/things", `k1`, `{"a":1}`, ""); r.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("bare key did not replay: %d %v", r.Code, r.Header())
	}
	// Without a key, every request runs.
	e.post("/v1/things", "", `{"a":1}`, "")
	if e.runs.Load() != 2 {
		t.Fatalf("keyless request: runs = %d, want 2", e.runs.Load())
	}
}

func TestIdempotencyReuseIs422(t *testing.T) {
	e := newIdemEnv(t)
	e.post("/v1/things", `"k"`, `{"a":1}`, "")
	for name, r := range map[string]*httptest.ResponseRecorder{
		"other body": e.post("/v1/things", `"k"`, `{"a":2}`, ""),
		"other path": e.post("/v1/others", `"k"`, `{"a":1}`, ""),
	} {
		if r.Code != 422 || !strings.Contains(r.Body.String(), `"code":"idempotency_key_reused"`) {
			t.Errorf("%s: %d %s, want 422 idempotency_key_reused", name, r.Code, r.Body.String())
		}
	}
	if e.runs.Load() != 1 {
		t.Fatalf("runs = %d, want 1", e.runs.Load())
	}
}

func TestIdempotencyPerPrincipal(t *testing.T) {
	e := newIdemEnv(t)
	_, a, _ := e.st.CreateScopedToken("a", []string{scopeAdmin}, time.Time{})
	_, b, _ := e.st.CreateScopedToken("b", []string{scopeAdmin}, time.Time{})
	ra := e.post("/v1/things", `"same"`, `{}`, a)
	rb := e.post("/v1/things", `"same"`, `{}`, b)
	if ra.Code != 201 || rb.Code != 201 || rb.Header().Get("Idempotent-Replayed") != "" || e.runs.Load() != 2 {
		t.Fatalf("two callers, one key: %d %d runs=%d; each should run", ra.Code, rb.Code, e.runs.Load())
	}
}

func TestIdempotencyInFlightIs409(t *testing.T) {
	e := newIdemEnv(t)
	e.block, e.start = make(chan struct{}), make(chan struct{})
	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- e.post("/v1/things", `"k"`, `{}`, "") }()
	<-e.start
	r := e.post("/v1/things", `"k"`, `{}`, "")
	if r.Code != 409 || r.Header().Get("Retry-After") == "" || !strings.Contains(r.Body.String(), `"code":"idempotency_key_in_use"`) {
		t.Errorf("concurrent repeat = %d %v %s, want 409 idempotency_key_in_use", r.Code, r.Header(), r.Body.String())
	}
	close(e.block)
	if first := <-done; first.Code != 201 {
		t.Fatalf("first = %d", first.Code)
	}
	e.block = nil
	if r := e.post("/v1/things", `"k"`, `{}`, ""); r.Header().Get("Idempotent-Replayed") != "true" {
		t.Errorf("after the first finished: %d %v, want a replay", r.Code, r.Header())
	}
}

// TestIdempotencyRetryableNotStored: a 5xx, a 409 or a panic frees the
// key, so the client's retry with it runs again.
func TestIdempotencyRetryableNotStored(t *testing.T) {
	for _, mode := range []string{"500", "409", "503", "panic"} {
		t.Run(mode, func(t *testing.T) {
			e := newIdemEnv(t)
			e.reply = func(w http.ResponseWriter, n int32) {
				if n == 1 {
					switch mode {
					case "panic":
						panic("boom")
					case "409":
						w.WriteHeader(409)
					case "503":
						w.WriteHeader(503)
					default:
						w.WriteHeader(500)
					}
					return
				}
				w.WriteHeader(201)
			}
			e.post("/v1/things", `"k"`, `{}`, "")
			r := e.post("/v1/things", `"k"`, `{}`, "")
			if r.Code != 201 || r.Header().Get("Idempotent-Replayed") != "" || e.runs.Load() != 2 {
				t.Fatalf("retry after %s = %d replayed=%q runs=%d; want a fresh run", mode, r.Code,
					r.Header().Get("Idempotent-Replayed"), e.runs.Load())
			}
		})
	}
}

// TestIdempotencyStoresClientErrors: a 4xx other than 409/429 is the
// request's answer, replayed like a success.
func TestIdempotencyStoresClientErrors(t *testing.T) {
	e := newIdemEnv(t)
	e.reply = func(w http.ResponseWriter, _ int32) {
		problem(w, httptest.NewRequest("POST", "/v1/things", nil), 422, codeValidation, "no")
	}
	e.post("/v1/things", `"k"`, `{}`, "")
	r := e.post("/v1/things", `"k"`, `{}`, "")
	if r.Code != 422 || r.Header().Get("Idempotent-Replayed") != "true" ||
		r.Header().Get("Content-Type") != "application/problem+json" || e.runs.Load() != 1 {
		t.Fatalf("replayed 422 = %d %v runs=%d", r.Code, r.Header(), e.runs.Load())
	}
}

func TestIdempotencyBadKeyAndBody(t *testing.T) {
	e := newIdemEnv(t)
	for _, k := range []string{`""`, `"open`, `a b`} {
		if r := e.post("/v1/things", k, `{}`, ""); r.Code != 400 || !strings.Contains(r.Body.String(), `"code":"idempotency_key_invalid"`) {
			t.Errorf("key %q: %d %s", k, r.Code, r.Body.String())
		}
	}
	// Two Idempotency-Key fields are one too many.
	r := httptest.NewRequest("POST", "/v1/things", strings.NewReader(`{}`))
	r.Header.Add("Idempotency-Key", `"a"`)
	r.Header.Add("Idempotency-Key", `"b"`)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	if rec.Code != 400 {
		t.Errorf("two keys: %d", rec.Code)
	}
	if r := e.post("/v1/things", `"big"`, strings.Repeat("a", maxV1Body+1), ""); r.Code != 413 {
		t.Errorf("big body: %d", r.Code)
	}
	if e.runs.Load() != 0 {
		t.Errorf("handler ran %d times on bad requests", e.runs.Load())
	}
}

// TestIdempotencyKeyExpires: after the TTL the same key is a new request.
func TestIdempotencyKeyExpires(t *testing.T) {
	e := newIdemEnv(t)
	e.post("/v1/things", `"k"`, `{}`, "")
	// Age the record past its TTL.
	if _, err := e.st.PurgeIdempotency(context.Background(), time.Now().Add(store.IdempotencyTTL+time.Minute)); err != nil {
		t.Fatal(err)
	}
	if r := e.post("/v1/things", `"k"`, `{"other":1}`, ""); r.Code != 201 || e.runs.Load() != 2 {
		t.Fatalf("after expiry: %d runs=%d", r.Code, e.runs.Load())
	}
}
