package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/accounts"
	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/relaysync"
	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/verify"
)

type v1FakeTranslator struct{ fail bool }

func (f v1FakeTranslator) Translate(_ context.Context, text, target string) (string, string, error) {
	if f.fail {
		return "", "", errors.New("deepl: 456 quota exceeded, key=secret")
	}
	return "[" + target + "] " + text, "en", nil
}

type v1FakeVerifier struct{ got verify.Input }

func (f *v1FakeVerifier) Verify(_ context.Context, in verify.Input) verify.Verdict {
	f.got = in
	return verify.Verdict{Platform: "nostr", Status: verify.StatusVerified, Assurance: "cryptographic",
		Signer: &verify.Signer{Npub: "npub1x"}, Checks: []verify.Check{{Name: "signature", Result: "pass"}}}
}

type v1FakeSyncer struct {
	scanned []relaysync.Target
	dir     string
	who     relaysync.Owner
}

func (f *v1FakeSyncer) Scan(_ context.Context, who relaysync.Owner, ts []relaysync.Target) []relaysync.RelayDiff {
	f.scanned, f.who = ts, who
	out := []relaysync.RelayDiff{}
	for _, t := range ts {
		out = append(out, relaysync.RelayDiff{URL: t.URL, Group: t.Group, MissingAtHome: 1, Status: "ok"})
	}
	return out
}

func (f *v1FakeSyncer) Apply(_ context.Context, who relaysync.Owner, ts []relaysync.Target, dir string) []relaysync.ApplyResult {
	f.scanned, f.dir, f.who = ts, dir, who
	out := []relaysync.ApplyResult{}
	for i, t := range ts {
		res := relaysync.ApplyResult{URL: t.URL, Direction: dir, Published: 2, Status: "ok"}
		if i == 1 {
			res.Failed, res.Status, res.Message = 1, "partial", "1 event refused"
		}
		out = append(out, res)
	}
	return out
}

// withNostr gives the env a Nostr account (besides Bluesky and Mastodon)
// whose home relay is wss://home.example, and loads the accounts; it
// returns the Nostr account's id.
func (e *v1Env) withNostr() string {
	e.t.Helper()
	if _, err := e.st.SeedEnvAccounts(context.Background(), store.DefaultOwner, []store.EnvAccount{
		{Type: "bluesky", Label: "Bluesky"}, {Type: "mastodon", Label: "Mastodon"}, {Type: "nostr", Label: "Nostr"}}); err != nil {
		e.t.Fatal(err)
	}
	e.withAccounts()
	return e.acct("nostr")
}

func fieldCodes(p problemBody) string {
	var got []string
	for _, fe := range p.Errors {
		got = append(got, fe.Pointer+fe.Parameter+":"+fe.Code)
	}
	return strings.Join(got, " ")
}

func TestV1Translate(t *testing.T) {
	e := newV1Env(t, true)
	wantProblem(t, e.do(req{method: "POST", path: "/v1/translate", body: `{"text":"hi","target_lang":"fr"}`}), 503, codeUnavailable)
	e.api.Translator = v1FakeTranslator{}
	r := e.do(req{method: "POST", path: "/v1/translate", body: `{"text":"hi","target_lang":"FR"}`})
	wantStatus(t, r, 200)
	var got v1TranslateBody
	r.json(&got)
	if got.Text != "[fr] hi" || got.DetectedSourceLanguage != "en" {
		t.Fatalf("translation = %+v", got)
	}
	p := wantProblem(t, e.do(req{method: "POST", path: "/v1/translate", badRequest: true,
		body: `{"text":" ","target_lang":"xx"}`}), 422, codeValidation)
	if c := fieldCodes(p); c != "/text:required /target_lang:unsupported_language" {
		t.Fatalf("errors = %s", c)
	}
	e.api.Translator = v1FakeTranslator{fail: true}
	p = wantProblem(t, e.do(req{method: "POST", path: "/v1/translate", body: `{"text":"hi","target_lang":"fr"}`}), 502, codeUpstream)
	if strings.Contains(p.Detail, "secret") {
		t.Fatalf("upstream detail leaked: %q", p.Detail)
	}
}

func TestV1Verify(t *testing.T) {
	e := newV1Env(t, true)
	wantProblem(t, e.do(req{method: "POST", path: "/v1/verify", body: `{"input":"x"}`}), 503, codeUnavailable)
	fv := &v1FakeVerifier{}
	e.api.Verify = fv
	r := e.do(req{method: "POST", path: "/v1/verify", body: `{"input":"{\"id\":\"1\"}","platform":"nostr","expected":"npub1x"}`})
	wantStatus(t, r, 200)
	var got verify.Verdict
	r.json(&got)
	if got.Status != verify.StatusVerified || fv.got.Expected != "npub1x" || fv.got.Platform != "nostr" {
		t.Fatalf("verdict = %+v, input = %+v", got, fv.got)
	}
	p := wantProblem(t, e.do(req{method: "POST", path: "/v1/verify", badRequest: true,
		body: `{"input":"","platform":"myspace","expected":"` + strings.Repeat("x", 501) + `"}`}), 422, codeValidation)
	if c := fieldCodes(p); c != "/input:required /platform:invalid /expected:too_long" {
		t.Fatalf("errors = %s", c)
	}
}

func TestV1Relays(t *testing.T) {
	e := newV1Env(t, true)
	nostr := e.withNostr()
	base := "/v1/accounts/" + nostr + "/nostr/relays"
	put := func(body string) v1Resp {
		return e.do(req{method: "PUT", path: base, body: body, badRequest: true})
	}
	r := put(`{"secondary":["wss://b.example/","WSS://A.Example:443","ws://192.168.1.5:7777"]}`)
	wantStatus(t, r, 200)
	var got v1RelaysBody
	r.json(&got)
	if strings.Join(got.Secondary, " ") != "wss://b.example wss://a.example ws://192.168.1.5:7777" ||
		got.Home != "wss://home.example" || len(got.Targets) != 3 || got.Targets[0].Group != "secondary" {
		t.Fatalf("relays = %+v", got)
	}
	p := wantProblem(t, put(`{"secondary":["https://x.example","wss://HOME.example:443/","wss://x.onion","wss://a.example","wss://a.example/","wss://u:p@x.example",`+
		`"ws://127.0.0.1:6379","ws://169.254.169.254","wss://localhost","ws://[::1]:80"]}`), 422, codeValidation)
	if c := fieldCodes(p); c != "/secondary/0:invalid /secondary/1:not_allowed /secondary/2:not_allowed /secondary/4:duplicate /secondary/5:invalid"+
		" /secondary/6:not_allowed /secondary/7:not_allowed /secondary/8:not_allowed /secondary/9:not_allowed" {
		t.Fatalf("errors = %s", c)
	}
	wantProblem(t, put(`{}`), 422, codeValidation)
	wantProblem(t, put(`{"secondary":[`+strings.Repeat(`"wss://a.example",`, 50)+`"wss://a.example"]}`), 422, codeValidation)

	// Scan and sync go through the targets, or the ones named.
	wantProblem(t, e.do(req{method: "POST", path: base + ":scan"}), 503, codeUnavailable)
	fs := &v1FakeSyncer{}
	e.api.Sync = fs
	r = e.do(req{method: "POST", path: base + ":scan"})
	wantStatus(t, r, 200)
	if len(fs.scanned) != 3 {
		t.Fatalf("scanned %+v", fs.scanned)
	}
	r = e.do(req{method: "POST", path: base + ":scan", body: `{"relays":["wss://a.example/"]}`})
	wantStatus(t, r, 200)
	if len(fs.scanned) != 1 || fs.scanned[0].URL != "wss://a.example" {
		t.Fatalf("scanned %+v", fs.scanned)
	}
	p = wantProblem(t, e.do(req{method: "POST", path: base + ":sync", badRequest: true,
		body: `{"direction":"sideways","relays":["wss://nope.example"]}`}), 422, codeValidation)
	if c := fieldCodes(p); c != "/direction:invalid" {
		t.Fatalf("errors = %s", c)
	}
	p = wantProblem(t, e.do(req{method: "POST", path: base + ":sync", badRequest: true,
		body: `{"direction":"pull","relays":["wss://nope.example"]}`}), 422, codeValidation)
	if c := fieldCodes(p); c != "/relays/0:unknown_relay" {
		t.Fatalf("errors = %s", c)
	}
	r = e.do(req{method: "POST", path: base + ":sync", body: `{"direction":"push"}`})
	wantStatus(t, r, 200)
	if fs.dir != "push" || len(fs.scanned) != 3 {
		t.Fatalf("sync = %s %+v", fs.dir, fs.scanned)
	}
	// One at a time.
	e.api.relaySyncMu.Lock()
	wantProblem(t, e.do(req{method: "POST", path: base + ":sync", body: `{"direction":"push"}`}), 409, codeInvalidState)
	e.api.relaySyncMu.Unlock()
	// A chunked request with no body scans everything.
	chunked := httptest.NewRequest("POST", base+":scan", nil)
	chunked.ContentLength = -1
	chunked.AddCookie(e.cookie)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, chunked)
	if rec.Code != 200 || len(fs.scanned) != 3 {
		t.Fatalf("chunked empty scan: %d %s", rec.Code, rec.Body.String())
	}

	// Only a Nostr account has relays.
	wantProblem(t, e.do(req{method: "GET", path: "/v1/accounts/" + e.acct("bluesky") + "/nostr/relays"}), 404, codeNotFound)
	wantProblem(t, e.do(req{method: "GET", path: "/v1/accounts/acc_nope/nostr/relays"}), 404, codeNotFound)
	// A turned-off Nostr account, saying so.
	e.disable(e.acct("nostr"))
	if p := wantProblem(t, e.do(req{method: "GET", path: base}), 409, codeInvalidState); !strings.Contains(p.Detail, "turned off") {
		t.Errorf("detail = %q", p.Detail)
	}
	// Relay sync needs admin.
	_, raw, err := e.st.CreateScopedToken("w", []string{"posts:write"}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	wantProblem(t, e.do(req{method: "GET", path: base, auth: raw}), 403, codeInsufficientScope)
}

// Each Nostr account has its own relay list, home relay and key: a scan
// or sync works on the path's account only, and another owner's account
// isn't found.
func TestV1RelaysPerAccount(t *testing.T) {
	e := newV1Env(t, true)
	ctx := context.Background()
	if _, err := e.st.SeedEnvAccounts(ctx, store.DefaultOwner, []store.EnvAccount{{Type: "nostr", Label: "Nostr"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.InsertAccount(ctx, store.Account{ID: "acc_n2", Type: "nostr", Label: "Second", Source: "env", EnvKey: "2",
		Enabled: true, CreatedAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	fs := &v1FakeSyncer{}
	e.api.Sync = fs
	e.withAccounts()
	first := e.acct("nostr")
	path := func(id string) string { return "/v1/accounts/" + id + "/nostr/relays" }
	put := func(id, list string) v1RelaysBody {
		t.Helper()
		r := e.do(req{method: "PUT", path: path(id), body: `{"secondary":` + list + `}`})
		wantStatus(t, r, 200)
		var got v1RelaysBody
		r.json(&got)
		return got
	}
	a := put(first, `["wss://a.example"]`)
	b := put("acc_n2", `["wss://b.example","wss://home.example"]`)
	if a.Home != "wss://home.example" || strings.Join(a.Secondary, " ") != "wss://a.example" {
		t.Errorf("first account = %+v", a)
	}
	// The first account's home relay is just another relay to the second.
	if b.Home != "wss://home-1.example" || strings.Join(b.Secondary, " ") != "wss://b.example wss://home.example" {
		t.Errorf("second account = %+v", b)
	}
	var got v1RelaysBody
	r := e.do(req{method: "GET", path: path(first)})
	wantStatus(t, r, 200)
	r.json(&got)
	if strings.Join(got.Secondary, " ") != "wss://a.example" {
		t.Errorf("the second account's PUT changed the first's list: %+v", got)
	}

	wantStatus(t, e.do(req{method: "POST", path: path("acc_n2") + ":scan"}), 200)
	if fs.who.Home != "wss://home-1.example" || len(fs.scanned) != 2 {
		t.Errorf("scan ran as %+v over %+v, want the second account's home and list", fs.who, fs.scanned)
	}
	wantStatus(t, e.do(req{method: "POST", path: path(first) + ":sync", body: `{"direction":"pull"}`}), 200)
	if fs.who.Home != "wss://home.example" || len(fs.scanned) != 1 || fs.scanned[0].URL != "wss://a.example" {
		t.Errorf("sync ran as %+v over %+v, want the first account's", fs.who, fs.scanned)
	}
	if bytes.Equal(fs.who.Pubkey[:], make([]byte, 32)) {
		t.Error("sync was given no public key")
	}
	// The home relay is dialed with the account's client; the targets,
	// which the owner set, through the guard (no loopback, whoever set the
	// account up).
	if fs.who.HomeClient == nil || fs.who.TargetClient == nil || fs.who.HomeClient == fs.who.TargetClient {
		t.Errorf("clients: home %p, target %p", fs.who.HomeClient, fs.who.TargetClient)
	}
	loop := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer loop.Close()
	if _, err := fs.who.TargetClient.Get(loop.URL); !errors.Is(err, destination.ErrBlockedAddress) {
		t.Errorf("the target client reached a loopback relay: %v", err)
	}

	e.api.Accounts = withForeign{e.reg, t}
	for _, m := range []string{"GET", "PUT"} {
		wantProblem(t, e.do(req{method: m, path: path("acc_theirs"), body: `{"secondary":[]}`}), 404, codeNotFound)
	}
	wantProblem(t, e.do(req{method: "POST", path: path("acc_theirs") + ":scan"}), 404, codeNotFound)
	e.api.Accounts = e.reg

	// Relay sync is offered while a Nostr account is connected.
	features := func() bool {
		var inst v1InstanceBody
		r := e.do(req{method: "GET", path: "/v1/instance"})
		wantStatus(t, r, 200)
		r.json(&inst)
		return inst.Features.RelaySync
	}
	if !features() {
		t.Error("relay_sync off with two connected Nostr accounts")
	}
	e.disable(first)
	e.disable("acc_n2")
	if features() {
		t.Error("relay_sync on with every Nostr account turned off")
	}
}

// withForeign is the registry plus acc_theirs, another owner's connected
// Nostr account, which must never be leased for this owner.
type withForeign struct {
	*accounts.Registry
	t *testing.T
}

func (w withForeign) Get(id string) (accounts.Entry, bool) {
	if id == "acc_theirs" {
		return accounts.Entry{Row: store.Account{ID: id, OwnerID: "own_other", Type: "nostr", Enabled: true},
			Status: accounts.Connected}, true
	}
	return w.Registry.Get(id)
}

func (w withForeign) Acquire(id string) (destination.Account, func(), error) {
	if id == "acc_theirs" {
		w.t.Error("another owner's account was leased")
	}
	return w.Registry.Acquire(id)
}

func TestV1PublicFeed(t *testing.T) {
	e := newV1Env(t, true)
	e.seedPost("live", "success", "bluesky")
	e.seedPost("down", "failed", "bluesky")
	feedReq := func(auth, q string) v1Resp {
		return e.do(req{method: "GET", path: "/v1/public/feed" + q, auth: auth, badRequest: q != ""})
	}
	wantProblem(t, feedReq("feed-secret", ""), 404, codeNotFound)
	e.api.PublicFeedToken = "feed-secret"
	wantProblem(t, feedReq("none", ""), 401, codeUnauthenticated)
	wantProblem(t, feedReq("wrong", ""), 401, codeUnauthenticated)
	// An API token is not the feed token.
	_, apiTok, err := e.st.CreateScopedToken("api", []string{"admin"}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	wantProblem(t, feedReq(apiTok, ""), 401, codeUnauthenticated)
	r := feedReq("feed-secret", "")
	wantStatus(t, r, 200)
	var got struct {
		Posts []struct {
			ID    string                 `json:"id"`
			Links []struct{ URL string } `json:"links"`
		} `json:"posts"`
	}
	r.json(&got)
	if len(got.Posts) != 1 || got.Posts[0].ID != "live" || len(got.Posts[0].Links) != 1 {
		t.Fatalf("feed = %+v", got)
	}
	wantProblem(t, feedReq("feed-secret", "?limit=0"), 400, codeInvalidParameter)
	wantStatus(t, feedReq("feed-secret", "?limit=100"), 200)
}
