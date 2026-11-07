package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/accounts"
	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/drivers"
	"github.com/geofox/publisher/internal/secrets"
	"github.com/geofox/publisher/internal/store"

	_ "modernc.org/sqlite"
)

// writeAccount is an account the account API opens. Who it is follows
// from what was typed, as a network's answer would: a Bluesky account is
// its identifier, a Mastodon one its server; a credential starting with
// "bad" is refused (the network echoing it), with "down" unreachable.
type writeAccount struct {
	apiAccount
	req destination.OpenRequest
}

func (a *writeAccount) secret() string {
	for _, k := range slices.Sorted(maps.Keys(a.req.Secrets)) {
		return a.req.Secrets[k]
	}
	return ""
}

func (a *writeAccount) Identity(context.Context) (destination.Identity, error) {
	s := a.secret()
	switch {
	case strings.HasPrefix(s, "bad"):
		return destination.Identity{}, &destination.HTTPError{Op: "createSession", StatusCode: 401,
			Body: `{"error":"AuthenticationRequired","message":"Invalid identifier or password: ` + s + `"}`}
	case strings.HasPrefix(s, "down"):
		return destination.Identity{}, &destination.HTTPError{Op: "createSession", StatusCode: 503, Body: "echo " + s}
	}
	switch a.typ {
	case "bluesky":
		id, _ := a.req.Config["identifier"].(string)
		return destination.Identity{RemoteID: "did:plc:" + id, Handle: id}, nil
	case "mastodon":
		u, _ := a.req.Config["instance_url"].(string)
		return destination.Identity{RemoteID: u + "/@me", Handle: "me"}, nil
	}
	return destination.Identity{RemoteID: a.typ + "-" + s[:min(len(s), 12)], Handle: "someone"}, nil
}

func (a *writeAccount) Check(context.Context, destination.CheckRequest) destination.Health {
	ok := true
	return destination.Health{OK: true, Reachable: true, AuthOK: &ok}
}

// writeEnv is newV1Env with a registry that can change accounts: a master
// key (unless noKey), the env accounts' variables set (Bluesky's, for
// adopting), and every open recorded.
type writeEnv struct {
	*v1Env
	kr    secrets.Keyring
	mu    sync.Mutex
	opens []destination.OpenRequest
	// instance answers a type's server settings (the Threads app).
	instance func(typ string) (map[string]any, map[string]string)
}

func newWriteEnv(t *testing.T, signIn, noKey bool) *writeEnv {
	t.Helper()
	drivers.Register()
	w := &writeEnv{v1Env: newV1Env(t, signIn)}
	if !noKey {
		k, err := secrets.NewKey(bytes.Repeat([]byte{9}, 32))
		if err != nil {
			t.Fatal(err)
		}
		w.kr = secrets.Keyring{Current: k}
	}
	ks, err := accounts.PrepareKeys(context.Background(), w.st, w.kr)
	if err != nil {
		t.Fatal(err)
	}
	env := []accounts.EnvAccount{
		{Type: "bluesky", EnvAccount: destination.EnvAccount{Key: "default",
			Config:  map[string]any{"identifier": "env.example", "pds_url": "https://pds.example"},
			Secrets: map[string]string{"app_password": "env-app-password"}}},
		{Type: "mastodon", EnvAccount: destination.EnvAccount{Key: "default",
			Config: map[string]any{"instance_url": "https://masto.example"}, Secrets: map[string]string{"access_token": "env-token"}}},
	}
	reg := accounts.New(accounts.Options{Store: w.st, Env: env, Secrets: w.st, Writes: w.st, Flows: w.st, Keys: w.kr, KeyState: ks,
		Getenv: func(string) string { return "" },
		Instance: func(typ string) (map[string]any, map[string]string) {
			w.mu.Lock()
			f := w.instance
			w.mu.Unlock()
			if f == nil {
				return nil, nil
			}
			return f(typ)
		},
		Open: func(_ context.Context, typ string, req destination.OpenRequest) (destination.Account, error) {
			w.mu.Lock()
			w.opens = append(w.opens, req)
			w.mu.Unlock()
			return &writeAccount{apiAccount{id: req.AccountID, typ: typ, env: w.v1Env}, req}, nil
		}})
	if err := reg.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reg.Close)
	w.reg, w.api.Accounts, w.api.MasterKey, w.api.IdemMAC = reg, reg, ks, w.kr.MAC
	return w
}

func (w *writeEnv) openCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.opens)
}

// reauthFor mints a nonce for op on target in the env's session.
func (e *v1Env) reauthFor(op, target string) map[string]string {
	e.t.Helper()
	raw, _, err := e.st.CreateReauthNonce(context.Background(), store.SessionHash(e.cookie.Value), op, target, time.Minute)
	if err != nil {
		e.t.Fatal(err)
	}
	return map[string]string{reauthHeader: raw}
}

const bskyCreate = `{"type":"bluesky","method":"app_password","config":{"pds_url":"https://pds.example"},` +
	`"credentials":{"identifier":"%s","app_password":"%s"}}`

func (w *writeEnv) create(ident, password string, hdr map[string]string) v1Resp {
	w.t.Helper()
	return w.do(req{method: "POST", path: "/v1/accounts", body: fmt.Sprintf(bskyCreate, ident, password), header: hdr})
}

// accountConformance exercises the account write routes once each and
// returns their patterns (for TestV1Conformance).
func accountConformance(t *testing.T) map[string]bool {
	t.Helper()
	w := newWriteEnv(t, true, false)
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
	var acct v1AccountBody
	call("POST /v1/accounts", req{method: "POST", path: "/v1/accounts", body: fmt.Sprintf(bskyCreate, "ada.example", "abcd-efgh-ijkl-mnop"),
		header: map[string]string{"Idempotency-Key": `"acct-1"`}}, 201).json(&acct)
	call("PATCH /v1/accounts/{id}", req{method: "PATCH", path: "/v1/accounts/" + acct.ID,
		body: `{"label":"Mine","credentials":{"identifier":"ada.example","app_password":"wxyz-wxyz-wxyz-wxyz"}}`}, 200)
	call("POST /v1/accounts/{id}:check", req{method: "POST", path: "/v1/accounts/" + acct.ID + ":check"}, 200)
	var key v1AccountBody
	call("POST /v1/accounts", req{method: "POST", path: "/v1/accounts",
		body: `{"type":"nostr","method":"generate","config":{"bootstrap_relay":"wss://relay.example"}}`}, 201).json(&key)
	call("POST /v1/accounts/{id}:reveal", req{method: "POST", path: "/v1/accounts/" + key.ID + ":reveal",
		header: w.reauthFor(opAccountReveal, key.ID)}, 200)
	call("POST /v1/accounts/{id}:adopt", req{method: "POST", path: "/v1/accounts/" + w.acct("bluesky") + ":adopt"}, 200)
	call("DELETE /v1/accounts/{id}", req{method: "DELETE", path: "/v1/accounts/" + acct.ID,
		header: w.reauthFor(opAccountDelete, acct.ID)}, 204)
	return seen
}

// Adding an account answers it with its settings and which credentials
// are set, never them; a replay answers the same without asking the
// network again.
func TestV1CreateAccount(t *testing.T) {
	w := newWriteEnv(t, true, false)
	key := map[string]string{"Idempotency-Key": `"add-1"`}
	r := w.create("@Ada.Example", "abcd-efgh-ijkl-mnop", key)
	if r.Code != 201 {
		t.Fatalf("status %d: %s", r.Code, r.Body.String())
	}
	var a v1AccountBody
	r.json(&a)
	if r.Header().Get("Location") != "/v1/accounts/"+a.ID || !etagRe.MatchString(r.Header().Get("ETag")) {
		t.Fatalf("headers = %v", r.Header())
	}
	if a.Source != "db" || a.AuthMethod != "app_password" || a.Status != "connected" || a.RemoteID != "did:plc:ada.example" ||
		a.Label != "Bluesky" || a.DefaultSelected || !a.Enabled || len(a.LockedFields) != 0 {
		t.Fatalf("account = %+v", a)
	}
	if !maps.Equal(a.Config, map[string]any{"identifier": "ada.example", "pds_url": "https://pds.example"}) ||
		!a.Credentials["app_password"].Set || a.Credentials["app_password"].UpdatedAt.IsZero() || len(a.Credentials) != 1 {
		t.Fatalf("config %v, credentials %v", a.Config, a.Credentials)
	}
	if strings.Contains(r.Body.String(), "abcd-efgh") {
		t.Fatal("the answer carries the password")
	}
	opens := w.openCount()
	again := w.create("@Ada.Example", "abcd-efgh-ijkl-mnop", key)
	if again.Code != 201 || again.Header().Get("Idempotent-Replayed") != "true" || again.Body.String() != r.Body.String() {
		t.Fatalf("replay: %d %v %s", again.Code, again.Header(), again.Body.String())
	}
	if w.openCount() != opens {
		t.Fatal("the replay opened the account again")
	}
	if p := w.create("@Ada.Example", "other-password", key).problem(); p.Code != codeIdemReused {
		t.Fatalf("another body: %+v", p)
	}
	// The stored fingerprint is of the keyed MAC, not of the body.
	var print string
	if err := w.raw().QueryRow(`SELECT fingerprint FROM idempotency_keys`).Scan(&print); err != nil {
		t.Fatal(err)
	}
	if print != fingerprint([]byte(fmt.Sprintf(bskyCreate, "@Ada.Example", "abcd-efgh-ijkl-mnop")), w.kr.MAC) {
		t.Fatal("the stored fingerprint isn't keyed")
	}
	// The same network account again, without a key.
	p := w.create("ada.example", "wxyz-wxyz-wxyz-wxyz", nil).problem()
	if p.Status != 409 || p.Code != codeDuplicateAccount || p.AccountID != a.ID {
		t.Fatalf("duplicate: %+v", p)
	}
	// It is listed, and GET shows the same.
	var got v1AccountBody
	w.do(req{method: "GET", path: "/v1/accounts/" + a.ID}).json(&got)
	if got.AuthMethod != "app_password" || !maps.Equal(got.Config, a.Config) || !got.Credentials["app_password"].Set {
		t.Fatalf("GET = %+v", got)
	}
	// An env account shows what its variables set, locked.
	w.do(req{method: "GET", path: "/v1/accounts/" + w.acct("mastodon")}).json(&got)
	if got.Source != "env" || !slices.Equal(got.LockedFields, []string{"access_token", "instance_url"}) ||
		got.Config["instance_url"] != "https://masto.example" || !got.Credentials["access_token"].Set ||
		strings.Contains(fmt.Sprint(got), "env-token") {
		t.Fatalf("env account = %+v", got)
	}
}

// What the network or the fields refuse is answered on the field, with
// nothing stored.
func TestV1CreateAccountRefused(t *testing.T) {
	w := newWriteEnv(t, true, false)
	p := w.create("ada.example", "bad-secret-pass", nil).problem()
	if p.Status != 422 || p.Code != codeCredentialsRejected || len(p.Errors) != 1 || p.Errors[0].Pointer != "/credentials" ||
		strings.Contains(p.Errors[0].Detail, "bad-secret-pass") || !strings.Contains(p.Errors[0].Detail, "Invalid identifier or password") {
		t.Fatalf("rejected: %+v", p)
	}
	p = w.create("ada.example", "down-secret-pass", nil).problem()
	if p.Status != 502 || p.Code != codeUpstream || strings.Contains(p.Detail, "down-secret") {
		t.Fatalf("down: %+v", p)
	}
	r := w.do(req{method: "POST", path: "/v1/accounts", badRequest: true,
		body: `{"type":"bluesky","credentials":{"identifier":"ada.example","app_password":"x","extra":1},"config":{"pds_url":"http://pds.example"}}`})
	p = r.problem()
	var ptrs []string
	for _, e := range p.Errors {
		ptrs = append(ptrs, e.Pointer)
	}
	if p.Code != codeValidation || !slices.Equal(ptrs, []string{"/credentials/extra", "/config/pds_url"}) {
		t.Fatalf("invalid: %+v", p)
	}
	p = w.do(req{method: "POST", path: "/v1/accounts", body: `{"type":"myspace"}`}).problem()
	if p.Code != codeValidation || p.Errors[0].Pointer != "/type" {
		t.Fatalf("type: %+v", p)
	}
	accts, _ := w.st.Accounts(context.Background(), store.DefaultOwner)
	if len(accts) != 2 {
		t.Fatalf("%d accounts", len(accts))
	}
}

// Without a master key nothing is stored, and the answer (a 409) isn't
// kept for the key, so the same request works once the key is set.
func TestV1CreateAccountNeedsMasterKey(t *testing.T) {
	w := newWriteEnv(t, true, true)
	p := w.create("ada.example", "abcd-efgh-ijkl-mnop", map[string]string{"Idempotency-Key": `"k"`}).problem()
	if p.Status != 409 || p.Code != codeMasterKeyRequired {
		t.Fatalf("%+v", p)
	}
	var n int
	if err := w.raw().QueryRow(`SELECT COUNT(*) FROM idempotency_keys`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("idempotency rows = %d, %v", n, err)
	}
}

// Changing credentials, deleting, checking, revealing and adopting.
func TestV1AccountWrites(t *testing.T) {
	w := newWriteEnv(t, true, false)
	var a v1AccountBody
	w.create("ada.example", "abcd-efgh-ijkl-mnop", nil).json(&a)
	path := "/v1/accounts/" + a.ID

	// A new server needs the credentials again.
	p := w.do(req{method: "PATCH", path: path, body: `{"config":{"pds_url":"https://other.example"}}`}).problem()
	if p.Code != codeValidation || p.Errors[0].Pointer != "/credentials" || p.Errors[0].Code != "required" {
		t.Fatalf("audience: %+v", p)
	}
	// Another identifier finds its server again, unless given: this one
	// leads nowhere here.
	p = w.do(req{method: "PATCH", path: path, body: `{"credentials":{"identifier":"bob.example","app_password":"wxyz-wxyz-wxyz-wxyz"}}`}).problem()
	if p.Status != 422 || p.Errors[0].Pointer != "/credentials/identifier" || p.Errors[0].Code != "not_found" {
		t.Fatalf("lookup: %+v", p)
	}
	// Another network account: refused.
	p = w.do(req{method: "PATCH", path: path, body: `{"config":{"pds_url":"https://pds.example"},"credentials":{"identifier":"bob.example","app_password":"wxyz-wxyz-wxyz-wxyz"}}`}).problem()
	if p.Status != 409 || p.Code != codeIdentityChanged {
		t.Fatalf("identity: %+v", p)
	}
	// The same identifier with the saved password: nothing new to ask for.
	if r := w.do(req{method: "PATCH", path: path, body: `{"credentials":{"identifier":"ada.example"}}`}); r.Code != 200 {
		t.Fatalf("keep password: %d %s", r.Code, r.Body.String())
	}
	// With them, and a label in the same request.
	var b v1AccountBody
	etag := w.do(req{method: "GET", path: path}).Header().Get("ETag")
	r := w.do(req{method: "PATCH", path: path, header: map[string]string{"If-Match": etag},
		body: `{"label":"Work","config":{"pds_url":"https://other.example"},"credentials":{"identifier":"ada.example","app_password":"wxyz-wxyz-wxyz-wxyz"}}`})
	r.json(&b)
	if r.Code != 200 || b.Label != "Work" || b.Config["pds_url"] != "https://other.example" {
		t.Fatalf("change: %d %s", r.Code, r.Body.String())
	}
	// An env account's connection can't be changed or deleted in the app.
	masto := w.acct("mastodon")
	if p := w.do(req{method: "PATCH", path: "/v1/accounts/" + masto, body: `{"config":{"instance_url":"https://x.example"}}`}).problem(); p.Code != codeLockedByEnv {
		t.Fatalf("env patch: %+v", p)
	}
	// Answered before asking for re-authentication.
	if p := w.do(req{method: "DELETE", path: "/v1/accounts/" + masto}).problem(); p.Code != codeLockedByEnv {
		t.Fatalf("env delete: %+v", p)
	}
	if p := w.do(req{method: "POST", path: "/v1/accounts/" + masto + ":reveal"}).problem(); p.Code != codeLockedByEnv {
		t.Fatalf("env reveal: %+v", p)
	}
	// One whose variables are gone (not_configured) can be deleted.
	threads := "acc_threads_gone"
	if err := w.st.InsertAccount(context.Background(), store.Account{ID: threads, Type: "threads", EnvKey: "default", Label: "Threads"}); err != nil {
		t.Fatal(err)
	}
	if p := w.do(req{method: "DELETE", path: "/v1/accounts/" + threads}).problem(); p.Code != codeReauthRequired {
		t.Fatalf("unconfigured env delete: %+v", p)
	}
	if r := w.do(req{method: "DELETE", path: "/v1/accounts/" + threads, header: w.reauthFor(opAccountDelete, threads)}); r.Code != 204 {
		t.Fatalf("unconfigured env delete: %d %s", r.Code, r.Body.String())
	}
	nonce := w.reauthFor(opAccountDelete, masto)
	// Re-authentication names the account: one for another is refused.
	if p := w.do(req{method: "DELETE", path: path, header: nonce}).problem(); p.Code != codeReauthRequired || p.Operation != opAccountDelete {
		t.Fatalf("other nonce: %+v", p)
	}
	// The problem names the account, for the web app's dialog.
	if p := w.do(req{method: "DELETE", path: path}).problem(); p.Code != codeReauthRequired || p.Target != a.ID {
		t.Fatalf("no nonce: %+v", p)
	}
	// Check: once per 10 seconds.
	if r := w.do(req{method: "POST", path: path + ":check"}); r.Code != 200 {
		t.Fatalf("check: %d %s", r.Code, r.Body.String())
	}
	var checked v1AccountBody
	w.do(req{method: "GET", path: path}).json(&checked)
	if checked.Health == nil || !checked.Health.OK || checked.Health.CheckedAt == "" {
		t.Fatalf("health = %+v", checked.Health)
	}
	if r := w.do(req{method: "POST", path: path + ":check"}); r.Code != 429 || r.Header().Get("Retry-After") == "" {
		t.Fatalf("second check: %d", r.Code)
	}
	// Reveal: nothing to show on Bluesky (asked before re-authenticating).
	if p := w.do(req{method: "POST", path: path + ":reveal"}).problem(); p.Code != codeInvalidState {
		t.Fatalf("reveal bluesky: %+v", p)
	}
	if r := w.do(req{method: "DELETE", path: path, header: w.reauthFor(opAccountDelete, a.ID)}); r.Code != 204 {
		t.Fatalf("delete: %d %s", r.Code, r.Body.String())
	}
	if p := w.do(req{method: "GET", path: path + ":nope", undocumented: true}); p.Code != 404 && p.Code != 405 {
		t.Fatalf("unknown action: %d", p.Code)
	}
	for _, rq := range []req{
		{method: "PATCH", path: path, body: `{"label":"x"}`},
		{method: "POST", path: path + ":check"},
		{method: "POST", path: path + ":adopt"},
		{method: "DELETE", path: path, header: w.reauthFor(opAccountDelete, a.ID)},
	} {
		if p := w.do(rq).problem(); p.Status != 404 {
			t.Errorf("%s %s on a deleted account: %+v", rq.method, rq.path, p)
		}
	}
	var list struct{ Data []v1AccountBody }
	w.do(req{method: "GET", path: "/v1/accounts?include=deleted"}).json(&list)
	var deleted *v1AccountBody
	for i := range list.Data {
		if list.Data[i].ID == a.ID {
			deleted = &list.Data[i]
		}
	}
	if deleted == nil || deleted.DeletedAt == "" || deleted.Status != "deleted" || deleted.Enabled {
		t.Fatalf("deleted = %+v", deleted)
	}
	w.do(req{method: "GET", path: "/v1/accounts"}).json(&list)
	if slices.ContainsFunc(list.Data, func(x v1AccountBody) bool { return x.ID == a.ID }) {
		t.Fatal("a deleted account is listed")
	}

	// Adopting the env Bluesky account: then it can be changed.
	bs := w.acct("bluesky")
	var ad v1AccountBody
	if r := w.do(req{method: "POST", path: "/v1/accounts/" + bs + ":adopt"}); r.Code != 200 {
		t.Fatalf("adopt: %d %s", r.Code, r.Body.String())
	} else {
		r.json(&ad)
	}
	if ad.Source != "db" || len(ad.LockedFields) != 0 || !ad.Credentials["app_password"].Set || ad.Config["identifier"] != "env.example" {
		t.Fatalf("adopted = %+v", ad)
	}
	if p := w.do(req{method: "POST", path: "/v1/accounts/" + bs + ":adopt"}).problem(); p.Code != codeInvalidState {
		t.Fatalf("adopt again: %+v", p)
	}
}

// A generated Nostr key is shown once asked, to the signed-in owner who
// just re-authenticated for that account, never cached.
func TestV1RevealAccount(t *testing.T) {
	w := newWriteEnv(t, true, false)
	var k v1AccountBody
	w.do(req{method: "POST", path: "/v1/accounts", header: map[string]string{"Idempotency-Key": `"gen"`},
		body: `{"type":"nostr","method":"generate","config":{"bootstrap_relay":"wss://relay.example"}}`}).json(&k)
	if !k.Credentials["secret_key"].Set {
		t.Fatalf("created = %+v", k)
	}
	path := "/v1/accounts/" + k.ID + ":reveal"
	if p := w.do(req{method: "POST", path: path}).problem(); p.Code != codeReauthRequired || p.Operation != opAccountReveal || p.Target != k.ID {
		t.Fatalf("no nonce: %+v", p)
	}
	tok := w.token(scopeAccountsWrite)
	if p := w.do(req{method: "POST", path: path, auth: tok}).problem(); p.Code != codeSessionRequired {
		t.Fatalf("token: %+v", p)
	}
	r := w.do(req{method: "POST", path: path, header: w.reauthFor(opAccountReveal, k.ID)})
	var out v1AccountRevealBody
	r.json(&out)
	if r.Code != 200 || !strings.HasPrefix(out.Credentials["secret_key"], "nsec1") || r.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("reveal: %d %s", r.Code, r.Body.String())
	}
	// A key that can't open it is said before re-authenticating.
	w.api.MasterKey = accounts.KeyState{Status: accounts.KeyMismatch}
	if p := w.do(req{method: "POST", path: path}).problem(); p.Status != 409 || p.Code != codeMasterKeyMismatch {
		t.Fatalf("mismatched key: %+v", p)
	}
	w.api.MasterKey = accounts.KeyState{Status: accounts.KeyMissing}
	if p := w.do(req{method: "POST", path: path}).problem(); p.Status != 409 || p.Code != codeMasterKeyRequired {
		t.Fatalf("missing key: %+v", p)
	}
	w.api.MasterKey = accounts.KeyState{Status: accounts.KeyOK}
	// The create's stored answer has no key.
	stored := w.idempotencyRows()
	if bytes.Contains(stored, []byte("nsec1")) || bytes.Contains(stored, []byte(out.Credentials["secret_key"])) ||
		bytes.Contains(stored, []byte(w.storedNostrKey(k.ID))) {
		t.Fatal("the stored answer has the key")
	}
}

// Tokens: accounts:write adds and changes accounts; admin doesn't
// include it; the session-only actions refuse tokens.
func TestV1AccountWriteTokens(t *testing.T) {
	w := newWriteEnv(t, true, false)
	admin, writer := w.token(scopeAdmin), w.token(scopeAccountsWrite)
	r := w.do(req{method: "POST", path: "/v1/accounts", auth: admin, body: fmt.Sprintf(bskyCreate, "ada.example", "pw")})
	if p := r.problem(); p.Code != codeInsufficientScope {
		t.Fatalf("admin: %+v", p)
	}
	var a v1AccountBody
	if r := w.do(req{method: "POST", path: "/v1/accounts", auth: writer, body: fmt.Sprintf(bskyCreate, "ada.example", "pw")}); r.Code != 201 {
		t.Fatalf("writer: %d %s", r.Code, r.Body.String())
	} else {
		r.json(&a)
	}
	for _, rq := range []req{
		{method: "DELETE", path: "/v1/accounts/" + a.ID, auth: writer},
		{method: "DELETE", path: "/v1/accounts/" + w.acct("mastodon"), auth: writer},
		{method: "POST", path: "/v1/accounts/" + w.acct("bluesky") + ":adopt", auth: writer},
		{method: "POST", path: "/v1/accounts/" + w.acct("bluesky") + ":reveal", auth: writer},
	} {
		if p := w.do(rq).problem(); p.Code != codeSessionRequired {
			t.Errorf("%s %s: %+v", rq.method, rq.path, p)
		}
	}
}

// Behind the reverse proxy alone, credentials can't be managed, by
// anyone; labels and flags still work.
func TestV1AccountWritesProxyMode(t *testing.T) {
	w := newWriteEnv(t, false, false)
	en, err := w.reg.Create(context.Background(), accounts.NewAccount{Owner: store.DefaultOwner, Type: "bluesky",
		Config: map[string]any{"pds_url": "https://pds.example"}, Credentials: map[string]any{"identifier": "ada.example", "app_password": "pw"}})
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/accounts/" + en.Row.ID
	tok := w.token(scopeAccountsWrite)
	for _, rq := range []req{
		{method: "POST", path: "/v1/accounts", body: fmt.Sprintf(bskyCreate, "bob.example", "pw")},
		{method: "POST", path: "/v1/accounts", auth: tok, body: fmt.Sprintf(bskyCreate, "bob.example", "pw")},
		{method: "PATCH", path: path, body: `{"credentials":{"identifier":"ada.example","app_password":"pw2"}}`},
		{method: "PATCH", path: path, auth: tok, body: `{"method":"app_password","credentials":{"identifier":"ada.example","app_password":"pw2"}}`},
		{method: "DELETE", path: path},
		{method: "POST", path: path + ":reveal"},
		{method: "POST", path: "/v1/accounts/" + w.acct("bluesky") + ":adopt"},
	} {
		if p := w.do(rq).problem(); p.Status != 403 || p.Code != codeSignInRequired {
			t.Errorf("%s %s (auth %q): %+v", rq.method, rq.path, rq.auth, p)
		}
	}
	for _, body := range []string{`{"label":"Proxy"}`, `{"config":{"pds_url":"https://pds.example"}}`} {
		if r := w.do(req{method: "PATCH", path: path, body: body}); r.Code != 200 {
			t.Errorf("PATCH %s: %d %s", body, r.Code, r.Body.String())
		}
	}
}

// At most 10 accounts added or changed a minute per owner; a replay
// doesn't count.
func TestV1AccountWriteRateLimit(t *testing.T) {
	w := newWriteEnv(t, true, false)
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	w.api.acctLimits.writes.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	key := map[string]string{"Idempotency-Key": `"first"`}
	for i := range 10 {
		hdr := map[string]string(nil)
		if i == 0 {
			hdr = key
		}
		if r := w.create(fmt.Sprintf("u%d.example", i), "pw", hdr); r.Code != 201 {
			t.Fatalf("create %d: %d %s", i, r.Code, r.Body.String())
		}
	}
	if r := w.create("u0.example", "pw", key); r.Code != 201 || r.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d", r.Code)
	}
	r := w.create("u10.example", "pw", nil)
	if p := r.problem(); p.Status != 429 || r.Header().Get("Retry-After") == "" {
		t.Fatalf("11th: %+v", p)
	}
	mu.Lock()
	now = now.Add(time.Minute)
	mu.Unlock()
	if r := w.create("u10.example", "pw", nil); r.Code != 201 {
		t.Fatalf("after a minute: %d %s", r.Code, r.Body.String())
	}
}

// The route table: an answer that carries a secret is never stored for an
// idempotent replay, and what manages credentials is refused behind the
// proxy alone.
func TestV1RouteSecrets(t *testing.T) {
	a := &API{}
	flagged := map[string]bool{}
	for _, rt := range a.v1Routes() {
		name := rt.Method + " " + rt.Path
		if rt.ReturnsSecret {
			flagged[name] = true
			if rt.Idempotent {
				t.Errorf("%s returns a secret and is idempotent", name)
			}
		}
		for act, rule := range rt.ActionRules {
			if !slices.Contains(rt.Actions, act) {
				t.Errorf("%s has a rule for an action it doesn't serve: %s", name, act)
			}
			if rule.ReturnsSecret {
				flagged[name+":"+act] = true
				if rt.Idempotent {
					t.Errorf("%s:%s returns a secret and is idempotent", name, act)
				}
			}
		}
		if rt.Idempotent && rt.Path == "/v1/accounts" && rt.IdemPrint == nil {
			t.Errorf("%s holds credentials but fingerprints the body as sent", name)
		}
	}
	for _, want := range []string{"POST /v1/tokens", "POST /v1/accounts/{id}:reveal"} {
		if !flagged[want] {
			t.Errorf("%s isn't flagged as returning a secret", want)
		}
	}
	// The fingerprint of a body with credentials is keyed: without the
	// key, a guess can't be checked against it.
	body := []byte(fmt.Sprintf(bskyCreate, "ada.example", "abcd-efgh-ijkl-mnop"))
	k1, _ := secrets.NewKey(bytes.Repeat([]byte{1}, 32))
	k2, _ := secrets.NewKey(bytes.Repeat([]byte{2}, 32))
	a1 := &API{IdemMAC: secrets.Keyring{Current: k1}.MAC}
	a2 := &API{IdemMAC: secrets.Keyring{Current: k2}.MAC}
	if bytes.Equal(a1.accountCreatePrint(body), a2.accountCreatePrint(body)) || bytes.Contains(a1.accountCreatePrint(body), []byte("abcd")) ||
		!bytes.Equal(a1.accountCreatePrint(body), a1.accountCreatePrint(body)) {
		t.Fatal("the fingerprint isn't keyed")
	}
	noKey := &API{}
	if bytes.Equal(noKey.accountCreatePrint(body), body) || !bytes.Equal(noKey.accountCreatePrint(body), noKey.accountCreatePrint(body)) {
		t.Fatal("without a master key the fingerprint isn't keyed")
	}
}

// No credential typed in the app reaches an answer, an event, a log line,
// the idempotency store, or the database and its write-ahead log in the
// clear.
func TestV1AccountCredentialsDontLeak(t *testing.T) {
	var logs bytes.Buffer
	var logMu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		logMu.Lock()
		defer logMu.Unlock()
		return logs.Write(p)
	}), &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	w := newWriteEnv(t, true, false)
	const first, second, refused, down = "canary-first-7d1f3a", "canary-second-9e2b4c", "bad-canary-5a6b7c", "down-canary-1f2e3d"
	var bodies []string
	keep := func(r v1Resp) v1Resp { bodies = append(bodies, r.Body.String()); return r }
	var a v1AccountBody
	keep(w.create("ada.example", first, map[string]string{"Idempotency-Key": `"leak-1"`})).json(&a)
	keep(w.create("ada.example", first, map[string]string{"Idempotency-Key": `"leak-1"`}))
	keep(w.create("bob.example", refused, map[string]string{"Idempotency-Key": `"leak-2"`}))
	keep(w.create("cy.example", down, map[string]string{"Idempotency-Key": `"leak-3"`}))
	keep(w.do(req{method: "PATCH", path: "/v1/accounts/" + a.ID,
		body: `{"credentials":{"identifier":"ada.example","app_password":"` + second + `"}}`}))
	keep(w.do(req{method: "PATCH", path: "/v1/accounts/" + a.ID,
		body: `{"credentials":{"identifier":"ada.example","app_password":"` + refused + `"}}`}))
	keep(w.do(req{method: "GET", path: "/v1/accounts"}))
	keep(w.do(req{method: "GET", path: "/v1/accounts/" + a.ID}))
	var gen v1AccountBody
	keep(w.do(req{method: "POST", path: "/v1/accounts", header: map[string]string{"Idempotency-Key": `"leak-4"`},
		body: `{"type":"nostr","method":"generate","config":{"bootstrap_relay":"wss://relay.example"}}`})).json(&gen)
	var shown v1AccountRevealBody
	w.do(req{method: "POST", path: "/v1/accounts/" + gen.ID + ":reveal", header: w.reauthFor(opAccountReveal, gen.ID)}).json(&shown)
	nsec := shown.Credentials["secret_key"]
	if nsec == "" {
		t.Fatal("no key revealed")
	}
	hexKey := w.storedNostrKey(gen.ID)

	evs, err := w.st.EventsSince(context.Background(), store.EventFilter{OwnerID: store.DefaultOwner})
	if err != nil {
		t.Fatal(err)
	}
	evJSON, _ := json.Marshal(evs)
	var created, updated int
	for _, e := range evs {
		switch e.Type {
		case store.EventAccountCreated:
			created++
		case store.EventAccountUpdated:
			updated++
		}
	}
	if created != 2 || updated != 1 {
		t.Fatalf("events: %d created, %d updated: %s", created, updated, evJSON)
	}
	if !strings.Contains(string(evJSON), "app_password") {
		t.Fatalf("events don't name the fields: %s", evJSON)
	}
	if err := w.st.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	db, _ := os.ReadFile(w.dbPath())
	wal, _ := os.ReadFile(w.dbPath() + "-wal")
	logMu.Lock()
	logText := logs.String()
	logMu.Unlock()
	places := map[string][]byte{
		"answers": []byte(strings.Join(bodies, "\n")), "events": evJSON, "logs": []byte(logText),
		"idempotency": w.idempotencyRows(), "database": db, "wal": wal,
	}
	for name, b := range places {
		for _, s := range []string{first, second, refused, down, hexKey, nsec} {
			if bytes.Contains(b, []byte(s)) {
				t.Errorf("%s contain %q", name, s)
			}
		}
	}
}

// fingerprint is withIdempotency's fingerprint of a JSON POST body seen
// through mac.
func fingerprint(body []byte, mac func([]byte) []byte) string {
	sum := sha256.New()
	sum.Write([]byte("application/json\x00\x00"))
	sum.Write(mac(body))
	return hex.EncodeToString(sum.Sum(nil))
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// storedNostrKey opens a Nostr account's stored key (hex), to look for it.
func (w *writeEnv) storedNostrKey(id string) string {
	w.t.Helper()
	sealed, err := w.st.Secrets(context.Background(), store.DefaultOwner, store.SecretScopeAccount, id)
	if err != nil || len(sealed) != 1 {
		w.t.Fatal(sealed, err)
	}
	v, err := w.kr.Open(accounts.Binding(sealed[0]), sealed[0].Sealed)
	if err != nil {
		w.t.Fatal(err)
	}
	return v
}

func (w *writeEnv) idempotencyRows() []byte {
	w.t.Helper()
	rows, err := w.raw().Query(`SELECT * FROM idempotency_keys`)
	if err != nil {
		w.t.Fatal(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out bytes.Buffer
	for rows.Next() {
		vals := make([]sql.RawBytes, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			w.t.Fatal(err)
		}
		for _, v := range vals {
			out.Write(v)
			out.WriteByte('\n')
		}
	}
	if out.Len() == 0 {
		w.t.Fatal("no idempotency rows")
	}
	return out.Bytes()
}

func (w *writeEnv) dbPath() string { return w.path }

// raw is a second connection to the env's database, to read it as stored.
func (w *writeEnv) raw() *sql.DB {
	w.t.Helper()
	db, err := sql.Open("sqlite", "file:"+w.path+"?mode=ro")
	if err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() { _ = db.Close() })
	return db
}
