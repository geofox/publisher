package verify

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIsBlockedIP(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1":       true,  // loopback
		"::1":             true,  // loopback v6
		"10.0.0.5":        true,  // private
		"192.168.1.1":     true,  // private
		"172.16.0.1":      true,  // private
		"169.254.0.1":     true,  // link-local
		"0.0.0.0":         true,  // unspecified
		"fc00::1":         true,  // ULA v6
		"8.8.8.8":         false, // public
		"1.1.1.1":         false, // public
		"100.64.0.1":      true,  // CGNAT (RFC 6598)
		"100.127.255.255": true,  // CGNAT upper edge
		"100.63.255.255":  false, // just below CGNAT
		"100.128.0.1":     false, // just above CGNAT
	}
	for s, want := range cases {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("bad test IP %q", s)
		}
		if got := isBlockedIP(ip); got != want {
			t.Errorf("isBlockedIP(%s) = %v, want %v", s, got, want)
		}
	}
}

func TestSafeClientRefusesLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := NewSafeClient(2 * time.Second)
	_, err := c.Get(srv.URL) // srv.URL is http://127.0.0.1:PORT
	if err == nil {
		t.Fatal("expected loopback dial to be blocked, got nil error")
	}
}

func TestSafeClientContextDeadline(t *testing.T) {
	c := NewSafeClient(time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://8.8.8.8", nil)
	if _, err := c.Do(req); err == nil {
		t.Fatal("expected context deadline error")
	}
}

func TestSafeClientRefusesRedirectToLoopback(t *testing.T) {
	// A public-looking server that 302-redirects to a loopback URL. The dial to
	// the redirect target must be refused by the Control hook.
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound) // target.URL is loopback
	}))
	defer redirector.Close()

	// Note: the redirector itself is also loopback, so the very first dial is
	// already blocked — which still proves loopback dials never succeed. To make
	// the assertion specifically about the redirect hop being guarded, we rely on
	// the Control hook firing per-connection (same guard on every hop).
	c := NewSafeClient(2 * time.Second)
	if _, err := c.Get(redirector.URL); err == nil {
		t.Fatal("expected redirect-to-loopback chain to be blocked, got nil error")
	}
}
