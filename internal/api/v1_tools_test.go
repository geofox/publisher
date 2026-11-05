package api

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
}

func (f *v1FakeSyncer) Scan(_ context.Context, ts []relaysync.Target) []relaysync.RelayDiff {
	f.scanned = ts
	out := []relaysync.RelayDiff{}
	for _, t := range ts {
		out = append(out, relaysync.RelayDiff{URL: t.URL, Group: t.Group, MissingAtHome: 1, Status: "ok"})
	}
	return out
}

func (f *v1FakeSyncer) Apply(_ context.Context, ts []relaysync.Target, dir string) []relaysync.ApplyResult {
	f.scanned, f.dir = ts, dir
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
// and a home relay; it returns the Nostr account's id.
func (e *v1Env) withNostr() string {
	e.t.Helper()
	if _, err := e.st.SeedEnvAccounts(context.Background(), store.DefaultOwner, []store.EnvAccount{
		{Type: "bluesky", Label: "Bluesky"}, {Type: "mastodon", Label: "Mastodon"}, {Type: "nostr", Label: "Nostr"}}); err != nil {
		e.t.Fatal(err)
	}
	e.api.HomeRelay = "wss://home.example"
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
	// A disabled Nostr account.
	if _, err := e.st.SeedEnvAccounts(context.Background(), store.DefaultOwner, []store.EnvAccount{{Type: "bluesky", Label: "Bluesky"}}); err != nil {
		t.Fatal(err)
	}
	wantProblem(t, e.do(req{method: "GET", path: base}), 409, codeInvalidState)
	// Relay sync needs admin.
	_, raw, err := e.st.CreateScopedToken("w", []string{"posts:write"}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	wantProblem(t, e.do(req{method: "GET", path: base, auth: raw}), 403, codeInsufficientScope)
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
