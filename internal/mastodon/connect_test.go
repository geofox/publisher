package mastodon

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/geofox/publisher/internal/destination"
)

// fakeInstance is a Mastodon server's sign-in side: its metadata (none
// before 4.3), app registration and token endpoint.
type fakeInstance struct {
	srv *httptest.Server
	mu  sync.Mutex
	// meta edits the metadata the server answers (nil: none, 404).
	meta     func(m map[string]any)
	apps     int
	forms    []url.Values // token requests
	tokenErr string       // the token endpoint's error code ("": a token)
	verifier string       // the challenge's verifier the token request must carry
	register url.Values
}

func newFakeInstance(t *testing.T, v43 bool) *fakeInstance {
	t.Helper()
	f := &fakeInstance{}
	if v43 {
		f.meta = func(map[string]any) {}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		edit := f.meta
		f.mu.Unlock()
		if edit == nil {
			http.NotFound(w, r)
			return
		}
		m := map[string]any{"issuer": f.srv.URL + "/", "authorization_endpoint": f.srv.URL + "/oauth/authorize",
			"token_endpoint": f.srv.URL + "/oauth/token", "code_challenge_methods_supported": []string{"S256"},
			"scopes_supported": strings.Fields(oauthScopes)}
		edit(m)
		_ = json.NewEncoder(w).Encode(m)
	})
	mux.HandleFunc("POST /api/v1/apps", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.apps++
		n := f.apps
		f.register = r.PostForm
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "1", "client_id": "cid-" + string(rune('0'+n)),
			"client_secret": "csec-" + string(rune('0'+n)), "client_secret_expires_at": 0})
	})
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.forms = append(f.forms, r.PostForm)
		e := f.tokenErr
		f.mu.Unlock()
		if e != "" {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": e, "error_description": "secret-ish text csec-1"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-" + r.PostForm.Get("code"), "token_type": "Bearer",
			"scope": oauthScopes, "created_at": 1})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// memClients keeps registered apps in memory.
type memClients struct {
	mu     sync.Mutex
	byIss  map[string]destination.OAuthClient
	forgot int
}

func (c *memClients) Client(_ context.Context, issuer string) (destination.OAuthClient, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cl, ok := c.byIss[issuer]
	return cl, ok, nil
}

func (c *memClients) SaveClient(_ context.Context, cl destination.OAuthClient) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byIss == nil {
		c.byIss = map[string]destination.OAuthClient{}
	}
	c.byIss[cl.Issuer] = cl
	return nil
}

func (c *memClients) ForgetClient(_ context.Context, issuer string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.byIss, issuer)
	c.forgot++
	return nil
}

const verifier = "the-pkce-verifier-of-this-flow-0123456789abcdef"

func challenge() string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func connectReq(f *fakeInstance, clients *memClients) destination.ConnectRequest {
	return destination.ConnectRequest{Method: MethodOAuth, Config: map[string]any{ConfigInstanceURL: f.srv.URL},
		RedirectURI: "https://pub.example/oauth/callback/mastodon", State: "the-state", CodeChallenge: challenge(),
		Net: destination.Net{DialContext: (&net.Dialer{}).DialContext}, Clients: clients}
}

func begin(t *testing.T, f *fakeInstance, clients *memClients) (destination.ConnectStart, url.Values) {
	t.Helper()
	s, err := Driver{}.BeginConnect(context.Background(), connectReq(f, clients))
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(s.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	return s, u.Query()
}

func complete(f *fakeInstance, clients *memClients, s destination.ConnectStart, cb url.Values) (destination.ConnectResult, error) {
	return Driver{}.CompleteConnect(context.Background(), destination.ConnectComplete{ConnectRequest: connectReq(f, clients),
		Data: s.Data, Callback: cb, CodeVerifier: verifier})
}

func codeOf(err error) string {
	var ce *destination.ConnectError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return ""
}

// 4.3 and later: the metadata's endpoints, PKCE, the app registered once
// for the redirect URI, the code exchanged for a token.
func TestConnectV43(t *testing.T) {
	f := newFakeInstance(t, true)
	clients := &memClients{}
	s, q := begin(t, f, clients)
	u, _ := url.Parse(s.AuthorizeURL)
	if u.Path != "/oauth/authorize" || q.Get("response_type") != "code" || q.Get("client_id") != "cid-1" ||
		q.Get("redirect_uri") != "https://pub.example/oauth/callback/mastodon" || q.Get("scope") != oauthScopes ||
		q.Get("state") != "the-state" || q.Get("code_challenge") != challenge() || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("authorize: %s", s.AuthorizeURL)
	}
	if strings.Contains(s.AuthorizeURL, "csec") || strings.Contains(string(s.Data), "csec") {
		t.Fatal("the client secret left the server")
	}
	reg := f.register
	if reg.Get("client_name") != "Publisher (pub.example)" || reg.Get("redirect_uris") != "https://pub.example/oauth/callback/mastodon" ||
		reg.Get("scopes") != oauthScopes || reg.Get("website") != "https://pub.example" {
		t.Fatalf("registered: %v", reg)
	}
	res, err := complete(f, clients, s, url.Values{"code": {"abc"}, "state": {"the-state"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Secrets[SecretAccessToken] != "tok-abc" || res.Config[ConfigInstanceURL] != f.srv.URL {
		t.Fatalf("result: %+v", res)
	}
	form := f.forms[0]
	if form.Get("grant_type") != "authorization_code" || form.Get("code") != "abc" || form.Get("client_id") != "cid-1" ||
		form.Get("client_secret") != "csec-1" || form.Get("redirect_uri") != "https://pub.example/oauth/callback/mastodon" ||
		form.Get("code_verifier") != verifier {
		t.Fatalf("token request: %v", form)
	}
	// The app is reused.
	begin(t, f, clients)
	if f.apps != 1 {
		t.Fatalf("%d apps registered", f.apps)
	}
}

// Before 4.3: no metadata, the standard paths, no PKCE.
func TestConnectBefore43(t *testing.T) {
	f := newFakeInstance(t, false)
	clients := &memClients{}
	s, q := begin(t, f, clients)
	if !strings.HasPrefix(s.AuthorizeURL, f.srv.URL+"/oauth/authorize?") || q.Has("code_challenge") {
		t.Fatalf("authorize: %s", s.AuthorizeURL)
	}
	if _, err := complete(f, clients, s, url.Values{"code": {"abc"}}); err != nil {
		t.Fatal(err)
	}
	if f.forms[0].Has("code_verifier") {
		t.Fatal("a verifier was sent without PKCE")
	}
}

// Metadata naming another issuer, or an endpoint elsewhere, fails before
// anything is registered; an answer from another issuer, or without the
// one the server promised, fails the exchange.
func TestConnectIssuer(t *testing.T) {
	for name, edit := range map[string]func(m map[string]any){
		"issuer":    func(m map[string]any) { m["issuer"] = "https://evil.example/" },
		"authorize": func(m map[string]any) { m["authorization_endpoint"] = "https://evil.example/oauth/authorize" },
		"token":     func(m map[string]any) { m["token_endpoint"] = "https://evil.example/oauth/token" },
		"userinfo": func(m map[string]any) {
			m["token_endpoint"] = strings.Replace(m["token_endpoint"].(string), "://", "://u:p@", 1)
		},
	} {
		f := newFakeInstance(t, true)
		f.meta = edit
		_, err := Driver{}.BeginConnect(context.Background(), connectReq(f, &memClients{}))
		if codeOf(err) != destination.ConnectIssuerMismatch || f.apps != 0 {
			t.Errorf("%s: %v (%d apps)", name, err, f.apps)
		}
	}
	f := newFakeInstance(t, true)
	f.meta = func(m map[string]any) { m["authorization_response_iss_parameter_supported"] = true }
	clients := &memClients{}
	s, _ := begin(t, f, clients)
	for name, cb := range map[string]url.Values{
		"missing": {"code": {"abc"}},
		"other":   {"code": {"abc"}, "iss": {"https://evil.example"}},
		"twice":   {"code": {"abc"}, "iss": {f.srv.URL, "https://evil.example"}},
	} {
		if _, err := complete(f, clients, s, cb); codeOf(err) != destination.ConnectIssuerMismatch {
			t.Errorf("iss %s: %v", name, err)
		}
	}
	if len(f.forms) != 0 {
		t.Fatal("a code was exchanged with a bad issuer")
	}
	if _, err := complete(f, clients, s, url.Values{"code": {"abc"}, "iss": {f.srv.URL + "/"}}); err != nil {
		t.Fatalf("the right iss: %v", err)
	}
}

// A server that forgot the app: the exchange fails, saying only the
// OAuth code, and the next sign-in registers again.
func TestConnectInvalidClient(t *testing.T) {
	f := newFakeInstance(t, true)
	clients := &memClients{}
	s, _ := begin(t, f, clients)
	f.tokenErr = "invalid_client"
	_, err := complete(f, clients, s, url.Values{"code": {"abc"}})
	if codeOf(err) != destination.ConnectExchangeFailed || clients.forgot != 1 {
		t.Fatalf("invalid client: %v (forgot %d)", err, clients.forgot)
	}
	if strings.Contains(err.Error(), "csec") || strings.Contains(err.Error(), "secret-ish") || !strings.Contains(err.Error(), "invalid_client") {
		t.Fatalf("the error: %v", err)
	}
	f.tokenErr = ""
	_, q := begin(t, f, clients)
	if f.apps != 2 || q.Get("client_id") != "cid-2" {
		t.Fatalf("not registered again: %d apps, %s", f.apps, q.Get("client_id"))
	}
	// The first flow's app is gone: its code can't be exchanged.
	if _, err := complete(f, clients, s, url.Values{"code": {"abc"}}); codeOf(err) != destination.ConnectExchangeFailed {
		t.Fatalf("the old flow: %v", err)
	}
	// Another refusal keeps the app.
	f.tokenErr = "invalid_grant"
	s, _ = begin(t, f, clients)
	if _, err := complete(f, clients, s, url.Values{"code": {"abc"}}); codeOf(err) != destination.ConnectExchangeFailed || clients.forgot != 1 {
		t.Fatalf("invalid grant: %v (forgot %d)", err, clients.forgot)
	}
	// No code, no exchange.
	if _, err := complete(f, clients, s, url.Values{"state": {"x"}}); codeOf(err) != destination.ConnectExchangeFailed {
		t.Fatalf("no code: %v", err)
	}
}

// A server that can't be reached, or answers the metadata with an error,
// fails with a fixed code; a private one is the guard's refusal.
func TestConnectUnreachable(t *testing.T) {
	f := newFakeInstance(t, true)
	f.meta = func(m map[string]any) {}
	req := connectReq(f, &memClients{})
	req.Config[ConfigInstanceURL] = "http://127.0.0.1:1"
	if _, err := (Driver{}).BeginConnect(context.Background(), req); codeOf(err) != destination.ConnectExchangeFailed {
		t.Fatalf("unreachable: %v", err)
	}
	req = connectReq(f, &memClients{})
	req.Net = destination.GuardedNet(func(string, net.IP) error { return errors.New("private") })
	if _, err := (Driver{}).BeginConnect(context.Background(), req); !errors.Is(err, destination.ErrBlockedAddress) {
		t.Fatalf("guarded: %v", err)
	}
}
