package bluesky

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/geofox/publisher/internal/destination"
)

// checkPDS answers the calls a check and an identity make, and records
// which paths carried credentials.
type checkPDS struct {
	mu            sync.Mutex
	createStatus  int    // createSession's status (0: 200)
	createBody    string // its error body
	getSessionErr []int  // statuses getSession answers in turn (then 200)
	health        int    // _health status (0: 200)
	authed        map[string]int
	calls         map[string]int
}

func (f *checkPDS) serve(t *testing.T) *httptest.Server {
	f.authed, f.calls = map[string]int{}, map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls[r.URL.Path]++
		if r.Header.Get("Authorization") != "" {
			f.authed[r.URL.Path]++
		}
		switch r.URL.Path {
		case "/xrpc/_health":
			if f.health != 0 {
				w.WriteHeader(f.health)
				return
			}
			_, _ = w.Write([]byte(`{"version":"0.4"}`))
		case "/xrpc/com.atproto.server.createSession":
			if f.createStatus != 0 {
				w.WriteHeader(f.createStatus)
				_, _ = w.Write([]byte(f.createBody))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"accessJwt": "jwt", "refreshJwt": "r",
				"did": "did:plc:abc", "handle": "me.example"})
		case "/xrpc/com.atproto.server.getSession":
			if len(f.getSessionErr) > 0 {
				code := f.getSessionErr[0]
				f.getSessionErr = f.getSessionErr[1:]
				w.WriteHeader(code)
				_, _ = w.Write([]byte(`{"error":"ExpiredToken"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"did": "did:plc:abc"})
		case "/xrpc/app.bsky.actor.getProfile":
			_ = json.NewEncoder(w).Encode(map[string]string{"handle": "me.example", "displayName": "Me"})
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newCheckedAccount(url string) *Account { return &Account{C: New(url, "me.example", "pw")} }

func TestIdentityRemoteIDIsTheDID(t *testing.T) {
	f := &checkPDS{}
	srv := f.serve(t)
	id, err := newCheckedAccount(srv.URL).Identity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if id.RemoteID != "did:plc:abc" || id.Handle != "me.example" || id.DisplayName != "Me" {
		t.Errorf("identity = %+v", id)
	}
}

// A readiness check signs nothing in and sends no credential.
func TestCheckUnauthenticatedSendsNoCredential(t *testing.T) {
	f := &checkPDS{}
	srv := f.serve(t)
	h := newCheckedAccount(srv.URL).Check(context.Background(), destination.CheckRequest{})
	if !h.OK || !h.Reachable || h.AuthOK != nil {
		t.Errorf("health = %+v", h)
	}
	if f.calls["/xrpc/com.atproto.server.createSession"] != 0 || len(f.authed) != 0 {
		t.Errorf("credentials sent: calls %v, authed %v", f.calls, f.authed)
	}
	f.health = 503
	if h := newCheckedAccount(srv.URL).Check(context.Background(), destination.CheckRequest{}); h.OK || h.Reachable {
		t.Errorf("503: health = %+v", h)
	}
}

func TestCheckAuthenticated(t *testing.T) {
	for _, tc := range []struct {
		name       string
		pds        *checkPDS
		ok         bool
		authOK     *bool
		reachable  bool
		getSession int // getSession calls expected
	}{
		{"valid", &checkPDS{}, true, ptr(true), true, 1},
		{"wrong password", &checkPDS{createStatus: 401, createBody: `{"error":"AuthenticationRequired","message":"Invalid identifier or password"}`},
			false, ptr(false), true, 0},
		{"taken down", &checkPDS{createStatus: 400, createBody: `{"error":"AccountTakedown"}`}, false, ptr(false), true, 0},
		{"rate limited", &checkPDS{createStatus: 429, createBody: `{"error":"RateLimitExceeded"}`}, false, nil, true, 0},
		{"server down", &checkPDS{createStatus: 502}, false, nil, false, 0},
		// An expired cached session is replaced once, not reported as a
		// bad password.
		{"expired session", &checkPDS{getSessionErr: []int{400}}, true, ptr(true), true, 2},
		{"refused twice", &checkPDS{getSessionErr: []int{401, 401}}, false, ptr(false), true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.pds
			srv := f.serve(t)
			h := newCheckedAccount(srv.URL).Check(context.Background(), destination.CheckRequest{Authenticated: true})
			if h.OK != tc.ok || h.Reachable != tc.reachable || !sameBool(h.AuthOK, tc.authOK) {
				t.Errorf("health = %+v (auth_ok %v)", h, deref(h.AuthOK))
			}
			if got := f.calls["/xrpc/com.atproto.server.getSession"]; got != tc.getSession {
				t.Errorf("getSession called %d times, want %d", got, tc.getSession)
			}
			if !h.OK && h.Message == "" {
				t.Error("a failed check has no message")
			}
		})
	}
}

func TestCheckUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	for _, auth := range []bool{false, true} {
		h := newCheckedAccount(url).Check(context.Background(), destination.CheckRequest{Authenticated: auth})
		// The port was just closed; the message depends on what the system
		// says about it, so only its presence is checked.
		if h.OK || h.Reachable || h.AuthOK != nil || h.Message == "" {
			t.Errorf("authenticated=%v: health = %+v", auth, h)
		}
	}
}

func ptr(b bool) *bool { return &b }

func sameBool(a, b *bool) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

func deref(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}
