package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/accounts"
	"github.com/geofox/publisher/internal/auth"
	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/drivers"
	"github.com/geofox/publisher/internal/store"
)

var _ AccountConnector = (*accounts.Registry)(nil)

// apiConnectDriver is a type whose accounts connect by signing in at the
// network. Its "network" answers the callback's code:
//
//   - "c-<x>": the token <x> (the account is "apiconnect-<x>");
//   - "many": two accounts to choose from;
//   - anything else: the exchange fails, the network quoting a secret.
//
// A server "down" can't be prepared (its answer quoting a secret), one
// "blocked" is on a private address.
type apiConnectDriver struct{}

const apiConnectType = "apiconnect"

func (apiConnectDriver) Manifest() destination.Manifest {
	// A real type's capabilities, so its accounts read as the document says.
	masto, _ := destination.Lookup("mastodon")
	return destination.Manifest{Contract: destination.ContractVersion, Type: apiConnectType, Name: "Connect Test",
		Kinds: []destination.Kind{destination.KindNote}, Defaults: masto.Manifest().Defaults,
		Auth: []destination.AuthMethod{{ID: "oauth", Flow: destination.FlowRedirect, Label: "Sign in", NeedsPublicURL: true,
			Fields: []destination.FieldSpec{{Key: "server", Label: "Server", Type: destination.FieldString, Required: true, Audience: true}}}},
	}
}

func (apiConnectDriver) Open(context.Context, destination.OpenRequest) (destination.Account, error) {
	return nil, errors.New("opened through the test opener")
}

func (apiConnectDriver) BeginConnect(_ context.Context, req destination.ConnectRequest) (destination.ConnectStart, error) {
	switch req.Config["server"] {
	case "blocked":
		return destination.ConnectStart{}, destination.ErrBlockedAddress
	case "down":
		return destination.ConnectStart{}, &destination.ConnectError{Code: destination.ConnectExchangeFailed,
			Err: errors.New("discovery answered 500: client_secret=s3cret")}
	}
	q := url.Values{"state": {req.State}, "redirect_uri": {req.RedirectURI}, "code_challenge": {req.CodeChallenge}}
	return destination.ConnectStart{AuthorizeURL: "https://net.example/authorize?" + q.Encode(), Data: json.RawMessage(`{}`)}, nil
}

func (apiConnectDriver) CompleteConnect(_ context.Context, req destination.ConnectComplete) (destination.ConnectResult, error) {
	cfg := map[string]any{"server": req.Config["server"]}
	switch code := req.Callback.Get("code"); {
	case strings.HasPrefix(code, "c-"):
		return destination.ConnectResult{Config: cfg, Secrets: map[string]string{"token": strings.TrimPrefix(code, "c-")}}, nil
	case code == "many":
		return destination.ConnectResult{Candidates: []destination.ConnectResult{
			{Config: cfg, Secrets: map[string]string{"token": "pick-a"}, Identity: destination.Identity{RemoteID: "apiconnect-pick-a", Handle: "a"}},
			{Config: cfg, Secrets: map[string]string{"token": "pick-b"}, Identity: destination.Identity{RemoteID: "apiconnect-pick-b", Handle: "b", DisplayName: "Bee"}},
		}}, nil
	}
	return destination.ConnectResult{}, &destination.ConnectError{Code: destination.ConnectExchangeFailed,
		Err: errors.New("the network said: bad code, client_secret=s3cret")}
}

// newConnectEnv is newWriteEnv (signed in with OIDC) with connect flows
// and a public address; a second allowed user has a session too.
func newConnectEnv(t *testing.T) (*writeEnv, *http.Cookie) {
	t.Helper()
	drivers.Register()
	// Registered for this test only: others list the real types.
	destination.Register(apiConnectDriver{})
	t.Cleanup(func() { destination.Unregister(apiConnectType) })
	w := newWriteEnv(t, true, false)
	w.api.AppBaseURL = "https://pub.example"
	w.api.Allowlist = auth.NewAllowlist([]string{"sub-1", "sub-2"}, nil)
	u, err := w.st.UpsertUser("sub-2", "bob@example.com", "Bob")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := w.st.CreateSession(u.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return w, &http.Cookie{Name: sessionCookie, Value: raw}
}

// startFlow starts connecting an account of server and answers the flow
// and its state.
func (w *writeEnv) startFlow(server string) (v1ConnectStartBody, string) {
	w.t.Helper()
	r := w.do(req{method: "POST", path: "/v1/accounts:connect", body: fmt.Sprintf(`{"type":%q,"config":{"server":%q}}`, apiConnectType, server)})
	wantStatus(w.t, r, http.StatusCreated)
	var b v1ConnectStartBody
	r.json(&b)
	u, err := url.Parse(b.AuthorizeURL)
	if err != nil {
		w.t.Fatal(err)
	}
	if got := r.Header().Get("Location"); got != "/v1/connect-flows/"+b.Flow {
		w.t.Fatalf("Location = %q", got)
	}
	return b, u.Query().Get("state")
}

// back is the network sending the owner back, with cookie (nil: none).
func (w *writeEnv) back(q url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	w.t.Helper()
	r := httptest.NewRequest("GET", "/oauth/callback/"+apiConnectType+"?"+q.Encode(), nil)
	r.Header.Set("Sec-Fetch-Mode", "navigate")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	w.h.ServeHTTP(rec, r)
	return rec
}

func (w *writeEnv) flow(id string) v1ConnectFlowBody {
	w.t.Helper()
	r := w.do(req{method: "GET", path: "/v1/connect-flows/" + id})
	wantStatus(w.t, r, http.StatusOK)
	var b v1ConnectFlowBody
	r.json(&b)
	return b
}

func wantPage(t *testing.T, rec *httptest.ResponseRecorder, status int, title string) string {
	t.Helper()
	body := rec.Body.String()
	if rec.Code != status || !strings.Contains(body, "<h1>"+title+"</h1>") {
		t.Fatalf("callback: %d %s", rec.Code, body)
	}
	h := rec.Header()
	if h.Get("Content-Security-Policy") != callbackCSP || h.Get("Cache-Control") != "no-store" || h.Get("Referrer-Policy") != "no-referrer" ||
		!strings.HasPrefix(h.Get("Content-Type"), "text/html") {
		t.Fatalf("callback headers: %v", h)
	}
	if strings.Contains(body, "<script") || strings.Contains(body, "s3cret") {
		t.Fatalf("callback page: %s", body)
	}
	return body
}

// A flow: started by the signed-in owner, finished by the network
// sending them back (with or without the session), read by the tab that
// started it; the page and the API say only fixed words.
func TestV1ConnectAccount(t *testing.T) {
	w, _ := newConnectEnv(t)
	f, state := w.startFlow("https://net.example")
	u, _ := url.Parse(f.AuthorizeURL)
	if got := u.Query().Get("redirect_uri"); got != "https://pub.example/oauth/callback/"+apiConnectType {
		t.Fatalf("redirect URI %q", got)
	}
	if len(state) < 40 || time.Until(f.ExpiresAt) > 10*time.Minute || time.Until(f.ExpiresAt) < 9*time.Minute {
		t.Fatalf("flow: %+v", f)
	}
	if b := w.flow(f.Flow); b.Status != store.FlowPending || b.Type != apiConnectType {
		t.Fatalf("pending: %+v", b)
	}

	// Back with the session: connected, once; the same callback again (a
	// refresh) answers the same page and makes nothing more.
	body := wantPage(t, w.back(url.Values{"state": {state}, "code": {"c-tok-one"}}, w.cookie), http.StatusOK, "Connected")
	if strings.Contains(body, "tok-one") || strings.Contains(body, state) {
		t.Fatalf("the page quotes the callback: %s", body)
	}
	b := w.flow(f.Flow)
	if b.Status != store.FlowCompleted || b.Account == "" || b.Error != "" {
		t.Fatalf("completed: %+v", b)
	}
	wantStatus(t, w.do(req{method: "GET", path: "/v1/accounts/" + b.Account}), http.StatusOK)
	opens := w.openCount()
	wantPage(t, w.back(url.Values{"state": {state}, "code": {"c-tok-one"}}, w.cookie), http.StatusOK, "Connected")
	if w.openCount() != opens {
		t.Fatal("a second callback did the exchange again")
	}

	// Back without a session (a home-screen app's other browser): the
	// session gate lets the callback through, and the owner confirms the
	// account in the tab that started the flow.
	f2, state2 := w.startFlow("https://net.example")
	body = wantPage(t, w.back(url.Values{"state": {state2}, "code": {"c-tok-two"}}, nil), http.StatusOK, "Almost there")
	if !strings.Contains(body, "confirm the account") {
		t.Fatalf("confirm page: %s", body)
	}
	b = w.flow(f2.Flow)
	if b.Status != store.FlowChoose || len(b.Candidates) != 1 || b.Candidates[0].Handle == "" || b.Account != "" {
		t.Fatalf("without a session: %+v", b)
	}
	r := w.do(req{method: "POST", path: "/v1/connect-flows/" + f2.Flow + ":choose", body: `{"candidate":0}`})
	wantStatus(t, r, http.StatusOK)
	if r.json(&b); b.Status != store.FlowCompleted || b.Account == "" {
		t.Fatalf("confirmed: %+v", b)
	}
	// The same network account again: a duplicate.
	f3, state3 := w.startFlow("https://net.example")
	body = wantPage(t, w.back(url.Values{"state": {state3}, "code": {"c-tok-two"}}, w.cookie), http.StatusBadRequest, "Not connected")
	if !strings.Contains(body, upperFirst(connectText[destination.ConnectDuplicateAccount])+".") {
		t.Fatalf("duplicate page: %s", body)
	}
	if b := w.flow(f3.Flow); b.Error != destination.ConnectDuplicateAccount || b.ErrorDetail != connectText[destination.ConnectDuplicateAccount] {
		t.Fatalf("duplicate: %+v", b)
	}
}

// Each way a callback fails ends the flow with a fixed code, and nothing
// the network said reaches the page or the API.
func TestV1ConnectFailures(t *testing.T) {
	w, other := newConnectEnv(t)
	cases := []struct {
		name   string
		q      func(state string) url.Values
		cookie *http.Cookie
		code   string
	}{
		{"declined", func(s string) url.Values { return url.Values{"state": {s}, "error": {"access_denied"}} }, nil, destination.ConnectDenied},
		{"another error", func(s string) url.Values {
			return url.Values{"state": {s}, "error": {"server_error"}, "error_description": {"<b>client_secret=s3cret</b>"}}
		}, nil, destination.ConnectExchangeFailed},
		{"exchange refused", func(s string) url.Values { return url.Values{"state": {s}, "code": {"nope"}} }, nil, destination.ConnectExchangeFailed},
		{"another user's session", func(s string) url.Values { return url.Values{"state": {s}, "code": {"c-tok-x"}} }, other, destination.ConnectSessionMismatch},
	}
	for _, c := range cases {
		f, state := w.startFlow("https://net.example")
		body := wantPage(t, w.back(c.q(state), c.cookie), http.StatusBadRequest, "Not connected")
		if !strings.Contains(body, "<code>"+c.code+"</code>") {
			t.Errorf("%s: page %s", c.name, body)
		}
		b := w.flow(f.Flow)
		if b.Status != store.FlowFailed || b.Error != c.code || b.Account != "" {
			t.Errorf("%s: %+v", c.name, b)
		}
		if raw, _ := json.Marshal(b); strings.Contains(string(raw), "s3cret") {
			t.Errorf("%s: the flow quotes the network: %s", c.name, raw)
		}
	}
	// No such state, and none at all: a page, no flow.
	for _, q := range []url.Values{{"state": {"made-up"}, "code": {"c-x"}}, {"code": {"c-x"}}} {
		body := wantPage(t, w.back(q, nil), http.StatusBadRequest, "Not connected")
		if !strings.Contains(body, "<code>"+destination.ConnectStateInvalid+"</code>") {
			t.Errorf("unknown state: %s", body)
		}
	}
	// Another type's callback with this flow's state: not this flow.
	_, state := w.startFlow("https://net.example")
	r := httptest.NewRequest("GET", "/oauth/callback/mastodon?state="+url.QueryEscape(state)+"&code=c-y", nil)
	rec := httptest.NewRecorder()
	w.h.ServeHTTP(rec, r)
	wantPage(t, rec, http.StatusBadRequest, "Not connected")
	// The other user can't read the owner's flow.
	f, _ := w.startFlow("https://net.example")
	w.cookie, other = other, w.cookie
	wantProblem(t, w.do(req{method: "GET", path: "/v1/connect-flows/" + f.Flow}), http.StatusNotFound, codeNotFound)
	w.cookie = other
	wantProblem(t, w.do(req{method: "GET", path: "/v1/connect-flows/flw_nope"}), http.StatusNotFound, codeNotFound)
}

// Starting: fixed words when the network can't be prepared, the field
// when its server is private, and the rules of who may start one.
func TestV1ConnectStart(t *testing.T) {
	w, _ := newConnectEnv(t)
	p := wantProblem(t, w.do(req{method: "POST", path: "/v1/accounts:connect", body: `{"type":"apiconnect","config":{"server":"down"}}`}),
		http.StatusBadGateway, codeConnectFailed)
	if p.ConnectError != destination.ConnectExchangeFailed || strings.Contains(p.Detail, "s3cret") {
		t.Fatalf("down: %+v", p)
	}
	p = wantProblem(t, w.do(req{method: "POST", path: "/v1/accounts:connect", body: `{"type":"apiconnect","config":{"server":"blocked"}}`}),
		http.StatusUnprocessableEntity, codeValidation)
	if len(p.Errors) != 1 || p.Errors[0].Pointer != "/config/server" {
		t.Fatalf("blocked: %+v", p)
	}
	wantProblem(t, w.do(req{method: "POST", path: "/v1/accounts:connect", body: `{"type":"apiconnect","config":{}}`}),
		http.StatusUnprocessableEntity, codeValidation)
	wantProblem(t, w.do(req{method: "POST", path: "/v1/accounts:connect", body: `{"config":{"server":"x"}}`, badRequest: true}),
		http.StatusUnprocessableEntity, codeValidation)
	// A type that connects with typed credentials only.
	p = wantProblem(t, w.do(req{method: "POST", path: "/v1/accounts:connect", body: `{"type":"bluesky","config":{}}`}),
		http.StatusUnprocessableEntity, codeValidation)
	if p.Errors[0].Pointer != "/type" {
		t.Fatalf("bluesky: %+v", p)
	}
	// A token can't: it's a browser flow.
	tok := w.token(scopeAccountsWrite)
	wantProblem(t, w.do(req{method: "POST", path: "/v1/accounts:connect", auth: tok, body: `{"type":"apiconnect","config":{"server":"x"}}`}),
		http.StatusForbidden, codeSessionRequired)
	// Without the public address, the network can't send the owner back.
	w.api.AppBaseURL = ""
	p = wantProblem(t, w.do(req{method: "POST", path: "/v1/accounts:connect", body: `{"type":"apiconnect","config":{"server":"x"}}`}),
		http.StatusConflict, codeInvalidState)
	if !strings.Contains(p.Detail, "PUBLIC_URL") {
		t.Fatalf("no public address: %+v", p)
	}
	w.api.AppBaseURL = "https://pub.example"
	// At most 10 starts a minute (some were spent above).
	n := 0
	for ; n < 12; n++ {
		r := w.do(req{method: "POST", path: "/v1/accounts:connect", body: `{"type":"apiconnect","config":{"server":"x"}}`})
		if r.Code == http.StatusTooManyRequests {
			break
		}
		wantStatus(t, r, http.StatusCreated)
	}
	if n == 12 {
		t.Fatal("no limit")
	}
}

// Threads: the server's Meta app comes first, then an https public
// address (Meta's rule); then the sign-in goes to threads.com, without
// the app secret.
func TestV1ConnectThreads(t *testing.T) {
	w, _ := newConnectEnv(t)
	start := func() v1Resp {
		return w.do(req{method: "POST", path: "/v1/accounts:connect", body: `{"type":"threads"}`})
	}
	// The list says where Meta sends the owner back, and why signing in
	// can't start yet.
	method := func() destination.AuthMethod {
		t.Helper()
		var l struct{ Data []destination.Manifest }
		w.do(req{method: "GET", path: "/v1/destinations"}).json(&l)
		for _, m := range l.Data {
			for _, a := range m.Auth {
				if m.Type == "threads" && a.ID == "oauth" {
					return a
				}
				if a.Flow != destination.FlowRedirect && (a.RedirectURI != "" || a.Unavailable != "") {
					t.Errorf("%s %s: %+v", m.Type, a.ID, a)
				}
			}
		}
		t.Fatal("no Threads sign-in")
		return destination.AuthMethod{}
	}
	if a := method(); a.RedirectURI != "https://pub.example/oauth/callback/threads" || !strings.Contains(a.Unavailable, "Threads app") {
		t.Fatalf("listed: %+v", a)
	}
	p := wantProblem(t, start(), http.StatusUnprocessableEntity, codeValidation)
	if len(p.Errors) != 1 || p.Errors[0].Pointer != "/method" || !strings.Contains(p.Errors[0].Detail, "Threads app") {
		t.Fatalf("no app: %+v", p)
	}
	w.mu.Lock()
	w.instance = func(typ string) (map[string]any, map[string]string) {
		if typ != "threads" {
			return nil, nil
		}
		return map[string]any{"app_id": "1234567890"}, map[string]string{"app_secret": "meta-s3cret"}
	}
	w.mu.Unlock()
	if a := method(); a.Unavailable != "" {
		t.Fatalf("with the app: %+v", a)
	}
	w.api.AppBaseURL = ""
	if a := method(); a.RedirectURI != "" || !strings.Contains(a.Unavailable, "PUBLIC_URL") {
		t.Fatalf("without a public address: %+v", a)
	}
	w.api.AppBaseURL = "http://pub.example"
	if a := method(); !strings.Contains(a.Unavailable, "https") {
		t.Fatalf("http, listed: %+v", a)
	}
	p = wantProblem(t, start(), http.StatusUnprocessableEntity, codeValidation)
	if len(p.Errors) != 1 || p.Errors[0].Pointer != "/method" || !strings.Contains(p.Errors[0].Detail, "https") {
		t.Fatalf("http: %+v", p)
	}
	w.api.AppBaseURL = "https://pub.example"
	if a := method(); a.Unavailable != "" {
		t.Fatalf("ready, listed: %+v", a)
	}
	r := start()
	wantStatus(t, r, http.StatusCreated)
	var b v1ConnectStartBody
	r.json(&b)
	u, err := url.Parse(b.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Host != "threads.com" || q.Get("client_id") != "1234567890" ||
		q.Get("redirect_uri") != "https://pub.example/oauth/callback/threads" || q.Get("state") == "" ||
		strings.Contains(r.Body.String(), "meta-s3cret") {
		t.Fatalf("started: %s", r.Body.String())
	}
}

// Several accounts offered: the starting tab picks one; a reconnect must
// bring the same account back.
func TestV1ConnectChooseAndReconnect(t *testing.T) {
	w, _ := newConnectEnv(t)
	f, state := w.startFlow("https://net.example")
	wantPage(t, w.back(url.Values{"state": {state}, "code": {"many"}}, w.cookie), http.StatusOK, "Almost there")
	b := w.flow(f.Flow)
	if b.Status != store.FlowChoose || len(b.Candidates) != 2 || b.Candidates[1].Handle != "b" || b.Candidates[1].DisplayName != "Bee" {
		t.Fatalf("choose: %+v", b)
	}
	if raw, _ := json.Marshal(b); strings.Contains(string(raw), "pick-") {
		t.Fatalf("the flow shows the candidates' tokens: %s", raw)
	}
	choose := func(body string) v1Resp {
		return w.do(req{method: "POST", path: "/v1/connect-flows/" + f.Flow + ":choose", body: body})
	}
	wantProblem(t, choose(`{"candidate":5}`), http.StatusUnprocessableEntity, codeValidation)
	r := choose(`{"candidate":1}`)
	wantStatus(t, r, http.StatusOK)
	b = v1ConnectFlowBody{}
	r.json(&b)
	if b.Status != store.FlowCompleted || b.Account == "" || len(b.Candidates) != 0 {
		t.Fatalf("chosen: %+v", b)
	}
	acct := b.Account
	// Chosen once: again answers the flow as it stands.
	r = choose(`{"candidate":0}`)
	wantStatus(t, r, http.StatusOK)
	if r.json(&b); b.Account != acct {
		t.Fatalf("chosen twice: %+v", b)
	}

	reconnect := func() (v1ConnectStartBody, string) {
		r := w.do(req{method: "POST", path: "/v1/accounts/" + acct + ":reconnect"})
		wantStatus(t, r, http.StatusCreated)
		var s v1ConnectStartBody
		r.json(&s)
		u, _ := url.Parse(s.AuthorizeURL)
		return s, u.Query().Get("state")
	}
	s, st := reconnect()
	wantPage(t, w.back(url.Values{"state": {st}, "code": {"c-pick-b"}}, w.cookie), http.StatusOK, "Connected")
	if b := w.flow(s.Flow); b.Status != store.FlowCompleted || b.Account != acct {
		t.Fatalf("reconnected: %+v", b)
	}
	s, st = reconnect()
	wantPage(t, w.back(url.Values{"state": {st}, "code": {"c-someone-else"}}, w.cookie), http.StatusBadRequest, "Not connected")
	if b := w.flow(s.Flow); b.Error != destination.ConnectIdentityChanged {
		t.Fatalf("another account: %+v", b)
	}
	// An account from the environment isn't reconnected here.
	wantProblem(t, w.do(req{method: "POST", path: "/v1/accounts/" + w.acct("mastodon") + ":reconnect"}), http.StatusConflict, codeLockedByEnv)
	wantProblem(t, w.do(req{method: "POST", path: "/v1/accounts/acc_nope:reconnect"}), http.StatusNotFound, codeNotFound)
}

// connectConformance exercises the connect routes once each (for
// TestV1Conformance).
func connectConformance(t *testing.T) map[string]bool {
	t.Helper()
	w, _ := newConnectEnv(t)
	seen := map[string]bool{}
	call := func(pattern string, rq req, status int) v1Resp {
		t.Helper()
		r := w.do(rq)
		if r.Code != status {
			t.Errorf("%s: status %d, want %d; body %s", pattern, r.Code, status, r.Body.String())
		}
		seen[pattern] = true
		return r
	}
	var s v1ConnectStartBody
	call("POST /v1/accounts:connect", req{method: "POST", path: "/v1/accounts:connect",
		body: `{"type":"apiconnect","label":"Mine","config":{"server":"https://net.example"}}`}, 201).json(&s)
	call("GET /v1/connect-flows/{id}", req{method: "GET", path: "/v1/connect-flows/" + s.Flow}, 200)
	u, _ := url.Parse(s.AuthorizeURL)
	w.back(url.Values{"state": {u.Query().Get("state")}, "code": {"many"}}, w.cookie)
	var b v1ConnectFlowBody
	call("POST /v1/connect-flows/{id}:choose", req{method: "POST", path: "/v1/connect-flows/" + s.Flow + ":choose", body: `{"candidate":0}`}, 200).json(&b)
	call("POST /v1/accounts/{id}:reconnect", req{method: "POST", path: "/v1/accounts/" + b.Account + ":reconnect",
		body: `{"config":{"server":"https://net.example"}}`}, 201)
	return seen
}

// The callback is the network's redirect only: a HEAD (a link preview)
// doesn't use the sign-in.
func TestV1ConnectCallbackOnlyGET(t *testing.T) {
	w, _ := newConnectEnv(t)
	f, state := w.startFlow("https://net.example")
	r := httptest.NewRequest("HEAD", "/oauth/callback/"+apiConnectType+"?"+url.Values{"state": {state}, "code": {"c-tok-head"}}.Encode(), nil)
	rec := httptest.NewRecorder()
	w.h.ServeHTTP(rec, r)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET" {
		t.Fatalf("HEAD: %d %v", rec.Code, rec.Header())
	}
	if b := w.flow(f.Flow); b.Status != store.FlowPending {
		t.Fatalf("after HEAD: %+v", b)
	}
	wantPage(t, w.back(url.Values{"state": {state}, "code": {"c-tok-head"}}, w.cookie), http.StatusOK, "Connected")
}
