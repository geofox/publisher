package mastodon

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/destination"
)

// Mastodon renders a line break inside a paragraph as <br />; stripping it
// glues the two lines together in a fan-out reproduction.
func TestDeHTMLKeepsLineBreaks(t *testing.T) {
	got := deHTML(`<p>first line<br />second line<br>third</p>`)
	if want := "first line\nsecond line\nthird"; got != want {
		t.Errorf("deHTML = %q, want %q", got, want)
	}
}

// A 429 on POST /api/v1/statuses is retried inside go-mastodon with the
// request body already consumed, and the app never sees the rate limit.
func TestStatusRateLimitIsSurfaced(t *testing.T) {
	var calls atomic.Int32
	var secondBody atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/statuses" {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		if calls.Add(1) == 1 {
			w.Header().Set("X-RateLimit-Reset", time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		secondBody.Store(string(b))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","url":"https://m.example/@a/1"}`))
	}))
	defer srv.Close()

	a := NewAccount(New(srv.URL, "tok"))
	start := time.Now()
	_, err := a.Publish(context.Background(), destination.PublishRequest{Text: "hello", IdempotencyKey: "k"})
	t.Logf("calls=%d elapsed=%s err=%v second body=%q", calls.Load(), time.Since(start).Round(time.Millisecond), err, secondBody.Load())
	if c := destination.ClassOf(err); c != destination.ClassRateLimited {
		t.Errorf("class = %q, want rate_limited (the library retried the 429 itself)", c)
	}
}

// The go-mastodon client that uploads media and creates statuses must not
// hang on a server that accepts and never answers, yet must not cut a
// slow, large upload short: it bounds the dial, handshake and answer, not
// the whole request.
func TestStatusClientHasTimeout(t *testing.T) {
	c := New("https://m.example", "tok")
	if c.c.Client.Timeout != 0 {
		t.Errorf("overall Timeout = %s: a large video on a slow uplink would never finish", c.c.Client.Timeout)
	}
	rl, ok := c.c.Client.Transport.(rateLimitTransport)
	if !ok {
		t.Fatalf("transport = %T", c.c.Client.Transport)
	}
	tr := rl.base.(idempotencyTransport).base.(*http.Transport)
	if tr.ResponseHeaderTimeout == 0 || tr.TLSHandshakeTimeout == 0 {
		t.Errorf("no answer/handshake timeout: %+v", tr)
	}
}
