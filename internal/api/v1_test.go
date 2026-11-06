package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pb33f/libopenapi"
	validator "github.com/pb33f/libopenapi-validator"
	"github.com/pb33f/libopenapi-validator/config"
	v3 "github.com/pb33f/libopenapi/datamodel/high/v3"

	apidocs "github.com/geofox/publisher/docs/api"
	"github.com/geofox/publisher/internal/accounts"
	"github.com/geofox/publisher/internal/auth"
	"github.com/geofox/publisher/internal/store"
)

// ─── harness ────────────────────────────────────────────────────────────

var (
	oaOnce sync.Once
	oaVal  validator.Validator
	oaErr  []error
)

// openAPIValidator validates requests and responses against
// docs/api/openapi.yaml, format assertions (date-time, uri) included.
func openAPIValidator(t *testing.T) validator.Validator {
	t.Helper()
	oaOnce.Do(func() {
		doc, err := libopenapi.NewDocument(apidocs.OpenAPIYAML)
		if err != nil {
			oaErr = []error{err}
			return
		}
		// Auth is tested directly; the validator can't model "a session
		// or, with OIDC off, nothing".
		oaVal, oaErr = validator.NewValidator(doc, config.WithFormatAssertions(), config.WithoutSecurityValidation(),
			// The radix tree takes /v1/posts/pst_1:cancel for /v1/posts/{id};
			// the regex matcher behind it knows {id}:cancel.
			config.WithPathTree(regexOnly{}))
	})
	if len(oaErr) > 0 {
		t.Fatalf("openapi: %v", oaErr)
	}
	return oaVal
}

// regexOnly is a path lookup that finds nothing, so the validator matches
// every path with its templates' regular expressions.
type regexOnly struct{}

func (regexOnly) Lookup(string) (*v3.PathItem, string, bool) { return nil, "", false }

type v1Env struct {
	t      *testing.T
	api    *API
	h      http.Handler
	st     *store.Store
	cookie *http.Cookie // a signed-in session (OIDC on only)
	reg    *accounts.Registry
	// lookups are the accounts that looked posts up, in order.
	lookups []string
}

// newV1Env serves the real Routes() over a fresh store with two seeded
// accounts. oidc turns sign-in on (a mock provider); off, the reverse proxy
// is the gate.
func newV1Env(t *testing.T, oidc bool) *v1Env {
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
	a := &API{Store: st, Version: "v9.9.9-test", UserLanguages: []string{"en", "fr"}}
	e := &v1Env{t: t, api: a, st: st}
	if oidc {
		au, err := auth.New(context.Background(), auth.Config{
			Issuer: newMockOIDC(t), ClientID: "client-1", ClientSecret: "s", RedirectURL: "https://app/auth/callback",
		})
		if err != nil {
			t.Fatal(err)
		}
		a.Auth, a.Allowlist, a.SessionTTL = au, auth.NewAllowlist([]string{"sub-1"}, nil), time.Hour
		u, err := st.UpsertUser("sub-1", "ann@example.com", "Ann")
		if err != nil {
			t.Fatal(err)
		}
		raw, err := st.CreateSession(u.ID, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		e.cookie = &http.Cookie{Name: sessionCookie, Value: raw}
	}
	e.h = a.Routes()
	return e
}

// token mints a token with scopes and returns its secret.
func (e *v1Env) token(scopes ...string) string {
	e.t.Helper()
	_, raw, err := e.st.CreateScopedToken("test", scopes, time.Time{})
	if err != nil {
		e.t.Fatal(err)
	}
	return raw
}

type v1Resp struct {
	*httptest.ResponseRecorder
	t *testing.T
}

func (r v1Resp) json(v any) {
	r.t.Helper()
	if err := json.Unmarshal(r.Body.Bytes(), v); err != nil {
		r.t.Fatalf("decode %s: %v", r.Body.String(), err)
	}
}

func (r v1Resp) problem() problemBody {
	r.t.Helper()
	if ct := r.Header().Get("Content-Type"); ct != "application/problem+json" {
		r.t.Fatalf("Content-Type = %q, want application/problem+json (body %s)", ct, r.Body.String())
	}
	var p problemBody
	r.json(&p)
	return p
}

// req is one /v1 call. auth: "" sends the session cookie (if any), "none"
// sends nothing, anything else is sent as a bearer token.
type req struct {
	method, path, auth, body string
	header                   map[string]string
	// badRequest: the request is meant to break the document's rules, so
	// only the response is validated.
	badRequest bool
	// undocumented: no operation matches (an unknown route or method), so
	// nothing is validated.
	undocumented bool
	// ctx ends the request (a stream that otherwise runs on).
	ctx context.Context
}

func (e *v1Env) do(rq req) v1Resp {
	e.t.Helper()
	var body io.Reader
	if rq.body != "" {
		body = strings.NewReader(rq.body)
	}
	r := httptest.NewRequest(rq.method, rq.path, body)
	if rq.ctx != nil {
		r = r.WithContext(rq.ctx)
	}
	if rq.body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	switch rq.auth {
	case "":
		if e.cookie != nil {
			r.AddCookie(e.cookie)
		}
	case "none":
	default:
		r.Header.Set("Authorization", "Bearer "+rq.auth)
	}
	for k, v := range rq.header {
		if v == "" {
			r.Header.Del(k)
		} else {
			r.Header.Set(k, v)
		}
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)

	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		e.t.Errorf("%s %s: Cache-Control = %q, want no-store", rq.method, rq.path, cc)
	}
	if rq.undocumented {
		return v1Resp{rec, e.t}
	}
	v := openAPIValidator(e.t)
	if !rq.badRequest {
		vr := httptest.NewRequest(rq.method, rq.path, strings.NewReader(rq.body))
		vr.Header = r.Header.Clone()
		if ok, errs := v.ValidateHttpRequest(vr); !ok {
			e.t.Errorf("%s %s: request breaks the OpenAPI document: %s", rq.method, rq.path, validationText(errs))
		}
	}
	vr := httptest.NewRequest(rq.method, rq.path, strings.NewReader(rq.body))
	vr.Header = r.Header.Clone()
	res := rec.Result()
	res.Body = io.NopCloser(bytes.NewReader(rec.Body.Bytes()))
	if ok, errs := v.ValidateHttpResponse(vr, res); !ok {
		e.t.Errorf("%s %s: %d response breaks the OpenAPI document: %s\nbody: %s",
			rq.method, rq.path, rec.Code, validationText(errs), rec.Body.String())
	}
	return v1Resp{rec, e.t}
}

func validationText(errs any) string {
	b, _ := json.MarshalIndent(errs, "", "  ")
	return string(b)
}

func wantStatus(t *testing.T, r v1Resp, status int) {
	t.Helper()
	if r.Code != status {
		t.Fatalf("status = %d, want %d; body %s", r.Code, status, r.Body.String())
	}
}

func wantProblem(t *testing.T, r v1Resp, status int, code string) problemBody {
	t.Helper()
	wantStatus(t, r, status)
	p := r.problem()
	if p.Code != code || p.Status != status || p.Type != problemTypeBase+code || p.Title != v1Codes[code] {
		t.Fatalf("problem = %+v, want code %s status %d", p, code, status)
	}
	return p
}

// ─── the document ───────────────────────────────────────────────────────

// docOperations lists the document's operations as "METHOD /path".
func docOperations(t *testing.T) []string {
	t.Helper()
	b, err := apidocs.OpenAPIJSON()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		OpenAPI string                                `json:"openapi"`
		Paths   map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.1") {
		t.Fatalf("openapi = %q, want 3.1.x", doc.OpenAPI)
	}
	var out []string
	for p, ops := range doc.Paths {
		for m := range ops {
			switch m {
			case "get", "put", "post", "delete", "patch", "head", "options":
				out = append(out, strings.ToUpper(m)+" "+p)
			}
		}
	}
	sort.Strings(out)
	return out
}

func TestOpenAPIDocumentIsValid(t *testing.T) {
	if ok, errs := openAPIValidator(t).ValidateDocument(); !ok {
		t.Fatalf("the OpenAPI document is invalid: %s", validationText(errs))
	}
}

// TestV1RoutesMatchDocument: every route is documented and every documented
// operation is served.
func TestV1RoutesMatchDocument(t *testing.T) {
	var routes []string
	for _, rt := range (&API{}).v1Routes() {
		if len(rt.Actions) > 0 {
			for _, act := range rt.Actions {
				routes = append(routes, rt.Method+" "+rt.Path+":"+act)
			}
			continue
		}
		routes = append(routes, rt.Method+" "+rt.Path)
	}
	sort.Strings(routes)
	if docs := docOperations(t); !slices.Equal(routes, docs) {
		t.Fatalf("routes and document differ:\nroutes: %v\ndoc:    %v", routes, docs)
	}
}

// TestV1ErrorCatalogue: every code the server can send has a section in
// docs/api/errors.md and a place in the document's Problem.code enum, and
// nothing is documented that the server doesn't send.
func TestV1ErrorCatalogue(t *testing.T) {
	var codes []string
	for c := range v1Codes {
		codes = append(codes, c)
	}
	sort.Strings(codes)

	var md []string
	for _, m := range regexp.MustCompile(`(?m)^## ([a-z_]+)$`).FindAllStringSubmatch(string(apidocs.ErrorsMarkdown), -1) {
		md = append(md, m[1])
	}
	sort.Strings(md)
	if !slices.Equal(codes, md) {
		t.Errorf("errors.md sections differ from the codes:\ncodes: %v\nmd:    %v", codes, md)
	}

	b, _ := apidocs.OpenAPIJSON()
	var doc struct {
		Components struct {
			Schemas struct {
				Problem struct {
					Properties struct {
						Code struct {
							Enum []string `json:"enum"`
						} `json:"code"`
					} `json:"properties"`
				} `json:"Problem"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	enum := slices.Clone(doc.Components.Schemas.Problem.Properties.Code.Enum)
	sort.Strings(enum)
	if !slices.Equal(codes, enum) {
		t.Errorf("Problem.code enum differs from the codes:\ncodes: %v\nenum:  %v", codes, enum)
	}
}

// TestV1Conformance calls every route once, signed in, and checks each
// request and response against the document (the harness does that on
// every call in this file).
func TestV1Conformance(t *testing.T) {
	e := newV1Env(t, true)
	accts, err := e.st.Accounts(context.Background(), store.DefaultOwner)
	if err != nil || len(accts) != 2 {
		t.Fatalf("accounts = %v, %v", accts, err)
	}
	tok, _, err := e.st.CreateScopedToken("conf", []string{"admin"}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	e.seedPost("pub", "success", "bluesky")
	e.seedScheduled("sch", time.Now().Add(time.Hour), "bluesky")
	e.seedPost("fail", "failed", "mastodon")
	unknownPath, unknownTarget := e.seedUnknown("unk", "bluesky")
	e.withEngine()
	e.api.Translator = v1FakeTranslator{}
	e.api.Verify = &v1FakeVerifier{}
	e.api.Sync = &v1FakeSyncer{}
	e.api.PublicFeedToken = "feed-secret"
	relays := "/v1/accounts/" + e.withNostr() + "/nostr/relays"
	med := e.upload("conf")
	png := string(pngBytes(t, 4, 4))
	imgHdr := map[string]string{"Content-Type": "image/png"}
	newPost := `{"content":{"text":"hi","media":[{"id":"` + med + `"}]},"targets":[{"account":"` + e.acct("bluesky") + `"}]}`
	drf := &store.V1Draft{OwnerID: store.DefaultOwner, Text: "draft", Body: `{"content":{"text":"draft"},"targets":[]}`}
	if err := e.st.CreateV1Draft(context.Background(), drf); err != nil {
		t.Fatal(err)
	}
	drfPath := "/v1/drafts/" + draftAPIID(drf.ID)
	stream, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	calls := []struct {
		pattern string
		req     req
		status  int
	}{
		{"GET /v1/openapi.json", req{method: "GET", path: "/v1/openapi.json", auth: "none"}, 200},
		{"GET /v1/instance", req{method: "GET", path: "/v1/instance"}, 200},
		{"GET /v1/destinations", req{method: "GET", path: "/v1/destinations"}, 200},
		{"GET /v1/accounts", req{method: "GET", path: "/v1/accounts"}, 200},
		{"GET /v1/accounts/{id}", req{method: "GET", path: "/v1/accounts/" + accts[0].ID}, 200},
		{"PATCH /v1/accounts/{id}", req{method: "PATCH", path: "/v1/accounts/" + accts[0].ID,
			header: map[string]string{"If-Match": "*"}, body: `{"label":"Work","default_selected":false}`}, 200},
		{"GET /v1/me", req{method: "GET", path: "/v1/me"}, 200},
		{"GET /v1/tokens", req{method: "GET", path: "/v1/tokens"}, 200},
		{"POST /v1/tokens", req{method: "POST", path: "/v1/tokens", body: `{"name":"ci","scopes":["posts:read"],"expires_at":"2099-01-01T00:00:00Z"}`}, 201},
		{"GET /v1/tokens/{id}", req{method: "GET", path: "/v1/tokens/" + tokenID(tok.ID)}, 200},
		{"DELETE /v1/tokens/{id}", req{method: "DELETE", path: "/v1/tokens/" + tokenID(tok.ID)}, 204},
		{"GET /v1/posts", req{method: "GET", path: "/v1/posts"}, 200},
		{"POST /v1/posts", req{method: "POST", path: "/v1/posts", body: newPost, header: map[string]string{"Idempotency-Key": `"conf-1"`}}, 202},
		{"POST /v1/posts:preview", req{method: "POST", path: "/v1/posts:preview", body: newPost}, 200},
		{"POST /v1/resolve", req{method: "POST", path: "/v1/resolve", body: `{"url":"` + bskySource + `"}`}, 200},
		{"POST /v1/media", req{method: "POST", path: "/v1/media?alt=x", body: png, header: imgHdr}, 201},
		{"POST /v1/media:compress", req{method: "POST", path: "/v1/media:compress?preset=small", body: png, header: imgHdr}, 200},
		{"GET /v1/media/{id}", req{method: "GET", path: "/v1/media/" + med}, 200},
		{"PATCH /v1/media/{id}", req{method: "PATCH", path: "/v1/media/" + med, body: `{"alt":"new"}`}, 200},
		{"GET /v1/posts/{id}", req{method: "GET", path: "/v1/posts/pst_pub"}, 200},
		{"PATCH /v1/posts/{id}", req{method: "PATCH", path: "/v1/posts/pst_sch", body: `{"schedule":{"at":"2099-01-01T09:00:00Z"}}`}, 200},
		{"POST /v1/posts/{id}:cancel", req{method: "POST", path: "/v1/posts/pst_sch:cancel"}, 200},
		{"POST /v1/posts/{id}:retry", req{method: "POST", path: "/v1/posts/pst_fail:retry"}, 202},
		{"POST /v1/posts/{id}:resolve", req{method: "POST", path: unknownPath + ":resolve",
			body: `{"target":"` + unknownTarget + `","ordinal":1,"outcome":"not_published"}`}, 200},
		{"GET /v1/posts/{id}/events", req{method: "GET", path: "/v1/posts/pst_pub/events"}, 200},
		{"GET /v1/events", req{method: "GET", path: "/v1/events?after=0", ctx: stream}, 200},
		{"POST /v1/translate", req{method: "POST", path: "/v1/translate", body: `{"text":"hi","target_lang":"fr"}`}, 200},
		{"POST /v1/verify", req{method: "POST", path: "/v1/verify", body: `{"input":"https://bsky.app/profile/a/post/b"}`}, 200},
		{"PUT /v1/accounts/{id}/nostr/relays", req{method: "PUT", path: relays, body: `{"secondary":["wss://a.example"]}`}, 200},
		{"GET /v1/accounts/{id}/nostr/relays", req{method: "GET", path: relays}, 200},
		{"POST /v1/accounts/{id}/nostr/relays:scan", req{method: "POST", path: relays + ":scan", body: `{"relays":["wss://a.example"]}`}, 200},
		{"POST /v1/accounts/{id}/nostr/relays:sync", req{method: "POST", path: relays + ":sync", body: `{"direction":"pull"}`}, 200},
		{"GET /v1/drafts", req{method: "GET", path: "/v1/drafts?q=dr&tag=x"}, 200},
		{"POST /v1/drafts", req{method: "POST", path: "/v1/drafts", body: `{"title":"t","tags":["x"],"post":` + newPost + `}`}, 201},
		{"GET /v1/drafts/{id}", req{method: "GET", path: drfPath}, 200},
		{"PATCH /v1/drafts/{id}", req{method: "PATCH", path: drfPath, header: map[string]string{"If-Match": `"1"`}, body: `{"title":"new"}`}, 200},
		{"POST /v1/drafts/{id}:translate", req{method: "POST", path: drfPath + ":translate", body: `{"target_lang":"fr"}`}, 201},
		{"DELETE /v1/drafts/{id}", req{method: "DELETE", path: drfPath, header: map[string]string{"If-Match": `"2"`}}, 204},
		{"GET /v1/public/feed", req{method: "GET", path: "/v1/public/feed?limit=5", auth: "feed-secret"}, 200},
	}
	seen := map[string]bool{}
	for _, c := range calls {
		r := e.do(c.req)
		if r.Code != c.status {
			t.Errorf("%s: status %d, want %d; body %s", c.pattern, r.Code, c.status, r.Body.String())
		}
		seen[c.pattern] = true
	}
	for _, rt := range e.api.v1Routes() {
		patterns := []string{rt.Method + " " + rt.Path}
		if len(rt.Actions) > 0 {
			patterns = nil
			for _, act := range rt.Actions {
				patterns = append(patterns, rt.Method+" "+rt.Path+":"+act)
			}
		}
		for _, p := range patterns {
			if !seen[p] {
				t.Errorf("%s is not exercised by TestV1Conformance", p)
			}
		}
	}
}

// ─── auth ───────────────────────────────────────────────────────────────

func TestV1AuthOIDC(t *testing.T) {
	e := newV1Env(t, true)

	r := e.do(req{method: "GET", path: "/v1/me", auth: "none"})
	wantProblem(t, r, 401, codeUnauthenticated)
	if !strings.HasPrefix(r.Header().Get("WWW-Authenticate"), "Bearer") {
		t.Errorf("WWW-Authenticate = %q", r.Header().Get("WWW-Authenticate"))
	}

	r = e.do(req{method: "GET", path: "/v1/me", auth: "none", header: map[string]string{"Authorization": "Basic YTpi"}, badRequest: true})
	wantProblem(t, r, 401, codeUnauthenticated)

	r = e.do(req{method: "GET", path: "/v1/me", auth: "not-a-token"})
	wantProblem(t, r, 401, codeUnauthenticated)
	if !strings.Contains(r.Header().Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Errorf("WWW-Authenticate = %q, want invalid_token", r.Header().Get("WWW-Authenticate"))
	}

	// A bearer header wins over the session cookie, even when it's bad.
	rq := httptest.NewRequest("GET", "/v1/me", nil)
	rq.AddCookie(e.cookie)
	rq.Header.Set("Authorization", "Bearer nope")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, rq)
	if rec.Code != 401 {
		t.Errorf("bad bearer + good cookie: %d, want 401", rec.Code)
	}

	// The session: every scope.
	r = e.do(req{method: "GET", path: "/v1/me"})
	wantStatus(t, r, 200)
	var me v1MeBody
	r.json(&me)
	if me.Kind != "session" || me.User == nil || me.User.Subject != "sub-1" || !slices.Equal(me.Scopes, v1Scopes) {
		t.Errorf("me = %+v", me)
	}

	// A token: its own scopes, and only routes they cover.
	read := e.token(scopeAccountsRead)
	r = e.do(req{method: "GET", path: "/v1/me", auth: read})
	wantStatus(t, r, 200)
	me = v1MeBody{}
	r.json(&me)
	if me.Kind != "token" || me.Token == nil || !slices.Equal(me.Scopes, []string{scopeAccountsRead}) || me.User != nil {
		t.Errorf("token me = %+v", me)
	}
	wantStatus(t, e.do(req{method: "GET", path: "/v1/accounts", auth: read}), 200)
	r = e.do(req{method: "GET", path: "/v1/tokens", auth: read})
	wantProblem(t, r, 403, codeInsufficientScope)
	if h := r.Header().Get("WWW-Authenticate"); !strings.Contains(h, `error="insufficient_scope"`) || !strings.Contains(h, `scope="admin"`) {
		t.Errorf("WWW-Authenticate = %q", h)
	}

	// admin includes every scope.
	wantStatus(t, e.do(req{method: "GET", path: "/v1/accounts", auth: e.token(scopeAdmin)}), 200)

	// A token from before scopes keeps posting rights only.
	_, legacy, err := e.st.CreateAPIToken("n8n")
	if err != nil {
		t.Fatal(err)
	}
	wantProblem(t, e.do(req{method: "GET", path: "/v1/accounts", auth: legacy}), 403, codeInsufficientScope)

	// Expired and revoked tokens stop working.
	_, expired, _ := e.st.CreateScopedToken("old", []string{scopeAdmin}, time.Now().Add(-time.Minute))
	wantProblem(t, e.do(req{method: "GET", path: "/v1/me", auth: expired}), 401, codeUnauthenticated)
	tok, revoked, _ := e.st.CreateScopedToken("gone", []string{scopeAdmin}, time.Time{})
	_ = e.st.RevokeAPIToken(tok.ID)
	wantProblem(t, e.do(req{method: "GET", path: "/v1/me", auth: revoked}), 401, codeUnauthenticated)

	// The OpenAPI document is public.
	wantStatus(t, e.do(req{method: "GET", path: "/v1/openapi.json", auth: "none"}), 200)
}

func TestV1AuthProxyMode(t *testing.T) {
	e := newV1Env(t, false)
	r := e.do(req{method: "GET", path: "/v1/me", auth: "none"})
	wantStatus(t, r, 200)
	var me v1MeBody
	r.json(&me)
	if me.Kind != "proxy" || !slices.Equal(me.Scopes, v1Scopes) {
		t.Errorf("me = %+v", me)
	}
	// A token sent with OIDC off is still checked.
	wantProblem(t, e.do(req{method: "GET", path: "/v1/me", auth: "bogus"}), 401, codeUnauthenticated)
	wantProblem(t, e.do(req{method: "GET", path: "/v1/tokens", auth: e.token(scopePostsRead)}), 403, codeInsufficientScope)
}

// TestV1PathTricksStayGated: a /v1 prefix can't smuggle a request past the
// session gate to the web app.
func TestV1PathTricksStayGated(t *testing.T) {
	e := newV1Env(t, true)
	rq := httptest.NewRequest("GET", "/v1/../index.html", nil)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, rq)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/v1/../index.html = %d without a session, want 401", rec.Code)
	}
}

// The pre-1.0 routes are gone, and say where they went: 410 with a pointer
// to the migration guide, signed in or not.
func TestOldRoutesAreGone(t *testing.T) {
	e := newV1Env(t, true)
	for _, rt := range []string{"POST /publish", "POST /upload-media", "GET /api/posts", "POST /api/post",
		"GET /api/drafts", "GET /api/config", "POST /api/tokens"} {
		method, path, _ := strings.Cut(rt, " ")
		rq := httptest.NewRequest(method, path, nil)
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, rq)
		if rec.Code != http.StatusGone || !strings.Contains(rec.Body.String(), "/v1") ||
			!strings.Contains(rec.Body.String(), "migrating-from-the-pre-10-api") {
			t.Errorf("%s: %d %s", rt, rec.Code, rec.Body.String())
		}
	}
}

// ─── routing and problems ───────────────────────────────────────────────

func TestV1UnknownRouteAndMethod(t *testing.T) {
	e := newV1Env(t, false)
	p := wantProblem(t, e.do(req{method: "GET", path: "/v1/nope", undocumented: true}), 404, codeNotFound)
	if p.Instance != "/v1/nope" {
		t.Errorf("instance = %q", p.Instance)
	}
	r := e.do(req{method: "PUT", path: "/v1/tokens", undocumented: true})
	wantProblem(t, r, 405, codeMethodNotAllowed)
	if got := r.Header().Get("Allow"); got != "GET, HEAD, POST" {
		t.Errorf("Allow = %q, want GET, HEAD, POST", got)
	}
}

func TestV1Recover(t *testing.T) {
	rec := httptest.NewRecorder()
	v1Recover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })).
		ServeHTTP(rec, httptest.NewRequest("GET", "/v1/x", nil))
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), `"code":"internal"`) {
		t.Fatalf("panic → %d %s", rec.Code, rec.Body.String())
	}
}

// ─── resources ──────────────────────────────────────────────────────────

func TestV1Instance(t *testing.T) {
	e := newV1Env(t, false)
	e.api.Translator = v1FakeTranslator{}
	e.api.PublicFeedToken = "x"
	e.h = e.api.Routes()
	r := e.do(req{method: "GET", path: "/v1/instance"})
	wantStatus(t, r, 200)
	var in v1InstanceBody
	r.json(&in)
	if in.Version != "v9.9.9-test" || in.Auth != "proxy" || !in.Features.Translate || !in.Features.PublicFeed ||
		in.Features.Video || !slices.Equal(in.UserLanguages, []string{"en", "fr"}) ||
		!slices.Equal(in.TranslateTargets, []string{"en", "fr"}) || in.Idempotency.TTLSecs != 86400 ||
		!slices.Contains(in.Destinations, "bluesky") || in.Limits.MaxJSONBodyBytes != maxV1Body {
		t.Errorf("instance = %+v", in)
	}
}

func TestV1DestinationsAndAccounts(t *testing.T) {
	e := newV1Env(t, false)
	r := e.do(req{method: "GET", path: "/v1/destinations"})
	wantStatus(t, r, 200)
	var dests struct {
		Data []struct {
			Type string `json:"type"`
		} `json:"data"`
		NextCursor *string `json:"next_cursor"`
	}
	r.json(&dests)
	if len(dests.Data) != 4 || dests.Data[0].Type != "bluesky" || dests.NextCursor != nil {
		t.Errorf("destinations = %+v", dests)
	}

	var page listBody[v1AccountBody]
	r = e.do(req{method: "GET", path: "/v1/accounts?limit=1"})
	wantStatus(t, r, 200)
	r.json(&page)
	// Seeded together, the accounts share created_at; the id breaks the tie.
	if len(page.Data) != 1 || page.NextCursor == nil || !strings.HasPrefix(page.Data[0].ID, "acc_") {
		t.Fatalf("page 1 = %+v", page)
	}
	first, firstType := page.Data[0].ID, page.Data[0].Type
	if want := map[string]int{"bluesky": 300, "mastodon": 500}[firstType]; page.Data[0].Capabilities.Text.MaxLength != want {
		t.Errorf("%s max_length = %d, want %d", firstType, page.Data[0].Capabilities.Text.MaxLength, want)
	}
	r = e.do(req{method: "GET", path: "/v1/accounts?limit=1&cursor=" + *page.NextCursor})
	wantStatus(t, r, 200)
	r.json(&page)
	if len(page.Data) != 1 || page.Data[0].Type == firstType || page.NextCursor != nil {
		t.Fatalf("page 2 = %+v", page)
	}

	r = e.do(req{method: "GET", path: "/v1/accounts/" + first})
	wantStatus(t, r, 200)
	var one v1AccountBody
	r.json(&one)
	if one.ID != first || !one.Enabled || one.Source != "env" {
		t.Errorf("account = %+v", one)
	}
	wantProblem(t, e.do(req{method: "GET", path: "/v1/accounts/acc_nope"}), 404, codeNotFound)

	wantProblem(t, e.do(req{method: "GET", path: "/v1/accounts?limit=0", badRequest: true}), 400, codeInvalidParameter)
	wantProblem(t, e.do(req{method: "GET", path: "/v1/accounts?limit=500", badRequest: true}), 400, codeInvalidParameter)
	wantProblem(t, e.do(req{method: "GET", path: "/v1/accounts?cursor=%25%25"}), 400, codeInvalidCursor)
}

// ─── tokens ─────────────────────────────────────────────────────────────

func TestV1TokenLifecycle(t *testing.T) {
	e := newV1Env(t, true)
	r := e.do(req{method: "POST", path: "/v1/tokens", body: `{"name":"  n8n  ","scopes":["media:write","posts:write","posts:write"]}`})
	wantStatus(t, r, 201)
	var created v1CreatedTokenBody
	r.json(&created)
	if created.Name != "n8n" || !slices.Equal(created.Scopes, []string{"posts:write", "media:write"}) ||
		created.Status != "active" || created.ExpiresAt != nil || created.Secret == "" ||
		r.Header().Get("Location") != "/v1/tokens/"+created.ID || !strings.HasPrefix(created.ID, "tok_") {
		t.Fatalf("created = %+v (Location %q)", created, r.Header().Get("Location"))
	}

	// The secret works, with its scopes and nothing more.
	r = e.do(req{method: "GET", path: "/v1/me", auth: created.Secret})
	wantStatus(t, r, 200)
	wantProblem(t, e.do(req{method: "GET", path: "/v1/tokens", auth: created.Secret}), 403, codeInsufficientScope)

	// Get never shows the secret.
	r = e.do(req{method: "GET", path: "/v1/tokens/" + created.ID})
	wantStatus(t, r, 200)
	if strings.Contains(r.Body.String(), created.Secret) || strings.Contains(r.Body.String(), `"secret"`) {
		t.Fatalf("GET shows the secret: %s", r.Body.String())
	}
	var got v1TokenBody
	r.json(&got)
	if got.LastUsedAt == nil {
		t.Errorf("last_used_at not set after use: %+v", got)
	}

	// Revoke, twice: both 204, and the secret stops working.
	wantStatus(t, e.do(req{method: "DELETE", path: "/v1/tokens/" + created.ID}), 204)
	wantStatus(t, e.do(req{method: "DELETE", path: "/v1/tokens/" + created.ID}), 204)
	wantProblem(t, e.do(req{method: "GET", path: "/v1/me", auth: created.Secret}), 401, codeUnauthenticated)
	r = e.do(req{method: "GET", path: "/v1/tokens/" + created.ID})
	r.json(&got)
	if got.Status != "revoked" || got.RevokedAt == nil {
		t.Errorf("revoked token = %+v", got)
	}

	for _, id := range []string{"nope", "tok_", "tok_nope", strings.TrimPrefix(created.ID, "tok_")} {
		wantProblem(t, e.do(req{method: "GET", path: "/v1/tokens/" + id}), 404, codeNotFound)
		wantProblem(t, e.do(req{method: "DELETE", path: "/v1/tokens/" + id}), 404, codeNotFound)
	}
}

func TestV1TokenExpiryAndStatus(t *testing.T) {
	e := newV1Env(t, true)
	exp := time.Now().Add(2 * time.Second).UTC().Format(time.RFC3339)
	r := e.do(req{method: "POST", path: "/v1/tokens", body: `{"name":"short","scopes":["admin"],"expires_at":"` + exp + `"}`})
	wantStatus(t, r, 201)
	var c v1CreatedTokenBody
	r.json(&c)
	if c.ExpiresAt == nil || *c.ExpiresAt != exp {
		t.Fatalf("expires_at = %v, want %s", c.ExpiresAt, exp)
	}
	sid, _ := parseID(c.ID, prefixToken)
	tok, err := e.st.APIToken(sid)
	if err != nil {
		t.Fatal(err)
	}
	tok.ExpiresAt = time.Now().Add(-time.Second)
	if v1Token(tok).Status != "expired" {
		t.Errorf("status of a past-expiry token = %q", v1Token(tok).Status)
	}
}

func TestV1TokenValidation(t *testing.T) {
	e := newV1Env(t, true)
	r := e.do(req{method: "POST", path: "/v1/tokens", badRequest: true,
		body: `{"name":"","scopes":["posts:read","root"],"expires_at":"2001-01-01T00:00:00Z"}`})
	p := wantProblem(t, r, 422, codeValidation)
	var ptrs []string
	for _, fe := range p.Errors {
		ptrs = append(ptrs, fe.Pointer+":"+fe.Code)
	}
	if want := []string{"/name:required", "/scopes/1:unknown_scope", "/expires_at:in_past"}; !slices.Equal(ptrs, want) {
		t.Errorf("errors = %v, want %v", ptrs, want)
	}

	cases := []struct {
		name, body, ct string
		status         int
		code           string
	}{
		{"empty scopes", `{"name":"x","scopes":[]}`, "", 422, codeValidation},
		{"long name", `{"name":"` + strings.Repeat("é", 101) + `","scopes":["admin"]}`, "", 422, codeValidation},
		{"bad time", `{"name":"x","scopes":["admin"],"expires_at":"tomorrow"}`, "", 422, codeValidation},
		{"unknown field", `{"name":"x","scopes":["admin"],"admin":true}`, "", 400, codeInvalidJSON},
		{"wrong type", `{"name":1,"scopes":["admin"]}`, "", 400, codeInvalidJSON},
		{"trailing data", `{"name":"x","scopes":["admin"]} {}`, "", 400, codeInvalidJSON},
		{"not json", `name=x`, "", 400, codeInvalidJSON},
		{"form body", `name=x`, "application/x-www-form-urlencoded", 415, codeUnsupportedMediaType},
		{"no content type", `{"name":"x","scopes":["admin"]}`, "-", 415, codeUnsupportedMediaType},
		{"json with charset", `{"name":"x","scopes":["admin"]}`, "application/json; charset=utf-8", 201, ""},
		{"jsonp", `{"name":"x","scopes":["admin"]}`, "application/jsonp", 415, codeUnsupportedMediaType},
		{"control char", `{"name":"a\u0000b","scopes":["admin"]}`, "", 422, codeValidation},
		{"bidi override", `{"name":"ci \u202etoor\u202c","scopes":["admin"]}`, "", 422, codeValidation},
		{"lowercase t and z", `{"name":"x","scopes":["admin"],"expires_at":"2099-01-01t00:00:00z"}`, "", 201, ""},
		{"too large", `{"name":"` + strings.Repeat("a", maxV1Body) + `","scopes":["admin"]}`, "", 413, codeTooLarge},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := map[string]string{}
			switch c.ct {
			case "":
			case "-":
				h["Content-Type"] = "" // deleted
			default:
				h["Content-Type"] = c.ct
			}
			r := e.do(req{method: "POST", path: "/v1/tokens", body: c.body, header: h, badRequest: true})
			if c.code == "" {
				wantStatus(t, r, c.status)
				return
			}
			wantProblem(t, r, c.status, c.code)
		})
	}
	// No body at all.
	wantProblem(t, e.do(req{method: "POST", path: "/v1/tokens", badRequest: true,
		header: map[string]string{"Content-Type": "application/json"}}), 400, codeInvalidJSON)
}

// TestV1TokenCantOutliveItsMaker: a token minting a token can't give it a
// longer life than its own.
func TestV1TokenCantOutliveItsMaker(t *testing.T) {
	e := newV1Env(t, true)
	parentExp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	_, parent, err := e.st.CreateScopedToken("short", []string{scopeAdmin}, parentExp)
	if err != nil {
		t.Fatal(err)
	}
	later := parentExp.Add(time.Minute).Format(time.RFC3339)
	sooner := parentExp.Add(-time.Minute).Format(time.RFC3339)
	for body, want := range map[string]int{
		`{"name":"x","scopes":["admin"]}`:                               422, // never expires
		`{"name":"x","scopes":["admin"],"expires_at":"` + later + `"}`:  422,
		`{"name":"x","scopes":["admin"],"expires_at":"` + sooner + `"}`: 201,
	} {
		r := e.do(req{method: "POST", path: "/v1/tokens", auth: parent, body: body, badRequest: want != 201})
		if r.Code != want {
			t.Errorf("%s: %d, want %d (%s)", body, r.Code, want, r.Body.String())
		}
		if want == 422 && !strings.Contains(r.Body.String(), `"exceeds_caller"`) {
			t.Errorf("%s: %s, want exceeds_caller", body, r.Body.String())
		}
	}
	// A session or a non-expiring token has no such limit.
	wantStatus(t, e.do(req{method: "POST", path: "/v1/tokens", body: `{"name":"x","scopes":["admin"]}`}), 201)
}

func TestV1TokenListPages(t *testing.T) {
	e := newV1Env(t, true)
	var made []string
	for range 5 {
		tok, _, err := e.st.CreateScopedToken("t", []string{scopePostsRead}, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		made = append(made, tokenID(tok.ID))
	}
	var seen []string
	path := "/v1/tokens?limit=2"
	for range 10 {
		r := e.do(req{method: "GET", path: path})
		wantStatus(t, r, 200)
		var page listBody[v1TokenBody]
		r.json(&page)
		for _, tk := range page.Data {
			seen = append(seen, tk.ID)
		}
		if page.NextCursor == nil {
			break
		}
		// A token made mid-listing sorts first, so it doesn't shift the
		// pages after the cursor.
		if len(seen) == 2 {
			if _, _, err := e.st.CreateScopedToken("late", []string{scopePostsRead}, time.Time{}); err != nil {
				t.Fatal(err)
			}
		}
		path = "/v1/tokens?limit=2&cursor=" + *page.NextCursor
	}
	// The expected order is the store's (created_at DESC, id DESC): on a
	// coarse clock tokens can share a created_at, and the id breaks ties.
	all, err := e.st.ListAPITokens()
	if err != nil {
		t.Fatal(err)
	}
	made = made[:0]
	for _, tk := range all {
		if tk.Name == "t" {
			made = append(made, tokenID(tk.ID))
		}
	}
	if !slices.Equal(seen, made) {
		t.Fatalf("listed %v, want %v", seen, made)
	}
}

func TestAbsoluteHTTPURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://cdn.example/a.png":     "https://cdn.example/a.png",
		"http://x/a":                    "http://x/a",
		"/avatars/original/missing.png": "",
		"javascript:alert(1)":           "",
		"data:image/png;base64,AAAA":    "",
		"https:///nohost":               "",
		"":                              "",
	} {
		if got := absoluteHTTPURL(in); got != want {
			t.Errorf("absoluteHTTPURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestV1AccountWithoutDriver: an account whose type has no registered
// driver is listed without capabilities, and still validates.
func TestV1AccountWithoutDriver(t *testing.T) {
	e := newV1Env(t, false)
	if _, err := e.st.SeedEnvAccounts(context.Background(), store.DefaultOwner,
		[]store.EnvAccount{{Type: "gone", Label: "Gone"}}); err != nil {
		t.Fatal(err)
	}
	r := e.do(req{method: "GET", path: "/v1/accounts"})
	wantStatus(t, r, 200)
	var page listBody[v1AccountBody]
	r.json(&page)
	for _, ac := range page.Data {
		if (ac.Type == "gone") != (ac.Capabilities == nil) {
			t.Errorf("%s: capabilities = %v", ac.Type, ac.Capabilities)
		}
	}
}

// TestV1RecoverAfterPartialWrite: a panic after the response began aborts
// the connection rather than appending a problem to it.
func TestV1RecoverAfterPartialWrite(t *testing.T) {
	h := v1Recover(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("partial"))
		panic("boom")
	}))
	defer func() {
		if p := recover(); p != http.ErrAbortHandler {
			t.Fatalf("recovered %v, want http.ErrAbortHandler", p)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/x", nil))
}

// TestV1CrossOriginIsAProblem: the CSRF guard answers /v1 with a problem.
func TestV1CrossOriginIsAProblem(t *testing.T) {
	e := newV1Env(t, true)
	r := e.do(req{method: "POST", path: "/v1/tokens", body: `{"name":"x","scopes":["admin"]}`,
		header: map[string]string{"Origin": "https://evil.example"}})
	wantProblem(t, r, 403, codeCrossOrigin)
}
