package threads

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/destination"
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
		})
	}
}

// The account reports the expiry the token manager recorded, on seeding
// and on every refresh.
func TestCheckReportsTokenExpiry(t *testing.T) {
	var authed []string
	c := graph(t, 200, `{"id":"1"}`, &authed)
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	m := newMgr(&fakeStore{}, c, &fakeNotifier{}, "envtoken", func() time.Time { return now })
	m.ensureSeed(now)
	h := (&Account{C: c}).Check(context.Background(), destination.CheckRequest{})
	if h.ExpiresAt == nil || !h.ExpiresAt.Equal(now.Add(seedProvisional)) {
		t.Fatalf("expires_at = %v, want the provisional %v", h.ExpiresAt, now.Add(seedProvisional))
	}
}

func TestTokenManagerRecordsRefreshedExpiry(t *testing.T) {
	t0 := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	cur := t0
	fc := &expiryClient{fakeClient: fakeClient{tok: "fresh", ttl: 60 * 24 * time.Hour}}
	m := newMgr(&fakeStore{}, fc, &fakeNotifier{}, "envtoken", func() time.Time { return cur })
	m.ensureSeed(cur)
	if !fc.expiry.Equal(t0.Add(seedProvisional)) {
		t.Fatalf("seed expiry = %v", fc.expiry)
	}
	cur = t0.Add(2 * 24 * time.Hour) // old enough, within the refresh window
	m.tick(context.Background())
	if want := cur.Add(60 * 24 * time.Hour); !fc.expiry.Equal(want) {
		t.Fatalf("refreshed expiry = %v, want %v", fc.expiry, want)
	}
}

type expiryClient struct {
	fakeClient
	expiry time.Time
}

func (e *expiryClient) SetTokenExpiry(t time.Time) { e.expiry = t }
