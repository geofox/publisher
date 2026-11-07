package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/httpx"
	"github.com/geofox/publisher/internal/localauth"
	"github.com/geofox/publisher/internal/localauth/webauthntest"
	"github.com/geofox/publisher/internal/store"
)

// What someone without the owner's credentials can do to get in the
// owner's way (the security review's findings), and that they can't.

// Wrong passwords from many addresses slow down password sign-in for
// browsers that never signed in, but not for one that did.
func TestOwnerWideDelaySparesAKnownBrowser(t *testing.T) {
	e := newLocalEnv(t)
	e.setupWithPassword(testPassword) // this browser is now known
	known := e.jar[deviceCookie]
	if known == nil {
		t.Fatal("setup didn't mark the browser as known")
	}
	ctx := context.Background()
	for i := range 30 {
		_ = e.st.RecordLoginFailure(ctx, localauth.AddrKey(netip.AddrFrom4([4]byte{198, 51, 100, byte(i)})), time.Now())
	}
	// A browser that never signed in, from a fresh address: held back,
	// and told why.
	e.jar = map[string]*http.Cookie{}
	e.addr = "203.0.113.9:1"
	rec := e.send("POST", "/auth/password", pwBody(testPassword), nil)
	wantCode(t, rec, http.StatusTooManyRequests)
	if !strings.Contains(rec.Body.String(), "many places") {
		t.Errorf("owner-wide delay said %s", rec.Body.String())
	}
	// The known browser gets in.
	e.jar = map[string]*http.Cookie{deviceCookie: known}
	wantCode(t, e.send("POST", "/auth/password", pwBody(testPassword), nil), http.StatusNoContent)
	// A forged or stale mark doesn't count.
	e.jar = map[string]*http.Cookie{deviceCookie: {Name: deviceCookie, Value: "1700000000.AAAA"}}
	wantCode(t, e.send("POST", "/auth/password", pwBody(testPassword), nil), http.StatusTooManyRequests)
	// reset-login forgets every browser.
	if err := e.st.ResetLocalLogin(ctx); err != nil {
		t.Fatal(err)
	}
	if e.api.Local.KnownDevice(ctx, known.Value) {
		t.Error("a browser is still known after reset-login")
	}
}

// Passkey requests are sealed in the browser's cookie: any number of
// begins from elsewhere can't push out the owner's.
func TestPasskeyRequestsCantBeCrowdedOut(t *testing.T) {
	e := newLocalEnv(t)
	e.setupWithPasskey()
	e.jar = map[string]*http.Cookie{}
	rec := e.send("POST", "/auth/passkey/begin", "{}", nil)
	wantCode(t, rec, http.StatusOK)
	mine := e.jar[ceremonyCookie]
	a, err := e.authn.Get(rec.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 1500 {
		e.addr = fmt.Sprintf("[2001:db8:%x::1]:1", i)
		wantCode(t, e.send("POST", "/auth/passkey/begin", "{}", nil), http.StatusOK)
	}
	e.addr = "192.0.2.10:4000"
	e.jar = map[string]*http.Cookie{ceremonyCookie: mine}
	wantCode(t, e.send("POST", "/auth/passkey/finish", string(a), nil), http.StatusNoContent)
}

// A finish without a passkey request isn't a failed sign-in (anyone can
// send one); real failures are recorded at most 10 per address, failed
// finishes count against the address's limit, and failures are listed
// apart, so they can't push the rest out of the Access panel.
func TestFailedSignInsCantFloodTheHistory(t *testing.T) {
	e := newLocalEnv(t)
	e.setupWithPasskey()
	ctx := context.Background()
	wantStatus(t, e.v1(req{method: "POST", path: "/v1/tokens", body: `{"name":"n8n","scopes":["posts:write"]}`,
		header: e.reauthPasskey(opTokenCreate)}), 201)
	failures := func() int {
		evs, _ := e.st.SignInEvents(ctx, store.DefaultOwner, 1000, 1000)
		n := 0
		for _, ev := range evs {
			if ev.Type == store.EventLoginFailed {
				n++
			}
		}
		return n
	}
	before := failures()

	// No passkey request at all.
	e.jar = map[string]*http.Cookie{}
	e.addr = "198.51.100.1:1"
	for range 30 {
		if rec := e.send("POST", "/auth/passkey/finish", `{}`, nil); rec.Code != http.StatusConflict && rec.Code != http.StatusTooManyRequests {
			t.Fatalf("finish without a request = %d", rec.Code)
		}
	}
	if n := failures(); n != before {
		t.Fatalf("%d failures recorded for finishes without a request", n-before)
	}

	// A real request, then garbage: rejected, counted, limited.
	e.addr = "198.51.100.2:1"
	wantCode(t, e.send("POST", "/auth/passkey/begin", "{}", nil), http.StatusOK)
	other := &webauthntest.Authenticator{Origin: "https://evil.example", Creds: e.authn.Creds} // signs for another origin
	codes := map[int]int{}
	for range 25 {
		rec := e.send("POST", "/auth/passkey/begin", "{}", nil)
		if rec.Code == http.StatusOK {
			a, err := other.Get(rec.Body.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			rec = e.send("POST", "/auth/passkey/finish", string(a), nil)
		}
		codes[rec.Code]++
	}
	if codes[http.StatusTooManyRequests] == 0 || codes[http.StatusUnauthorized] == 0 {
		t.Errorf("answers %v: want rejections, then 429", codes)
	}
	// 20 a minute, begins and failures together: 9 failures got through.
	if n := failures() - before; n != 9 {
		t.Errorf("%d failures recorded from one address, want 9", n)
	}
	// Over a longer time, at most 10 are recorded per address in ten
	// minutes (all are logged).
	before = failures()
	for range 15 {
		r := httptest.NewRequest("POST", "/auth/password", nil)
		r.RemoteAddr = "[2001:db8:5:5::1]:1"
		e.api.failedSignIn(r, "password", localauth.ErrSignIn)
		r.RemoteAddr = "[2001:db8:5:5::2]:1" // the same /64
		e.api.failedSignIn(r, "password", localauth.ErrSignIn)
	}
	if n := failures() - before; n != 10 {
		t.Errorf("%d failures recorded from one /64, want 10", n)
	}

	// Many addresses: the listing keeps the sign-ins and the token.
	for i := range 60 {
		_ = e.st.AddSignInEvent(ctx, store.DefaultOwner, store.EventLoginFailed, store.EventData{Method: "password", Addr: fmt.Sprint("203.0.113.", i)})
	}
	e.addr = "192.0.2.10:4000"
	wantCode(t, e.signInWithPasskey(), http.StatusNoContent)
	r := e.v1(req{method: "GET", path: "/v1/me/sign-in-events"})
	var list struct{ Data []v1SignInEventBody }
	r.json(&list)
	seen := map[string]int{}
	for _, ev := range list.Data {
		seen[ev.Type]++
	}
	if seen[store.EventTokenCreated] != 1 || seen[store.EventSignInAdded] != 1 || seen[store.EventLoginFailed] != failedEventsShown {
		t.Errorf("listed %v", seen)
	}
}

// A passkey the authenticator rejected during setup is a 422 on the
// credential, not the "setup expired" 401, and setup goes on.
func TestSetupRejectedPasskeyIsNotExpired(t *testing.T) {
	e := newLocalEnv(t)
	wantCode(t, e.exchange(e.setupLink()), http.StatusNoContent)
	begin := func() []byte {
		rec := e.send("POST", "/setup/passkey/begin", "{}", nil)
		wantCode(t, rec, http.StatusOK)
		return rec.Body.Bytes()
	}
	e.authn.NoUV = true
	cred, _, err := e.authn.Create(begin())
	if err != nil {
		t.Fatal(err)
	}
	e.authn.Creds = nil
	rec := e.send("POST", "/setup/passkey/finish", finishBody("Laptop", cred), nil)
	wantCode(t, rec, http.StatusUnprocessableEntity)
	if !strings.Contains(rec.Body.String(), "/credential") {
		t.Errorf("answer %s", rec.Body.String())
	}
	e.authn.NoUV = false
	cred, _, _ = e.authn.Create(begin())
	wantCode(t, e.send("POST", "/setup/passkey/finish", finishBody("Laptop", cred), nil), http.StatusNoContent)
}

// A newer setup link ends a setup page opened with an older one (a
// leaked link stops working when the owner makes a new one).
func TestNewerSetupLinkEndsAnOpenedSetup(t *testing.T) {
	e := newLocalEnv(t)
	wantCode(t, e.exchange(e.setupLink()), http.StatusNoContent)
	state := func() int { return e.send("GET", "/setup/state", "", nil).Code }
	if code := state(); code != http.StatusNoContent {
		t.Fatalf("state after exchange = %d", code)
	}
	_ = e.setupLink() // publisher setup-url
	if code := state(); code != http.StatusUnauthorized {
		t.Errorf("state after a newer link = %d, want 401", code)
	}
	wantCode(t, e.send("POST", "/setup/password", pwBody(testPassword), nil), http.StatusUnauthorized)
	delete(e.jar, setupCookie)
	if code := state(); code != http.StatusUnauthorized {
		t.Errorf("state without a setup cookie = %d, want 401", code)
	}
	e.setupWithPassword(testPassword)
	if code := state(); code != http.StatusGone {
		t.Errorf("state after setup = %d, want 410", code)
	}
}

// After PUBLIC_URL changes, passkeys made for the old address can't sign
// in, so they don't count: setup opens again, and they show as unusable.
func TestPasskeysForTheOldAddress(t *testing.T) {
	e := newLocalEnv(t)
	e.setupWithPasskey()
	// The same database, now at another address.
	svc, err := localauth.New(e.st, "https://new.example")
	if err != nil {
		t.Fatal(err)
	}
	a := &API{Store: e.st, Version: "v9.9.9-test", UserLanguages: []string{"en"}, Mode: ModeLocal, Local: svc,
		Clients: &httpx.ClientAddrs{}, PublicURL: "https://new.example", AppBaseURL: "https://new.example", SessionTTL: time.Hour}
	h := a.Routes()
	rec := rawReq(h, "GET", "/auth/methods", false, "")
	var m methodsBody
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if !m.Setup || m.HasPasskey {
		t.Errorf("methods at the new address = %+v", m)
	}
	// The old address still works, and lists the passkey as usable.
	r := e.v1(req{method: "GET", path: "/v1/me/sign-in"})
	var si v1SignInBody
	r.json(&si)
	if len(si.Passkeys) != 1 || !si.Passkeys[0].Usable || si.Passkeys[0].RPID != "app.example" {
		t.Errorf("passkeys = %+v", si.Passkeys)
	}
}

// A removal that can't happen is answered before asking to confirm.
func TestRemovalChecksComeBeforeConfirming(t *testing.T) {
	e := newLocalEnv(t)
	e.setupWithPasskey()
	r := e.v1(req{method: "GET", path: "/v1/me/sign-in"})
	var si v1SignInBody
	r.json(&si)
	wantProblem(t, e.v1(req{method: "DELETE", path: "/v1/me/passkeys/" + si.Passkeys[0].ID}), 409, codeLastSignInMethod)
	wantProblem(t, e.v1(req{method: "DELETE", path: "/v1/me/passkeys/pk_nope"}), 404, codeNotFound)
	wantProblem(t, e.v1(req{method: "DELETE", path: "/v1/me/password"}), 404, codeNotFound)
	// With a password, the passkey can go (after confirming).
	wantCode(t, e.v1(req{method: "PUT", path: "/v1/me/password", body: pwBody(testPassword), header: e.reauthPasskey(opPasswordSet)}).ResponseRecorder, 204)
	wantProblem(t, e.v1(req{method: "DELETE", path: "/v1/me/passkeys/" + si.Passkeys[0].ID}), 403, codeReauthRequired)
}

// Changing the password takes the current one instead of a nonce; a wrong
// one is a 422 on it.
func TestChangePasswordWithTheCurrentOne(t *testing.T) {
	e := newLocalEnv(t)
	e.setupWithPassword(testPassword)
	change := func(cur string) v1Resp {
		b, _ := json.Marshal(map[string]string{"password": "a brand new passphrase", "current_password": cur})
		return e.v1(req{method: "PUT", path: "/v1/me/password", body: string(b)})
	}
	p := wantProblem(t, change("not the password"), 422, codeValidation)
	if len(p.Errors) != 1 || p.Errors[0].Pointer != "/current_password" {
		t.Errorf("errors %+v", p.Errors)
	}
	wantStatus(t, change(testPassword), 204)
	wantCode(t, e.send("POST", "/auth/password", pwBody("a brand new passphrase"), nil), http.StatusNoContent)
}

// At most MaxPasskeys passkeys.
func TestPasskeyCap(t *testing.T) {
	e := newLocalEnv(t)
	e.setupWithPasskey()
	u, _ := e.st.LocalUser(context.Background())
	for i := range localauth.MaxPasskeys - 1 {
		if _, err := e.st.AddWebAuthnCredential(context.Background(), u.ID, store.WebAuthnCredential{
			RPID: "app.example", CredentialID: []byte(fmt.Sprint("c", i)), PublicKey: []byte("k"), Name: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	wantProblem(t, e.v1(req{method: "POST", path: "/v1/me/passkeys:begin", header: e.reauthPasskey(opSignInAdd)}), 409, codeInvalidState)
}

// A passkey that always reports a counter of 0 (synced passkeys do) can't
// be replayed either: its sign-in request is used once.
func TestReplayWithoutACounter(t *testing.T) {
	e := newLocalEnv(t)
	e.setupWithPasskey()
	e.authn.FreezeCounter = true // stays at 0
	e.jar = map[string]*http.Cookie{}
	rec := e.send("POST", "/auth/passkey/begin", "{}", nil)
	wantCode(t, rec, http.StatusOK)
	sealed := e.jar[ceremonyCookie]
	a, err := e.authn.Get(rec.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	wantCode(t, e.send("POST", "/auth/passkey/finish", string(a), nil), http.StatusNoContent)
	e.jar = map[string]*http.Cookie{ceremonyCookie: sealed}
	wantCode(t, e.send("POST", "/auth/passkey/finish", string(a), nil), http.StatusConflict)
}
