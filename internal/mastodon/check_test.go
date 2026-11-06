package mastodon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/geofox/publisher/internal/destination"
)

// instance answers verify_credentials with status and body, and the
// public instance description; it records requests that carried a token.
func instance(t *testing.T, status int, body string, authed *[]string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			*authed = append(*authed, r.URL.Path)
		}
		switch r.URL.Path {
		case "/api/v1/instance":
			_, _ = w.Write([]byte(`{"uri":"masto.example"}`))
		case "/api/v1/accounts/verify_credentials":
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The remote id is the ActivityPub actor id, or the profile URL on servers
// older than Mastodon 4.2.
func TestIdentityRemoteID(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"id":"1","uri":"https://masto.example/users/me","url":"https://masto.example/@me","acct":"me"}`, "https://masto.example/users/me"},
		{`{"id":"1","url":"https://masto.example/@me","acct":"me"}`, "https://masto.example/@me"},
	} {
		var authed []string
		srv := instance(t, 200, tc.body, &authed)
		id, err := NewAccount(New(srv.URL, "tok")).Identity(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if id.RemoteID != tc.want || id.Handle == "" {
			t.Errorf("identity = %+v, want remote id %s", id, tc.want)
		}
	}
}

func TestCheck(t *testing.T) {
	yes, no := true, false
	const account, refusal, page = `{"id":"1","acct":"me"}`, `{"error":"The access token is invalid"}`, `<html>Forbidden</html>`
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		auth      bool
		ok        bool
		reachable bool
		authOK    *bool
	}{
		{"reachable", 200, account, false, true, true, nil},
		{"valid token", 200, account, true, true, true, &yes},
		{"revoked token", 401, refusal, true, false, true, &no},
		{"disabled login", 403, `{"error":"Your login is currently disabled"}`, true, false, true, &no},
		{"app-only token", 422, `{"error":"This method requires an authenticated user"}`, true, false, true, &no},
		// A firewall or proxy page says nothing about the token.
		{"firewall page", 403, page, true, false, true, nil},
		{"proxy sign-in", 401, page, true, false, true, nil},
		{"rate limited", 429, refusal, true, false, true, nil},
		{"server error", 503, refusal, true, false, false, nil},
		{"not JSON", 200, page, true, false, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var authed []string
			srv := instance(t, tc.status, tc.body, &authed)
			h := NewAccount(New(srv.URL, "tok")).Check(context.Background(), destination.CheckRequest{Authenticated: tc.auth})
			if h.OK != tc.ok || h.Reachable != tc.reachable || (h.AuthOK == nil) != (tc.authOK == nil) ||
				h.AuthOK != nil && *h.AuthOK != *tc.authOK {
				t.Errorf("health = %+v", h)
			}
			if !tc.auth && len(authed) != 0 {
				t.Errorf("an unauthenticated check sent the token to %v", authed)
			}
		})
	}
}
