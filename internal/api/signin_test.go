package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/auth"
	"github.com/geofox/publisher/internal/localauth/webauthntest"
	"github.com/geofox/publisher/internal/store"
)

// ─── modes ──────────────────────────────────────────────────────────────

// get makes an unauthenticated request to h; navigate marks it a page load.
func rawReq(h http.Handler, method, path string, navigate bool, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if navigate {
		r.Header.Set("Sec-Fetch-Mode", "navigate")
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// TestModeGates: what someone without a session gets, per mode.
func TestModeGates(t *testing.T) {
	proxy := newV1Env(t, false)
	proxy.api.Mode = ModeProxy
	proxy.h = proxy.api.Routes()
	oidc := newV1Env(t, true)
	local := newLocalEnv(t)
	token := `{"name":"x","scopes":["admin"]}`
	for _, c := range []struct {
		name     string
		h        http.Handler
		method   string
		path     string
		navigate bool
		body     string
		status   int
		has      string
	}{
		{"proxy page", proxy.h, "GET", "/", true, "", 200, "main.js"},
		{"proxy api", proxy.h, "GET", "/v1/accounts", false, "", 200, ""},
		{"proxy admin token", proxy.h, "POST", "/v1/tokens", false, token, 403, "sign_in_required"},
		{"proxy setup", proxy.h, "GET", "/setup", true, "", 404, ""},
		{"proxy methods", proxy.h, "GET", "/auth/methods", false, "", 404, ""},

		{"oidc page", oidc.h, "GET", "/", true, "", 302, ""},
		{"oidc api", oidc.h, "GET", "/v1/accounts", false, "", 401, ""},
		{"oidc token", oidc.h, "POST", "/v1/tokens", false, token, 401, ""},
		{"oidc setup", oidc.h, "GET", "/setup", true, "", 302, ""},
		{"oidc methods", oidc.h, "GET", "/auth/methods", false, "", 404, ""},

		{"local page", local.h, "GET", "/", true, "", 200, "signin.js"},
		{"local deep page", local.h, "GET", "/history/x", true, "", 200, "signin.js"},
		{"local app script", local.h, "GET", "/main.js", false, "", 401, ""},
		{"local api", local.h, "GET", "/v1/accounts", false, "", 401, ""},
		{"local token", local.h, "POST", "/v1/tokens", false, token, 401, ""},
		{"local setup", local.h, "GET", "/setup", true, "", 200, "setup.js"},
		{"local methods", local.h, "GET", "/auth/methods", false, "", 200, `"setup":true`},
		{"local sign-in asset", local.h, "GET", "/webauthn.js", false, "", 200, "navigator.credentials"},
		{"local path trick", local.h, "GET", "/setup/../v1/accounts", false, "", 307, ""}, // cleaned and redirected, never served
	} {
		rec := rawReq(c.h, c.method, c.path, c.navigate, c.body)
		if rec.Code != c.status || (c.has != "" && !strings.Contains(rec.Body.String(), c.has)) {
			t.Errorf("%s: %s %s = %d (want %d, containing %q): %.200s", c.name, c.method, c.path, rec.Code, c.status, c.has, rec.Body.String())
		}
	}
}

// TestSessionsBelongToTheirMode: a local session doesn't count with OIDC,
// nor an OIDC one in local mode.
func TestSessionsBelongToTheirMode(t *testing.T) {
	e := newLocalEnv(t)
	e.setupWithPassword("a long enough passphrase")
	localCookie := e.cookie
	u, err := e.st.UpsertUser("sub-1", "ann@example.com", "Ann")
	if err != nil {
		t.Fatal(err)
	}
	oidcRaw, err := e.st.CreateSession(u.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	get := func(h http.Handler, c *http.Cookie) int {
		r := httptest.NewRequest("GET", "/v1/me/sessions", nil)
		r.AddCookie(c)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}
	if code := get(e.h, localCookie); code != 200 {
		t.Errorf("local session in local mode = %d", code)
	}
	if code := get(e.h, &http.Cookie{Name: sessionCookie, Value: oidcRaw}); code != 401 {
		t.Errorf("OIDC session in local mode = %d, want 401", code)
	}
	au, err := auth.New(context.Background(), auth.Config{Issuer: newMockOIDC(t), ClientID: "client-1", ClientSecret: "s", RedirectURL: "https://app/auth/callback"})
	if err != nil {
		t.Fatal(err)
	}
	o := &API{Store: e.st, Mode: ModeOIDC, Auth: au, Allowlist: auth.NewAllowlist([]string{"sub-1", store.LocalSubject}, nil), SessionTTL: time.Hour}
	oh := o.Routes()
	if code := get(oh, localCookie); code != 401 {
		t.Errorf("local session in OIDC mode = %d, want 401", code)
	}
	if code := get(oh, &http.Cookie{Name: sessionCookie, Value: oidcRaw}); code != 200 {
		t.Errorf("OIDC session in OIDC mode = %d", code)
	}
	// An OIDC login can't take over the local owner's row either.
	if _, err := e.st.UpsertUser(store.LocalSubject, "", ""); err == nil {
		t.Error("an OIDC subject equal to the local owner's updated the local row")
	}
}

// ─── passkeys ───────────────────────────────────────────────────────────

func TestPasskeySignIn(t *testing.T) {
	e := newLocalEnv(t)
	e.setupWithPasskey()
	setupSession := e.cookie.Value
	rec := e.signInWithPasskey()
	wantCode(t, rec, http.StatusNoContent)
	if e.cookie == nil || e.cookie.Value == setupSession {
		t.Fatal("signing in didn't start a new session")
	}
	if _, _, err := e.st.SessionByRaw(setupSession); err == nil {
		t.Error("the session the browser had before signing in survived")
	}
	r := e.v1(req{method: "GET", path: "/v1/me/sessions"})
	var list struct{ Data []v1SessionBody }
	r.json(&list)
	if len(list.Data) != 1 || list.Data[0].Method != "passkey" || list.Data[0].Addr != "192.0.2.10" || !list.Data[0].Current {
		t.Errorf("sessions = %+v", list.Data)
	}
}

func TestPasskeyRefusals(t *testing.T) {
	e := newLocalEnv(t)
	cred := e.setupWithPasskey()
	begin := func() []byte {
		t.Helper()
		rec := e.send("POST", "/auth/passkey/begin", "{}", nil)
		wantCode(t, rec, http.StatusOK)
		return rec.Body.Bytes()
	}
	finish := func(body []byte) int { return e.send("POST", "/auth/passkey/finish", string(body), nil).Code }

	// Another origin's "browser".
	evil := &webauthntest.Authenticator{Origin: "https://evil.example", Creds: e.authn.Creds}
	a, err := evil.Get(begin())
	if err != nil {
		t.Fatal(err)
	}
	if code := finish(a); code != http.StatusUnauthorized {
		t.Errorf("wrong origin = %d, want 401", code)
	}
	// Signed for another relying party.
	opts := begin()
	a, _ = e.authn.Sign(cred, webauthntest.Challenge(opts), "evil.example")
	if code := finish(a); code != http.StatusUnauthorized {
		t.Errorf("wrong relying party = %d, want 401", code)
	}
	// A replayed assertion: its ceremony is spent.
	opts = begin()
	a, _ = e.authn.Get(opts)
	ceremony := e.jar[ceremonyCookie]
	if code := finish(a); code != http.StatusNoContent {
		t.Fatalf("good assertion = %d", code)
	}
	e.jar[ceremonyCookie] = ceremony
	if code := finish(a); code == http.StatusNoContent {
		t.Error("a replayed assertion signed in")
	}
	// A ceremony begun for sign-in can't re-authenticate.
	opts = begin()
	a, _ = e.authn.Get(opts)
	b, _ := json.Marshal(map[string]any{"operation": opSignInAdd, "credential": json.RawMessage(a)})
	if rec := e.send("POST", "/auth/reauth", string(b), nil); rec.Code == http.StatusOK {
		t.Error("a sign-in ceremony re-authenticated")
	}
	// No user verification.
	e.authn.NoUV = true
	a, _ = e.authn.Get(begin())
	if code := finish(a); code != http.StatusUnauthorized {
		t.Errorf("without user verification = %d, want 401", code)
	}
	e.authn.NoUV = false
	// A counter that stops moving forward (a cloned authenticator).
	a, _ = e.authn.Get(begin())
	if code := finish(a); code != http.StatusNoContent {
		t.Fatalf("good assertion = %d", code)
	}
	e.authn.FreezeCounter = true
	a, _ = e.authn.Get(begin())
	if code := finish(a); code != http.StatusUnauthorized {
		t.Errorf("a counter that didn't move = %d, want 401", code)
	}
	evs, _ := e.st.SignInEvents(context.Background(), store.DefaultOwner, 50)
	failed := 0
	for _, ev := range evs {
		if ev.Type == store.EventLoginFailed && ev.Data.Method == "passkey" {
			failed++
		}
	}
	if failed < 4 {
		t.Errorf("%d failed passkey sign-ins recorded, want at least 4", failed)
	}
}

// ─── password and throttling ────────────────────────────────────────────

const testPassword = "a long enough passphrase"

func pwBody(pw string) string {
	b, _ := json.Marshal(map[string]string{"password": pw})
	return string(b)
}

func TestPasswordSignInAndThrottle(t *testing.T) {
	e := newLocalEnv(t)
	e.setupWithPasskey()
	wantCode(t, e.v1(req{method: "PUT", path: "/v1/me/password", body: pwBody(testPassword), header: e.reauthPasskey(opPasswordSet)}).ResponseRecorder, 204)
	wantCode(t, e.send("POST", "/auth/password", pwBody(testPassword), nil), http.StatusNoContent)
	for i := 0; i < 5; i++ {
		rec := e.send("POST", "/auth/password", pwBody("wrong password "+strconv.Itoa(i)), nil)
		wantCode(t, rec, http.StatusUnauthorized)
	}
	rec := e.send("POST", "/auth/password", pwBody(testPassword), nil)
	wantCode(t, rec, http.StatusTooManyRequests)
	if ra, _ := strconv.Atoi(rec.Header().Get("Retry-After")); ra < 1 {
		t.Errorf("Retry-After = %q", rec.Header().Get("Retry-After"))
	}
	// Another address isn't throttled, nor is a passkey here.
	e.addr = "198.51.100.7:1"
	wantCode(t, e.send("POST", "/auth/password", pwBody(testPassword), nil), http.StatusNoContent)
	e.addr = "192.0.2.10:4000"
	wantCode(t, e.signInWithPasskey(), http.StatusNoContent)
}

func TestLocalPostsNeedOriginAndJSON(t *testing.T) {
	e := newLocalEnv(t)
	e.setupWithPassword(testPassword)
	for _, c := range []struct {
		name string
		hdr  map[string]string
		code int
	}{
		{"another origin", map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
		{"no origin", map[string]string{"Origin": ""}, http.StatusForbidden},
		{"null origin", map[string]string{"Origin": "null"}, http.StatusForbidden},
		{"a form", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, http.StatusUnsupportedMediaType},
		{"text", map[string]string{"Content-Type": "text/plain"}, http.StatusUnsupportedMediaType},
	} {
		if rec := e.send("POST", "/auth/password", pwBody(testPassword), c.hdr); rec.Code != c.code {
			t.Errorf("%s: %d, want %d: %s", c.name, rec.Code, c.code, rec.Body.String())
		}
	}
	// Sign-out answers JSON to the app's fetch, and refuses another origin.
	if rec := e.send("POST", "/auth/logout", "", map[string]string{"Origin": "https://evil.example"}); rec.Code != http.StatusForbidden {
		t.Errorf("cross-site sign-out = %d", rec.Code)
	}
	rec := e.send("POST", "/auth/logout", "", map[string]string{"Accept": "application/json"})
	wantCode(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"redirect":"/"`) || e.cookie != nil {
		t.Errorf("sign-out = %s, cookie %v", rec.Body.String(), e.cookie)
	}
}

// ─── setup ──────────────────────────────────────────────────────────────

func TestSetupToken(t *testing.T) {
	e := newLocalEnv(t)
	tok := e.setupLink()
	wantCode(t, e.exchange(tok), http.StatusNoContent)
	delete(e.jar, setupCookie)
	wantCode(t, e.exchange(tok), http.StatusUnauthorized) // single use
	wantCode(t, e.exchange("nope"), http.StatusUnauthorized)
	// Without the setup cookie, no first credential.
	wantCode(t, e.send("POST", "/setup/password", pwBody(testPassword), nil), http.StatusUnauthorized)
	// A newer link makes older ones useless.
	old := e.setupLink()
	_ = e.setupLink()
	wantCode(t, e.exchange(old), http.StatusUnauthorized)
	// A weak password is refused.
	wantCode(t, e.exchange(e.setupLink()), http.StatusNoContent)
	wantCode(t, e.send("POST", "/setup/password", pwBody("short"), nil), http.StatusUnprocessableEntity)
}

func TestSetupRaceMakesOneCredential(t *testing.T) {
	e := newLocalEnv(t)
	type browser struct{ jar map[string]*http.Cookie }
	var bs []browser
	for i := 0; i < 2; i++ {
		e.jar = map[string]*http.Cookie{}
		wantCode(t, e.exchange(e.setupLink()), http.StatusNoContent)
		bs = append(bs, browser{jar: e.jar})
	}
	// The second exchange's link replaced the first's, but the first's
	// setup state is already held: both may finish; only one may win.
	codes := make([]int, len(bs))
	var wg sync.WaitGroup
	for i, b := range bs {
		wg.Add(1)
		go func(i int, b browser) {
			defer wg.Done()
			r := httptest.NewRequest("POST", "/setup/password", strings.NewReader(pwBody(testPassword+strconv.Itoa(i))))
			r.Header.Set("Origin", testPublicURL)
			r.Header.Set("Content-Type", "application/json")
			for _, c := range b.jar {
				r.AddCookie(c)
			}
			rec := httptest.NewRecorder()
			e.h.ServeHTTP(rec, r)
			codes[i] = rec.Code
		}(i, b)
	}
	wg.Wait()
	ok := 0
	for _, c := range codes {
		if c == http.StatusNoContent {
			ok++
		} else if c != http.StatusGone && c != http.StatusUnauthorized {
			t.Errorf("a losing setup answered %d", c)
		}
	}
	if ok != 1 {
		t.Fatalf("%d setups succeeded (%v), want 1", ok, codes)
	}
	u, _ := e.st.LocalUser(context.Background())
	if _, err := e.st.PasswordPHC(context.Background(), u.ID); err != nil {
		t.Fatal(err)
	}
	// Setup is closed.
	for _, p := range []string{"/setup/exchange", "/setup/password", "/setup/passkey/begin"} {
		if rec := e.send("POST", p, "{}", nil); rec.Code != http.StatusGone {
			t.Errorf("POST %s after setup = %d, want 410", p, rec.Code)
		}
	}
	if rec := rawReq(e.h, "GET", "/setup", true, ""); rec.Code != http.StatusGone {
		t.Errorf("GET /setup after setup = %d, want 410", rec.Code)
	}
}

// ─── re-authentication ──────────────────────────────────────────────────

func TestReauthNonces(t *testing.T) {
	e := newLocalEnv(t)
	e.setupWithPasskey()
	add := func(h map[string]string) int {
		return e.v1(req{method: "POST", path: "/v1/me/passkeys:begin", header: h}).Code
	}
	if p := e.v1(req{method: "POST", path: "/v1/me/passkeys:begin"}).problem(); p.Code != codeReauthRequired || p.Operation != opSignInAdd {
		t.Errorf("without a nonce: %+v", p)
	}
	h := e.reauthPasskey(opSignInAdd)
	if code := add(h); code != 200 {
		t.Fatalf("with a nonce = %d", code)
	}
	if code := add(h); code != 403 {
		t.Errorf("the nonce again = %d, want 403", code)
	}
	if code := add(e.reauthPasskey(opSignInRemove)); code != 403 {
		t.Errorf("another operation's nonce = %d, want 403", code)
	}
	// Another session's nonce.
	h = e.reauthPasskey(opSignInAdd)
	wantCode(t, e.signInWithPasskey(), http.StatusNoContent)
	if code := add(h); code != 403 {
		t.Errorf("another session's nonce = %d, want 403", code)
	}
	// An expired one.
	raw, _, err := e.st.CreateReauthNonce(context.Background(), store.SessionHash(e.cookie.Value), opSignInAdd, "", -time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if code := add(map[string]string{reauthHeader: raw}); code != 403 {
		t.Errorf("an expired nonce = %d, want 403", code)
	}
	// The password, wrong then right.
	wantCode(t, e.v1(req{method: "PUT", path: "/v1/me/password", body: pwBody(testPassword), header: e.reauthPasskey(opPasswordSet)}).ResponseRecorder, 204)
	rec := e.reauthPassword(opSignInAdd, "not it at all")
	wantCode(t, rec, http.StatusUnprocessableEntity)
	rec = e.reauthPassword(opSignInAdd, testPassword)
	wantCode(t, rec, http.StatusOK)
	// An unknown operation.
	wantCode(t, e.reauthPassword("everything", testPassword), http.StatusUnprocessableEntity)
	// No session.
	e.jar = map[string]*http.Cookie{}
	wantCode(t, e.reauthPassword(opSignInAdd, testPassword), http.StatusUnauthorized)
}

func TestPrivilegedTokens(t *testing.T) {
	e := newLocalEnv(t)
	e.setupWithPasskey()
	create := func(scopes string, h map[string]string, auth string) v1Resp {
		rq := req{method: "POST", path: "/v1/tokens", body: `{"name":"t","scopes":[` + scopes + `]}`, header: h}
		if auth != "" {
			rq.auth = auth
			return e.do(rq)
		}
		return e.v1(rq)
	}
	wantProblem(t, create(`"admin"`, nil, ""), 403, codeReauthRequired)
	wantProblem(t, create(`"accounts:write"`, nil, ""), 403, codeReauthRequired)
	wantStatus(t, create(`"posts:write"`, nil, ""), 201)
	wantStatus(t, create(`"admin"`, e.reauthPasskey(opTokenPrivileged), ""), 201)
	admin := e.token("admin")
	wantProblem(t, create(`"admin"`, nil, admin), 403, codeSessionRequired)
	wantStatus(t, create(`"posts:read"`, nil, admin), 201)
	evs, _ := e.st.SignInEvents(context.Background(), store.DefaultOwner, 50)
	n := 0
	for _, ev := range evs {
		if ev.Type == store.EventTokenCreated {
			n++
		}
	}
	if n != 3 {
		t.Errorf("%d token.created events, want 3", n)
	}
}

// ─── /v1/me ─────────────────────────────────────────────────────────────

func TestSignInMethodsLifecycle(t *testing.T) {
	e := newLocalEnv(t)
	cred := e.setupWithPasskey()
	_ = cred
	pk := func() string {
		var si v1SignInBody
		e.v1(req{method: "GET", path: "/v1/me/sign-in"}).json(&si)
		if len(si.Passkeys) == 0 {
			return ""
		}
		return si.Passkeys[0].ID
	}
	first := pk()
	// The last way to sign in can't go.
	wantProblem(t, e.v1(req{method: "DELETE", path: "/v1/me/passkeys/" + first, header: e.reauthPasskey(opSignInRemove)}), 409, codeLastSignInMethod)
	wantProblem(t, e.v1(req{method: "DELETE", path: "/v1/me/password", header: e.reauthPasskey(opSignInRemove)}), 404, codeNotFound)
	// A password; changing it needs the current one; it signs others out.
	wantStatus(t, e.v1(req{method: "PUT", path: "/v1/me/password", body: pwBody(testPassword), header: e.reauthPasskey(opPasswordSet)}), 204)
	other, err := e.st.CreateSessionWith(mustLocalUser(t, e).ID, time.Hour, store.SessionMeta{Method: "password"})
	if err != nil {
		t.Fatal(err)
	}
	wantProblem(t, e.v1(req{method: "PUT", path: "/v1/me/password", body: pwBody("another long passphrase")}), 422, codeValidation)
	wrong := `{"password":"another long passphrase","current_password":"nope nope nope"}`
	wantProblem(t, e.v1(req{method: "PUT", path: "/v1/me/password", body: wrong, header: e.reauthPasskey(opPasswordSet)}), 422, codeValidation)
	right := `{"password":"another long passphrase","current_password":"` + testPassword + `"}`
	wantStatus(t, e.v1(req{method: "PUT", path: "/v1/me/password", body: right, header: e.reauthPasskey(opPasswordSet)}), 204)
	if _, _, err := e.st.SessionByRaw(other); err == nil {
		t.Error("changing the password left another session signed in")
	}
	if e.cookie == nil {
		t.Fatal("changing the password signed the caller out")
	}
	// Now the passkey can go (the password remains), and a token can't
	// touch any of it.
	wantStatus(t, e.v1(req{method: "DELETE", path: "/v1/me/passkeys/" + first, header: e.reauthPassword2(opSignInRemove)}), 204)
	wantProblem(t, e.do(req{method: "GET", path: "/v1/me/sign-in", auth: e.token("admin")}), 403, codeSessionRequired)
	// Renaming: cleaned, bounded.
	wantProblem(t, e.v1(req{method: "PATCH", path: "/v1/me/passkeys/pk_nope", body: `{"name":"x"}`}), 404, codeNotFound)
}

func mustLocalUser(t *testing.T, e *localEnv) store.User {
	t.Helper()
	u, err := e.st.LocalUser(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// reauthPassword2 re-authenticates with the current test password
// ("another long passphrase") and returns the nonce header.
func (e *localEnv) reauthPassword2(op string) map[string]string {
	e.t.Helper()
	rec := e.reauthPassword(op, "another long passphrase")
	wantCode(e.t, rec, http.StatusOK)
	var n reauthNonceBody
	_ = json.Unmarshal(rec.Body.Bytes(), &n)
	return map[string]string{reauthHeader: n.Nonce}
}

func TestSignInEventsStayOutOfTheStream(t *testing.T) {
	e := newLocalEnv(t)
	e.setupWithPassword(testPassword)
	wantCode(t, e.send("POST", "/auth/password", pwBody("wrong wrong wrong"), nil), 401)
	evs, err := e.st.EventsSince(context.Background(), store.EventFilter{OwnerID: store.DefaultOwner, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		if strings.HasPrefix(ev.Type, "login.") || strings.HasPrefix(ev.Type, "signin.") || ev.Type == store.EventTokenCreated {
			t.Errorf("sign-in event %q in the /v1/events stream", ev.Type)
		}
	}
	r := e.v1(req{method: "GET", path: "/v1/me/sign-in-events"})
	var list struct{ Data []v1SignInEventBody }
	r.json(&list)
	types := []string{}
	for _, ev := range list.Data {
		types = append(types, ev.Type)
	}
	want := []string{store.EventLoginFailed, store.EventLoginSucceeded, store.EventSignInAdded}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Errorf("sign-in events = %v, want %v (newest first)", types, want)
	}
	if list.Data[0].Addr != "192.0.2.10" || list.Data[0].Method != "password" {
		t.Errorf("failed sign-in event = %+v", list.Data[0])
	}
}

// ─── OIDC re-authentication ─────────────────────────────────────────────

func TestOIDCReauth(t *testing.T) {
	var mu sync.Mutex
	claims := map[string]any{}
	issuer := newMockOIDCWith(t, func() map[string]any {
		mu.Lock()
		defer mu.Unlock()
		out := map[string]any{}
		for k, v := range claims {
			out[k] = v
		}
		return out
	})
	e := newV1Env(t, false)
	au, err := auth.New(context.Background(), auth.Config{Issuer: issuer, ClientID: "client-1", ClientSecret: "s", RedirectURL: "https://app/auth/callback"})
	if err != nil {
		t.Fatal(err)
	}
	e.api.Mode, e.api.Auth, e.api.Allowlist, e.api.SessionTTL = ModeOIDC, au, auth.NewAllowlist([]string{"sub-1", "sub-2"}, nil), time.Hour
	e.h = e.api.Routes()
	u, _ := e.st.UpsertUser("sub-1", "", "")
	raw, _ := e.st.CreateSession(u.ID, time.Hour)
	e.cookie = &http.Cookie{Name: sessionCookie, Value: raw}

	call := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.AddCookie(e.cookie)
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, r)
		return rec
	}
	// start answers the identity provider's URL; login completes it with
	// the given claims.
	start := func() url.Values {
		rec := call("GET", "/auth/reauth/oidc?operation="+opTokenPrivileged, "")
		if rec.Code != http.StatusFound {
			t.Fatalf("start = %d %s", rec.Code, rec.Body.String())
		}
		loc, _ := url.Parse(rec.Header().Get("Location"))
		q := loc.Query()
		if q.Get("prompt") != "login" || q.Get("max_age") != "0" {
			t.Errorf("the login isn't forced fresh: %s", loc)
		}
		return q
	}
	login := func(q url.Values, c map[string]any) *httptest.ResponseRecorder {
		mu.Lock()
		claims = map[string]any{"nonce": q.Get("nonce")}
		for k, v := range c {
			claims[k] = v
		}
		mu.Unlock()
		return call("GET", "/auth/callback?code=x&state="+url.QueryEscape(q.Get("state")), "")
	}
	claim := func() *httptest.ResponseRecorder {
		return call("POST", "/auth/reauth/claim", `{"operation":"`+opTokenPrivileged+`"}`)
	}
	now := time.Now().Unix()
	for _, c := range []struct {
		name   string
		claims map[string]any
	}{
		{"no auth_time", map[string]any{"auth_time": nil}},
		{"a stale auth_time", map[string]any{"auth_time": now - 3600}},
		{"another subject", map[string]any{"auth_time": now + 1, "sub": "sub-2"}},
	} {
		rec := login(start(), c.claims)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: callback = %d, want 403", c.name, rec.Code)
		}
		if rec := claim(); rec.Code != http.StatusForbidden {
			t.Errorf("%s: claim = %d, want 403", c.name, rec.Code)
		}
	}
	rec := login(start(), map[string]any{"auth_time": now + 1})
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/reauth-done.html" || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("fresh login = %d %v cookies %v", rec.Code, rec.Header(), rec.Result().Cookies())
	}
	rec = claim()
	wantCode(t, rec, http.StatusOK)
	var n reauthNonceBody
	_ = json.Unmarshal(rec.Body.Bytes(), &n)
	if rec := claim(); rec.Code != http.StatusForbidden {
		t.Errorf("a second claim = %d, want 403", rec.Code)
	}
	tok := e.do(req{method: "POST", path: "/v1/tokens", body: `{"name":"t","scopes":["admin"]}`, header: map[string]string{reauthHeader: n.Nonce}})
	wantStatus(t, tok, 201)
	// A state replayed in another session is refused.
	q := start()
	other, _ := e.st.CreateSession(u.ID, time.Hour)
	e.cookie = &http.Cookie{Name: sessionCookie, Value: other}
	if rec := login(q, map[string]any{"auth_time": now + 1}); rec.Code != http.StatusBadRequest {
		t.Errorf("another session's state = %d, want 400", rec.Code)
	}
}
