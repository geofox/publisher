package threads

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/destination/destinationtest"
)

// graph answers /me with status and body, and the API root; it records
// requests that carried the token.
func graph(t *testing.T, status int, body string, authed *[]string) *Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			*authed = append(*authed, r.URL.Path)
		}
		switch {
		case r.URL.Path == "/":
			w.WriteHeader(400) // the real root answers 400 without a path
		case strings.HasPrefix(r.URL.Path, "/v1.0/me"):
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	c := New("tok", "")
	c.BaseURL = srv.URL + "/v1.0"
	return c
}

func TestIdentityRemoteIDIsTheUserID(t *testing.T) {
	var authed []string
	c := graph(t, 200, `{"id":"1789","username":"me","name":"Me"}`, &authed)
	id, err := (&Account{C: c}).Identity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if id.RemoteID != "1789" || id.Handle != "@me" || id.DisplayName != "Me" {
		t.Errorf("identity = %+v", id)
	}
}

func TestCheck(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		auth      bool
		ok        bool
		reachable bool
		authOK    *bool
	}{
		{"reachable", 200, `{"id":"1"}`, false, true, true, nil},
		{"valid token", 200, `{"id":"1"}`, true, true, true, &yes},
		{"expired token", 400, `{"error":{"code":190,"error_subcode":463}}`, true, false, true, &no},
		{"unauthorized", 401, `{"error":{"code":190}}`, true, false, true, &no},
		{"throttled", 400, `{"error":{"code":4}}`, true, false, true, nil},
		{"server error", 500, `{}`, true, false, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var authed []string
			c := graph(t, tc.status, tc.body, &authed)
			h := (&Account{C: c}).Check(context.Background(), destination.CheckRequest{Authenticated: tc.auth})
			if h.OK != tc.ok || h.Reachable != tc.reachable || (h.AuthOK == nil) != (tc.authOK == nil) ||
				h.AuthOK != nil && *h.AuthOK != *tc.authOK {
				t.Errorf("health = %+v", h)
			}
			if !tc.auth && len(authed) != 0 {
				t.Errorf("an unauthenticated check sent the token to %v", authed)
			}
			if h.ExpiresAt != nil {
				t.Errorf("expiry %v reported before the token manager set one", h.ExpiresAt)
			}
			if tc.name == "throttled" && !strings.Contains(h.Message, "rate limiting") {
				t.Errorf("a Graph rate limit reads as %q", h.Message)
			}
		})
	}
}

// The account reports the expiry it knows: none after a seed (the
// provisional expiry only schedules the first refresh), the real one once
// a refresh has set it, including after a restart.
func TestCheckReportsTokenExpiry(t *testing.T) {
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	check := func(s tokenState) *time.Time {
		t.Helper()
		var calls atomic.Int32
		a := openWithState(t, destinationtest.NewHost(t).AllowDial(), refreshServer(t, 200, &calls), s, now)
		var authed []string
		a.C.BaseURL = graph(t, 200, `{"id":"1"}`, &authed).BaseURL
		return a.Check(context.Background(), destination.CheckRequest{}).ExpiresAt
	}
	if exp := check(tokenState{expiresAt: now.Add(SeedProvisional), refreshedAt: now}); exp != nil {
		t.Fatalf("expires_at = %v after a seed, want unknown", exp)
	}
	real := now.Add(60 * 24 * time.Hour)
	if exp := check(tokenState{expiresAt: real, refreshedAt: now}); exp == nil || !exp.Equal(real) {
		t.Fatalf("expires_at = %v, want %v", exp, real)
	}
}
