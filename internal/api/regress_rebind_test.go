package api

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// DNS rebinding: a page on evil.test re-resolves its own name to the
// server's address. The browser then sends Host: evil.test and a matching
// Origin, which the CSRF guard accepts. With ALLOWED_HOSTS set, the Host
// itself is refused.
func TestDNSRebindingRefusedWithAllowedHosts(t *testing.T) {
	e := newV1Env(t, false)
	e.api.AllowedHosts = []string{"post.example.com"}
	h := e.api.Routes()
	send := func(host string) int {
		r := httptest.NewRequest("POST", "http://"+host+"/v1/tokens", strings.NewReader(`{"name":"x","scopes":["admin"]}`))
		r.Host = host
		r.Header.Set("Origin", "http://"+host)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if c := send("evil.test:8080"); c != 421 {
		t.Errorf("foreign host: %d, want 421", c)
	}
	for _, host := range []string{"post.example.com", "localhost:8080", "127.0.0.1:8080"} {
		if c := send(host); c != 201 {
			t.Errorf("%s: %d, want 201", host, c)
		}
	}
}
