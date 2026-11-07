package threads

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/destination"
)

const (
	testAppID     = "1234567890"
	testAppSecret = "app-secret/+=0123"
	testRedirect  = "https://pub.example/oauth/callback/threads"
)

// fakeMeta is the Graph API's sign-in side: the code exchange and the
// long-lived exchange.
type fakeMeta struct {
	srv *httptest.Server
	mu  sync.Mutex
	// codeErr and longErr: the step answers this status with a Graph
	// error that quotes what was sent (0: a token).
	codeErr, longErr int
	ttl              int64 // the long-lived token's expires_in
	codes            []url.Values
	longs            []url.Values
}

func newFakeMeta(t *testing.T) *fakeMeta {
	t.Helper()
	f := &fakeMeta{ttl: 5183944}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		f.codes = append(f.codes, r.PostForm)
		if f.codeErr != 0 {
			w.WriteHeader(f.codeErr)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": "OAuthException", "code": 100,
				"message": "Invalid code " + r.PostForm.Get("code") + " for secret " + r.PostForm.Get("client_secret")}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "short-" + r.PostForm.Get("code"), "user_id": 17841400000})
	})
	mux.HandleFunc("GET /access_token", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f.mu.Lock()
		defer f.mu.Unlock()
		f.longs = append(f.longs, q)
		if f.longErr != 0 {
			w.WriteHeader(f.longErr)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": "OAuthException", "code": 190,
				"message": "Bad token " + q.Get("access_token") + " / " + q.Get("client_secret")}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "long-" + q.Get("access_token"), "token_type": "bearer",
			"expires_in": f.ttl})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	pointAt(t, f.srv.URL)
	return f
}

// pointAt sends the sign-in to base for the test.
func pointAt(t *testing.T, base string) {
	t.Helper()
	a, tk, ex := AuthorizeURL, TokenURL, ExchangeURL
	t.Cleanup(func() { AuthorizeURL, TokenURL, ExchangeURL = a, tk, ex })
	AuthorizeURL, TokenURL, ExchangeURL = base+"/oauth/authorize", base+"/oauth/access_token", base+"/access_token"
}

func threadsReq() destination.ConnectRequest {
	return destination.ConnectRequest{Method: MethodOAuth, Config: map[string]any{ConfigUserID: "me"},
		InstanceConfig:  map[string]any{InstanceAppID: testAppID},
		InstanceSecrets: map[string]string{InstanceAppSecret: testAppSecret},
		RedirectURI:     testRedirect, State: "the-state", CodeChallenge: "a-challenge",
		Net: destination.Net{DialContext: (&net.Dialer{}).DialContext}}
}

func threadsComplete(req destination.ConnectRequest, s destination.ConnectStart, cb url.Values) (destination.ConnectResult, error) {
	return Driver{}.CompleteConnect(context.Background(), destination.ConnectComplete{ConnectRequest: req, Data: s.Data,
		Callback: cb, CodeVerifier: "unused"})
}

func connectCode(err error) string {
	var ce *destination.ConnectError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return ""
}

// The sign-in address carries the app, the redirect URI, the scopes and
// the state, never the secret; the code becomes a short-lived token, and
// that a long-lived one whose expiry the refresher takes over.
func TestConnect(t *testing.T) {
	f := newFakeMeta(t)
	req := threadsReq()
	s, err := Driver{}.BeginConnect(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(s.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Path != "/oauth/authorize" || q.Get("client_id") != testAppID || q.Get("redirect_uri") != testRedirect ||
		q.Get("scope") != "threads_basic,threads_content_publish,threads_manage_replies" ||
		q.Get("response_type") != "code" || q.Get("state") != "the-state" || q.Has("code_challenge") {
		t.Fatalf("authorize: %s", s.AuthorizeURL)
	}
	if strings.Contains(s.AuthorizeURL, "app-secret") || strings.Contains(string(s.Data), "app-secret") {
		t.Fatal("the app secret left the server")
	}
	before := time.Now().UTC()
	res, err := threadsComplete(req, s, url.Values{"code": {"abc#_"}, "state": {"the-state"}})
	if err != nil {
		t.Fatal(err)
	}
	c := f.codes[0]
	if c.Get("client_id") != testAppID || c.Get("client_secret") != testAppSecret || c.Get("code") != "abc" ||
		c.Get("grant_type") != "authorization_code" || c.Get("redirect_uri") != testRedirect {
		t.Fatalf("code exchange: %v", c)
	}
	l := f.longs[0]
	if l.Get("grant_type") != "th_exchange_token" || l.Get("client_secret") != testAppSecret || l.Get("access_token") != "short-abc" {
		t.Fatalf("long-lived exchange: %v", l)
	}
	if res.Secrets[SecretAccessToken] != "long-short-abc" || len(res.Secrets) != 1 {
		t.Fatalf("secrets: %v", res.Secrets)
	}
	st, err := openTokenState(res.Config)
	if err != nil {
		t.Fatal(err)
	}
	ttl := time.Duration(f.ttl) * time.Second
	if st.refreshedAt.Before(before.Add(-time.Second)) || st.expiresAt.Sub(st.refreshedAt) != ttl || st.known().IsZero() {
		t.Fatalf("token state: %+v", st)
	}
	a := &Account{tokens: st}
	if next := a.NextRefresh(time.Now()); !next.Equal(st.expiresAt.Add(-refreshWindow)) {
		t.Fatalf("next refresh %v", next)
	}
}

// Without the app, or with a redirect URI Meta refuses, the sign-in
// doesn't start, saying what to change.
func TestConnectNeedsAppAndHTTPS(t *testing.T) {
	newFakeMeta(t)
	for name, edit := range map[string]func(r *destination.ConnectRequest){
		"no app id":     func(r *destination.ConnectRequest) { delete(r.InstanceConfig, InstanceAppID) },
		"no app secret": func(r *destination.ConnectRequest) { r.InstanceSecrets = nil },
		"http":          func(r *destination.ConnectRequest) { r.RedirectURI = "http://pub.example/oauth/callback/threads" },
		"localhost":     func(r *destination.ConnectRequest) { r.RedirectURI = "https://localhost:8080/oauth/callback/threads" },
		"sub.localhost": func(r *destination.ConnectRequest) { r.RedirectURI = "https://pub.localhost/oauth/callback/threads" },
		"loopback":      func(r *destination.ConnectRequest) { r.RedirectURI = "https://127.0.0.1/oauth/callback/threads" },
		"loopback v6":   func(r *destination.ConnectRequest) { r.RedirectURI = "https://[::1]/oauth/callback/threads" },
	} {
		req := threadsReq()
		edit(&req)
		_, err := Driver{}.BeginConnect(context.Background(), req)
		var fe *destination.FieldError
		if !errors.As(err, &fe) || fe.Key != "method" || fe.Code != "not_allowed" {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The app changed between the two steps, or the answer has no code:
// nothing is exchanged.
func TestConnectAppChanged(t *testing.T) {
	f := newFakeMeta(t)
	req := threadsReq()
	s, err := Driver{}.BeginConnect(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	other := threadsReq()
	other.InstanceConfig = map[string]any{InstanceAppID: "999"}
	if _, err := threadsComplete(other, s, url.Values{"code": {"abc"}}); connectCode(err) != destination.ConnectExchangeFailed {
		t.Fatalf("another app: %v", err)
	}
	if _, err := threadsComplete(req, s, url.Values{"code": {"#_"}}); connectCode(err) != destination.ConnectExchangeFailed {
		t.Fatalf("no code: %v", err)
	}
	if _, err := threadsComplete(req, destination.ConnectStart{Data: json.RawMessage(`{}`)}, url.Values{"code": {"abc"}}); connectCode(err) != destination.ConnectExchangeFailed {
		t.Fatalf("no data: %v", err)
	}
	if len(f.codes) != 0 {
		t.Fatal("a code was exchanged")
	}
}

// A refusal at either step says the step, the status and the Graph
// error's type and code, never Meta's words, the code, the tokens or the
// app secret; so does a server that can't be reached.
func TestConnectErrorsKeepSecrets(t *testing.T) {
	f := newFakeMeta(t)
	req := threadsReq()
	s, err := Driver{}.BeginConnect(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	clean := func(name string, err error, want string) {
		t.Helper()
		if connectCode(err) != destination.ConnectExchangeFailed {
			t.Errorf("%s: %v", name, err)
			return
		}
		msg := err.Error()
		for _, leak := range []string{"app-secret", url.QueryEscape(testAppSecret), "short-", "the-code", "Invalid", "Bad token"} {
			if strings.Contains(msg, leak) {
				t.Errorf("%s: the error quotes %q: %s", name, leak, msg)
			}
		}
		if !strings.Contains(msg, want) {
			t.Errorf("%s: %q lacks %q", name, msg, want)
		}
	}
	f.codeErr = 400
	_, err = threadsComplete(req, s, url.Values{"code": {"the-code"}})
	clean("code", err, "the code exchange answered 400 (OAuthException 100)")
	f.codeErr, f.longErr = 0, 400
	_, err = threadsComplete(req, s, url.Values{"code": {"the-code"}})
	clean("long-lived", err, "the long-lived token exchange answered 400 (OAuthException 190)")
	f.longErr, f.ttl = 0, 0
	_, err = threadsComplete(req, s, url.Values{"code": {"the-code"}})
	clean("no lifetime", err, "no lifetime")
	// The long-lived step's server is gone: the transport error's
	// address carries the secret and the token.
	f.ttl = 60
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	ExchangeURL = gone.URL + "/access_token"
	_, err = threadsComplete(req, s, url.Values{"code": {"the-code"}})
	clean("unreachable", err, "the long-lived token exchange")
}
