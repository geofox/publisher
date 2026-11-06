package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/httpx"
	"github.com/geofox/publisher/internal/localauth"
	"github.com/geofox/publisher/internal/localauth/webauthntest"
	"github.com/geofox/publisher/internal/store"
)

// testPublicURL is the local-mode harness's PUBLIC_URL.
const testPublicURL = "https://app.example"

// localEnv is a v1Env in local mode (AUTH_MODE=local, PUBLIC_URL set) with
// a software passkey authenticator and a cookie jar for the /auth and
// /setup routes. Nobody is signed in until setup runs.
type localEnv struct {
	*v1Env
	authn *webauthntest.Authenticator
	jar   map[string]*http.Cookie
	// addr is the client address requests come from.
	addr string
}

func newLocalEnv(t *testing.T) *localEnv {
	t.Helper()
	st, err := storeOpen(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.SeedEnvAccounts(context.Background(), store.DefaultOwner,
		[]store.EnvAccount{{Type: "bluesky", Label: "Bluesky"}, {Type: "mastodon", Label: "Mastodon"}}); err != nil {
		t.Fatal(err)
	}
	svc, err := localauth.New(st, testPublicURL)
	if err != nil {
		t.Fatal(err)
	}
	a := &API{Store: st, Version: "v9.9.9-test", UserLanguages: []string{"en"}, Mode: ModeLocal, Local: svc,
		Clients: &httpx.ClientAddrs{}, PublicURL: testPublicURL, AppBaseURL: testPublicURL, SessionTTL: time.Hour}
	e := &localEnv{v1Env: &v1Env{t: t, api: a, st: st}, authn: webauthntest.New(testPublicURL),
		jar: map[string]*http.Cookie{}, addr: "192.0.2.10:4000"}
	e.h = a.Routes()
	return e
}

// send makes a request to any route, from PUBLIC_URL's pages (Origin) as
// JSON, with the jar's cookies (and the session's), and keeps the cookies
// it sets. hdr overrides headers ("" deletes one).
func (e *localEnv) send(method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = e.addr
	r.Header.Set("Origin", testPublicURL)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for _, c := range e.jar {
		r.AddCookie(c)
	}
	for k, v := range hdr {
		if v == "" {
			r.Header.Del(k)
		} else {
			r.Header.Set(k, v)
		}
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	e.keep(rec)
	return rec
}

// keep stores the cookies a response set (or cleared).
func (e *localEnv) keep(rec *httptest.ResponseRecorder) {
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(e.jar, c.Name)
		} else {
			e.jar[c.Name] = &http.Cookie{Name: c.Name, Value: c.Value}
		}
	}
	if c, ok := e.jar[sessionCookie]; ok {
		e.cookie = c
	} else {
		e.cookie = nil
	}
}

// v1 is e.do (checked against the OpenAPI document) with the jar's
// cookies, keeping the cookies it sets.
func (e *localEnv) v1(rq req) v1Resp {
	e.t.Helper()
	rq.header = with(rq.header, map[string]string{"Cookie": e.cookieHeader()})
	rq.auth = "none"
	r := e.do(rq)
	e.keep(r.ResponseRecorder)
	return r
}

// cookieHeader is the jar as a Cookie header (for e.do, which sends only
// the session cookie by itself).
func (e *localEnv) cookieHeader() string {
	var parts []string
	for _, c := range e.jar {
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

func wantCode(t *testing.T, rec *httptest.ResponseRecorder, status int) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status %d, want %d; body %s", rec.Code, status, rec.Body.String())
	}
}

// setupLink mints a setup token (what the log's setup link carries).
func (e *localEnv) setupLink() string {
	e.t.Helper()
	tok, _, err := e.st.MintSetupToken(context.Background(), localauth.SetupTokenTTL)
	if err != nil {
		e.t.Fatal(err)
	}
	return tok
}

// exchange trades a setup token for the setup cookie.
func (e *localEnv) exchange(tok string) *httptest.ResponseRecorder {
	b, _ := json.Marshal(map[string]string{"token": tok})
	return e.send("POST", "/setup/exchange", string(b), nil)
}

// setupWithPasskey runs first-run setup with a new passkey and leaves the
// owner signed in.
func (e *localEnv) setupWithPasskey() *webauthntest.Credential {
	e.t.Helper()
	wantCode(e.t, e.exchange(e.setupLink()), http.StatusNoContent)
	rec := e.send("POST", "/setup/passkey/begin", "{}", nil)
	wantCode(e.t, rec, http.StatusOK)
	cred, c, err := e.authn.Create(rec.Body.Bytes())
	if err != nil {
		e.t.Fatal(err)
	}
	rec = e.send("POST", "/setup/passkey/finish", finishBody("Laptop", cred), nil)
	wantCode(e.t, rec, http.StatusNoContent)
	if e.cookie == nil {
		e.t.Fatal("setup didn't sign in")
	}
	return c
}

// setupWithPassword runs first-run setup with a password.
func (e *localEnv) setupWithPassword(pw string) {
	e.t.Helper()
	wantCode(e.t, e.exchange(e.setupLink()), http.StatusNoContent)
	b, _ := json.Marshal(map[string]string{"password": pw})
	wantCode(e.t, e.send("POST", "/setup/password", string(b), nil), http.StatusNoContent)
}

func finishBody(name string, cred json.RawMessage) string {
	b, _ := json.Marshal(map[string]any{"name": name, "credential": cred})
	return string(b)
}

// signInWithPasskey signs in afresh with the authenticator's passkey.
func (e *localEnv) signInWithPasskey() *httptest.ResponseRecorder {
	e.t.Helper()
	rec := e.send("POST", "/auth/passkey/begin", "{}", nil)
	wantCode(e.t, rec, http.StatusOK)
	a, err := e.authn.Get(rec.Body.Bytes())
	if err != nil {
		e.t.Fatal(err)
	}
	return e.send("POST", "/auth/passkey/finish", string(a), nil)
}

// reauthPasskey re-authenticates with the passkey for op and returns the
// header carrying the nonce.
func (e *localEnv) reauthPasskey(op string) map[string]string {
	e.t.Helper()
	rec := e.send("POST", "/auth/reauth/begin", `{"operation":"`+op+`"}`, nil)
	wantCode(e.t, rec, http.StatusOK)
	a, err := e.authn.Get(rec.Body.Bytes())
	if err != nil {
		e.t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"operation": op, "credential": a})
	rec = e.send("POST", "/auth/reauth", string(b), nil)
	wantCode(e.t, rec, http.StatusOK)
	var n reauthNonceBody
	if err := json.Unmarshal(rec.Body.Bytes(), &n); err != nil || n.Nonce == "" || n.Operation != op {
		e.t.Fatalf("reauth answer %s (%v)", rec.Body.String(), err)
	}
	return map[string]string{reauthHeader: n.Nonce}
}

// reauthPassword re-authenticates with the password for op.
func (e *localEnv) reauthPassword(op, pw string) *httptest.ResponseRecorder {
	b, _ := json.Marshal(map[string]any{"operation": op, "password": pw})
	return e.send("POST", "/auth/reauth", string(b), nil)
}

// with merges header maps.
func with(ms ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range ms {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// localConformance exercises /v1/me's sign-in routes in local mode (with
// OIDC they answer 409) against the OpenAPI document and returns the
// patterns it called.
func localConformance(t *testing.T) map[string]bool {
	t.Helper()
	e := newLocalEnv(t)
	e.setupWithPasskey()
	seen := map[string]bool{}
	call := func(pattern string, rq req, status int) v1Resp {
		t.Helper()
		r := e.v1(rq)
		if r.Code != status {
			t.Errorf("%s: status %d, want %d; body %s", pattern, r.Code, status, r.Body.String())
		}
		seen[pattern] = true
		return r
	}
	call("GET /v1/me/sign-in", req{method: "GET", path: "/v1/me/sign-in"}, 200)
	r := call("POST /v1/me/passkeys:begin", req{method: "POST", path: "/v1/me/passkeys:begin",
		header: e.reauthPasskey(opSignInAdd)}, 200)
	cred, _, err := webauthntest.New(testPublicURL).Create(r.Body.Bytes()) // another device
	if err != nil {
		t.Fatal(err)
	}
	r = call("POST /v1/me/passkeys", req{method: "POST", path: "/v1/me/passkeys", body: finishBody("Phone", cred)}, 201)
	var pk v1PasskeyBody
	r.json(&pk)
	call("PATCH /v1/me/passkeys/{id}", req{method: "PATCH", path: "/v1/me/passkeys/" + pk.ID, body: `{"name":"Work phone"}`}, 200)
	call("PUT /v1/me/password", req{method: "PUT", path: "/v1/me/password", body: `{"password":"correct horse battery"}`,
		header: e.reauthPasskey(opPasswordSet)}, 204)
	call("DELETE /v1/me/password", req{method: "DELETE", path: "/v1/me/password", header: e.reauthPasskey(opSignInRemove)}, 204)
	call("DELETE /v1/me/passkeys/{id}", req{method: "DELETE", path: "/v1/me/passkeys/" + pk.ID,
		header: e.reauthPasskey(opSignInRemove)}, 204)
	u, err := e.st.LocalUser(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.CreateSessionWith(u.ID, time.Hour, store.SessionMeta{Method: "passkey", UserAgent: "Phone", Addr: "192.0.2.20"}); err != nil {
		t.Fatal(err)
	}
	r = call("GET /v1/me/sessions", req{method: "GET", path: "/v1/me/sessions"}, 200)
	var list struct{ Data []v1SessionBody }
	r.json(&list)
	other := ""
	for _, s := range list.Data {
		if !s.Current {
			other = s.ID
		}
	}
	if len(list.Data) != 2 || other == "" {
		t.Fatalf("sessions = %+v", list.Data)
	}
	call("DELETE /v1/me/sessions/{id}", req{method: "DELETE", path: "/v1/me/sessions/" + other}, 204)
	call("GET /v1/me/sign-in-events", req{method: "GET", path: "/v1/me/sign-in-events"}, 200)
	return seen
}
