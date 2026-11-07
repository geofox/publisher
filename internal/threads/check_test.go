package threads

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	c.DebugURL = srv.URL + "/v1.0/debug_token" // 404: Meta doesn't say
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

// An authenticated check that works asks when Meta's grant ends and
// reports it; when Meta stops saying, the last answer stays; a check that
// fails, or an unauthenticated one, doesn't ask. The token never reaches
// an error.
func TestCheckReportsGrantEnd(t *testing.T) {
	end := time.Date(2027, 2, 4, 10, 0, 0, 0, time.UTC)
	var debug atomic.Int32
	var answer atomic.Value
	answer.Store(fmt.Sprintf(`{"data":{"is_valid":true,"data_access_expires_at":%d,"expires_at":1}}`, end.Unix()))
	var meStatus atomic.Int32
	meStatus.Store(200)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1.0/me":
			w.WriteHeader(int(meStatus.Load()))
			_, _ = w.Write([]byte(`{"id":"1","error":{"code":190}}`))
		case "/v1.0/debug_token":
			debug.Add(1)
			if r.URL.Query().Get("input_token") != "tok-x/+=" || r.URL.Query().Get("access_token") != "tok-x/+=" {
				w.WriteHeader(400)
				return
			}
			a := answer.Load().(string)
			if a == "" {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":{"message":"bad tok-x/+=","type":"OAuthException","code":100}}`))
				return
			}
			_, _ = w.Write([]byte(a))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	c := New("tok-x/+=", "")
	c.BaseURL, c.DebugURL = srv.URL+"/v1.0", srv.URL+"/v1.0/debug_token"
	a := &Account{C: c}
	check := func(auth bool) *time.Time {
		t.Helper()
		return a.Check(context.Background(), destination.CheckRequest{Authenticated: auth}).ReconnectBy
	}
	if got := check(true); got == nil || !got.Equal(end) {
		t.Fatalf("reconnect_by = %v, want %v", got, end)
	}
	answer.Store("")
	if got := check(true); got == nil || !got.Equal(end) {
		t.Fatalf("after Meta stopped saying: %v, want the last answer", got)
	}
	if _, err := c.GrantEnd(context.Background()); err == nil || strings.Contains(err.Error(), "tok-x") ||
		strings.Contains(err.Error(), url.QueryEscape("tok-x/+=")) || !strings.Contains(err.Error(), "OAuthException 100") {
		t.Fatalf("GrantEnd's error: %v", err)
	}
	n := debug.Load()
	if check(false); debug.Load() != n {
		t.Fatal("an unauthenticated check asked Meta")
	}
	meStatus.Store(400)
	if check(true); debug.Load() != n {
		t.Fatal("a refused token was sent to debug_token")
	}
	// Valid but no end: never.
	meStatus.Store(200)
	answer.Store(`{"data":{"is_valid":true,"data_access_expires_at":0}}`)
	if got := check(true); got != nil {
		t.Fatalf("no end: %v", got)
	}
}
