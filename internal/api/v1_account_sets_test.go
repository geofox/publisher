package api

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/store"
)

// accountSetConformance calls each account-set route once, for
// TestV1Conformance.
func accountSetConformance(t *testing.T) map[string]bool {
	t.Helper()
	e := newV1Env(t, true)
	seen := map[string]bool{}
	call := func(pattern string, rq req, status int) v1Resp {
		t.Helper()
		r := e.do(rq)
		if r.Code != status {
			t.Errorf("%s: status %d, want %d; body %s", pattern, r.Code, status, r.Body.String())
		}
		seen[pattern] = true
		return r
	}
	var s v1AccountSetBody
	r := call("POST /v1/account-sets", req{method: "POST", path: "/v1/account-sets",
		body: `{"name":"Work","accounts":["` + e.acct("mastodon") + `"],"is_default":true}`}, 201)
	r.json(&s)
	call("GET /v1/account-sets", req{method: "GET", path: "/v1/account-sets"}, 200)
	etag := call("GET /v1/account-sets/{id}", req{method: "GET", path: "/v1/account-sets/" + s.ID}, 200).Header().Get("ETag")
	etag = call("PATCH /v1/account-sets/{id}", req{method: "PATCH", path: "/v1/account-sets/" + s.ID, header: map[string]string{"If-Match": etag},
		body: `{"accounts":["` + e.acct("mastodon") + `","` + e.acct("bluesky") + `"]}`}, 200).Header().Get("ETag")
	call("DELETE /v1/account-sets/{id}", req{method: "DELETE", path: "/v1/account-sets/" + s.ID, header: map[string]string{"If-Match": etag}}, 204)
	return seen
}

func (e *v1Env) makeSet(body string) v1AccountSetBody {
	e.t.Helper()
	r := e.do(req{method: "POST", path: "/v1/account-sets", body: body})
	wantStatus(e.t, r, 201)
	var s v1AccountSetBody
	r.json(&s)
	if r.Header().Get("Location") != "/v1/account-sets/"+s.ID || !etagRe.MatchString(r.Header().Get("ETag")) {
		e.t.Fatalf("Location %q ETag %q", r.Header().Get("Location"), r.Header().Get("ETag"))
	}
	return s
}

func (e *v1Env) sets() []v1AccountSetBody {
	e.t.Helper()
	r := e.do(req{method: "GET", path: "/v1/account-sets"})
	wantStatus(e.t, r, 200)
	var l struct{ Data []v1AccountSetBody }
	r.json(&l)
	return l.Data
}

// Sets are listed by name; one at most is the default, and making one
// the default takes it from the other.
func TestV1AccountSets(t *testing.T) {
	e := newV1Env(t, true)
	m, b := e.acct("mastodon"), e.acct("bluesky")
	work := e.makeSet(`{"name":" Work ","accounts":["` + m + `","` + b + `"],"is_default":true}`)
	if work.Name != "Work" || !slices.Equal(work.Accounts, []string{m, b}) || !work.IsDefault || !strings.HasPrefix(work.ID, "set_") {
		t.Fatalf("work = %+v", work)
	}
	art := e.makeSet(`{"name":"art","accounts":["` + b + `"],"is_default":true}`)
	got := e.sets()
	if len(got) != 2 || got[0].ID != art.ID || got[1].ID != work.ID || !got[0].IsDefault || got[1].IsDefault {
		t.Fatalf("sets = %+v", got)
	}

	// PATCH: what is sent replaces; If-Match guards a stale read.
	r := e.do(req{method: "GET", path: "/v1/account-sets/" + work.ID})
	etag := r.Header().Get("ETag")
	r = e.do(req{method: "PATCH", path: "/v1/account-sets/" + work.ID, header: map[string]string{"If-Match": etag},
		body: `{"name":"Day job","accounts":["` + b + `"]}`})
	wantStatus(t, r, 200)
	var s v1AccountSetBody
	r.json(&s)
	if s.Name != "Day job" || !slices.Equal(s.Accounts, []string{b}) || r.Header().Get("ETag") == etag {
		t.Fatalf("patched = %+v, ETag %s", s, r.Header().Get("ETag"))
	}
	r = e.do(req{method: "PATCH", path: "/v1/account-sets/" + work.ID, header: map[string]string{"If-Match": etag}, body: `{"name":"Old"}`})
	wantProblem(t, r, 412, codePreconditionFailed)
	if r.Header().Get("ETag") == etag || r.Header().Get("ETag") == "" {
		t.Errorf("412 ETag = %q", r.Header().Get("ETag"))
	}
	wantProblem(t, e.do(req{method: "PATCH", path: "/v1/account-sets/" + work.ID, header: map[string]string{"If-Match": "W/" + etag},
		body: `{}`, badRequest: true}), 400, codeInvalidParameter)
	// An empty change answers the set as it is.
	before := e.do(req{method: "GET", path: "/v1/account-sets/" + work.ID}).Header().Get("ETag")
	if r := e.do(req{method: "PATCH", path: "/v1/account-sets/" + work.ID, body: `{}`}); r.Header().Get("ETag") != before {
		t.Error("an empty PATCH moved the ETag")
	}

	// A deleted account drops out of the sets that hold it.
	if err := e.st.ForgetEnvAccount(context.Background(), store.DefaultOwner, b, "session", ""); err != nil {
		t.Fatal(err)
	}
	for _, s := range e.sets() {
		if slices.Contains(s.Accounts, b) {
			t.Errorf("set %s still lists the deleted account", s.Name)
		}
	}

	// DELETE: the selection goes, If-Match guards it.
	wantProblem(t, e.do(req{method: "DELETE", path: "/v1/account-sets/" + art.ID, header: map[string]string{"If-Match": `"1"`}}),
		412, codePreconditionFailed)
	wantStatus(t, e.do(req{method: "DELETE", path: "/v1/account-sets/" + art.ID}), 204)
	wantProblem(t, e.do(req{method: "GET", path: "/v1/account-sets/" + art.ID}), 404, codeNotFound)
	wantProblem(t, e.do(req{method: "DELETE", path: "/v1/account-sets/" + art.ID}), 404, codeNotFound)
}

// What a set refuses, each at its field.
func TestV1AccountSetRefusals(t *testing.T) {
	e := newV1Env(t, true)
	m := e.acct("mastodon")
	e.makeSet(`{"name":"Work","accounts":["` + m + `"]}`)
	other := e.makeSet(`{"name":"Other","accounts":["` + m + `"]}`)
	cases := []struct{ body, pointer, code string }{
		{`{"accounts":["` + m + `"]}`, "/name", "required"},
		{`{"name":"x"}`, "/accounts", "required"},
		{`{"name":"  ","accounts":["` + m + `"]}`, "/name", "required"},
		{`{"name":"` + strings.Repeat("n", 65) + `","accounts":["` + m + `"]}`, "/name", "too_long"},
		{`{"name":"a​b","accounts":["` + m + `"]}`, "/name", "invalid"},
		{`{"name":null,"accounts":["` + m + `"]}`, "/name", "invalid"},
		{`{"name":"work","accounts":["` + m + `"]}`, "/name", "duplicate"},
		{`{"name":"x","accounts":[]}`, "/accounts", "required"},
		{`{"name":"x","accounts":["acc_nope"]}`, "/accounts/0", "unknown_account"},
		{`{"name":"x","accounts":["` + m + `","` + m + `"]}`, "/accounts/1", "duplicate"},
		{`{"name":"x","accounts":"` + m + `"}`, "/accounts", "invalid"},
		{`{"name":"x","accounts":["` + m + `"],"is_default":null}`, "/is_default", "invalid"},
	}
	for _, c := range cases {
		p := wantProblem(t, e.do(req{method: "POST", path: "/v1/account-sets", body: c.body, badRequest: true}), 422, codeValidation)
		found := false
		for _, fe := range p.Errors {
			found = found || (fe.Pointer == c.pointer && fe.Code == c.code)
		}
		if !found {
			t.Errorf("%s: %+v, want %s at %s", c.body, p.Errors, c.code, c.pointer)
		}
	}
	// Renaming onto another set's name, whatever its case.
	p := wantProblem(t, e.do(req{method: "PATCH", path: "/v1/account-sets/" + other.ID, body: `{"name":"WORK"}`}), 422, codeValidation)
	if p.Errors[0].Pointer != "/name" || p.Errors[0].Code != "duplicate" {
		t.Errorf("rename = %+v", p.Errors)
	}
	// Its own name, in another case, is fine.
	wantStatus(t, e.do(req{method: "PATCH", path: "/v1/account-sets/" + other.ID, body: `{"name":"OTHER"}`}), 200)
	if n := len(e.sets()); n != 2 {
		t.Errorf("%d sets after refusals, want 2", n)
	}
	// At most 50.
	for i := len(e.sets()); i < maxAccountSets; i++ {
		e.makeSet(fmt.Sprintf(`{"name":"s%d","accounts":["%s"]}`, i, m))
	}
	wantProblem(t, e.do(req{method: "POST", path: "/v1/account-sets", body: `{"name":"one more","accounts":["` + m + `"]}`}), 409, codeInvalidState)
}

// Reads take accounts:read, writes accounts:write; another owner's set
// is not found.
func TestV1AccountSetScopes(t *testing.T) {
	e := newV1Env(t, true)
	s := e.makeSet(`{"name":"Work","accounts":["` + e.acct("mastodon") + `"]}`)
	read, write := e.token(scopeAccountsRead), e.token(scopeAccountsWrite)
	wantStatus(t, e.do(req{method: "GET", path: "/v1/account-sets", auth: read}), 200)
	wantStatus(t, e.do(req{method: "GET", path: "/v1/account-sets/" + s.ID, auth: read}), 200)
	wantProblem(t, e.do(req{method: "PATCH", path: "/v1/account-sets/" + s.ID, auth: read, body: `{"name":"x"}`}), 403, codeInsufficientScope)
	wantProblem(t, e.do(req{method: "DELETE", path: "/v1/account-sets/" + s.ID, auth: read}), 403, codeInsufficientScope)
	wantProblem(t, e.do(req{method: "GET", path: "/v1/account-sets", auth: e.token(scopePostsRead)}), 403, codeInsufficientScope)
	wantStatus(t, e.do(req{method: "PATCH", path: "/v1/account-sets/" + s.ID, auth: write, body: `{"name":"x"}`}), 200)
	// Behind the proxy alone, sets work like the account flags.
	px := newV1Env(t, false)
	wantStatus(t, px.do(req{method: "POST", path: "/v1/account-sets", auth: "none",
		body: `{"name":"Work","accounts":["` + px.acct("mastodon") + `"]}`}), 201)
	if _, err := e.st.AccountSet(context.Background(), "someone-else", s.ID); err == nil {
		t.Error("another owner reads the set")
	}
}

// An account's defaults: saved as checked against what it can take, and
// shown on its read.
func TestV1AccountDefaults(t *testing.T) {
	e := newV1Env(t, true)
	m, b := "/v1/accounts/"+e.acct("mastodon"), "/v1/accounts/"+e.acct("bluesky")
	r := e.do(req{method: "PATCH", path: m,
		body: `{"defaults":{"content_warning":" spoilers ","languages":["fr"],"options":{"visibility":"unlisted"}}}`})
	wantStatus(t, r, 200)
	var a v1AccountBody
	r.json(&a)
	want := v1AccountDefaults{ContentWarning: "spoilers", Languages: []string{"fr"}, Options: map[string]any{"visibility": "unlisted"}}
	if fmt.Sprint(a.Defaults) != fmt.Sprint(want) {
		t.Fatalf("defaults = %+v", a.Defaults)
	}
	e.do(req{method: "GET", path: m}).json(&a)
	if fmt.Sprint(a.Defaults) != fmt.Sprint(want) {
		t.Fatalf("read back = %+v", a.Defaults)
	}

	cases := []struct{ path, body, pointer, code string }{
		{b, `{"defaults":{"content_warning":"cw"}}`, "/defaults/content_warning", "unsupported"},
		{b, `{"defaults":{"languages":["en","fr","de","es"]}}`, "/defaults/languages", "too_many"},
		{b, `{"defaults":{"languages":["english"]}}`, "/defaults/languages/0", "invalid_language"},
		{b, `{"defaults":{"options":{"visibility":"unlisted"}}}`, "/defaults/options/visibility", "unknown_option"},
		{m, `{"defaults":{"options":{"visibility":"secret"}}}`, "/defaults/options/visibility", "invalid_option"},
		{m, `{"defaults":{"languages":["en","fr"]}}`, "/defaults/languages", "too_many"},
		{m, `{"defaults":{"content_warning":"` + strings.Repeat("c", 1001) + `"}}`, "/defaults/content_warning", "too_long"},
		{m, `{"defaults":null}`, "/defaults", "invalid"},
		{m, `{"defaults":{"visibility":"unlisted"}}`, "/defaults", "invalid"},
	}
	for _, c := range cases {
		p := wantProblem(t, e.do(req{method: "PATCH", path: c.path, body: c.body, badRequest: true}), 422, codeValidation)
		found := false
		for _, fe := range p.Errors {
			found = found || (fe.Pointer == c.pointer && fe.Code == c.code)
		}
		if !found {
			t.Errorf("%s %s: %+v, want %s at %s", c.path, c.body, p.Errors, c.code, c.pointer)
		}
	}
	// A refused change keeps what was saved; {} clears.
	e.do(req{method: "GET", path: m}).json(&a)
	if a.Defaults.ContentWarning != "spoilers" {
		t.Errorf("a refused change touched the defaults: %+v", a.Defaults)
	}
	// Bluesky's own options.
	wantStatus(t, e.do(req{method: "PATCH", path: b, body: `{"defaults":{"languages":["en","fr"],"options":{"reply_gate":"following"}}}`}), 200)
	wantStatus(t, e.do(req{method: "PATCH", path: m, body: `{"defaults":{}}`}), 200)
	var cleared v1AccountBody
	e.do(req{method: "GET", path: m}).json(&cleared)
	if !cleared.Defaults.empty() {
		t.Errorf("{} left %+v", cleared.Defaults)
	}
}

// A post takes an account's defaults for what it leaves out: a target's
// own value first, then the shared content, then the default; "" or []
// clears one. The preview lists what it applied, and so do the create
// answer's warnings.
func TestV1AccountDefaultsApplied(t *testing.T) {
	e := newV1Env(t, true)
	e.withEngine()
	m := e.acct("mastodon")
	wantStatus(t, e.do(req{method: "PATCH", path: "/v1/accounts/" + m,
		body: `{"defaults":{"content_warning":"spoilers","languages":["fr"],"options":{"visibility":"unlisted"}}}`}), 200)
	preview := func(content, target string) v1PreviewTarget {
		t.Helper()
		body := `{"content":{"text":"hi"` + content + `},"targets":[{"account":"` + m + `"` + target + `}]}`
		r := e.do(req{method: "POST", path: "/v1/posts:preview", body: body})
		wantStatus(t, r, 200)
		var p v1PreviewBody
		r.json(&p)
		return p.Targets[0]
	}

	all := preview("", "")
	if fmt.Sprint(all.DefaultsApplied) != fmt.Sprint(v1AccountDefaults{ContentWarning: "spoilers", Languages: []string{"fr"},
		Options: map[string]any{"visibility": "unlisted"}}) {
		t.Errorf("applied = %+v", all.DefaultsApplied)
	}
	for _, w := range []string{"default applied: content_warning=spoilers", "default applied: languages=fr", "default applied: visibility=unlisted"} {
		if !slices.Contains(all.Warnings, w) {
			t.Errorf("warnings %q lack %q", all.Warnings, w)
		}
	}

	cases := []struct {
		name, content, target string
		want                  v1AccountDefaults
	}{
		{"shared content first", `,"content_warning":"mine","languages":["de"]`, "",
			v1AccountDefaults{Options: map[string]any{"visibility": "unlisted"}}},
		{"shared blank clears", `,"content_warning":"","languages":[]`, "",
			v1AccountDefaults{Options: map[string]any{"visibility": "unlisted"}}},
		{"target first", "", `,"content":{"content_warning":"own","languages":["en"]},"options":{"visibility":"public"}`,
			v1AccountDefaults{}},
		{"target blank clears", "", `,"options":{"visibility":""}`,
			v1AccountDefaults{ContentWarning: "spoilers", Languages: []string{"fr"}}},
		{"target null inherits", "", `,"options":{"visibility":null}`,
			v1AccountDefaults{ContentWarning: "spoilers", Languages: []string{"fr"}, Options: map[string]any{"visibility": "unlisted"}}},
	}
	for _, c := range cases {
		if got := preview(c.content, c.target); fmt.Sprint(got.DefaultsApplied) != fmt.Sprint(c.want) {
			t.Errorf("%s: applied %+v, want %+v", c.name, got.DefaultsApplied, c.want)
		}
	}

	// The post saved takes them, and its answer says so.
	r := e.do(req{method: "POST", path: "/v1/posts", body: `{"content":{"text":"hi"},"targets":[{"account":"` + m + `"}]}`})
	wantStatus(t, r, 202)
	var p v1PostBody
	r.json(&p)
	if len(p.Targets) != 1 || !slices.Contains(p.Targets[0].Warnings, "default applied: visibility=unlisted") {
		t.Fatalf("targets = %+v", p.Targets)
	}
	saved, err := e.st.GetPost(strings.TrimPrefix(p.ID, "pst_"))
	if err != nil {
		t.Fatal(err)
	}
	if f := saved.Targets[0].FieldsJSON; !strings.Contains(f, "unlisted") || !strings.Contains(f, "spoilers") {
		t.Errorf("saved fields = %s", f)
	}
	// A read of the post doesn't repeat them.
	if g := e.do(req{method: "GET", path: "/v1/posts/" + p.ID}); strings.Contains(g.Body.String(), "default applied") {
		t.Error("a read repeats the create answer's warnings")
	}

	// A default the account can no longer take is left out with a
	// warning; the post isn't refused.
	e.api.Caps = func(_ context.Context, _, typ string) destination.Capabilities {
		d, _ := destination.Lookup(typ)
		c := d.Manifest().Defaults
		c.ContentWarning, c.Visibility = false, nil
		return c
	}
	got := preview("", "")
	if got.DefaultsApplied.ContentWarning != "" || got.DefaultsApplied.Options != nil {
		t.Errorf("applied %+v with the account unable to take them", got.DefaultsApplied)
	}
	joined := strings.Join(got.Warnings, "\n")
	if !strings.Contains(joined, "default content warning is left out") || !strings.Contains(joined, "default visibility is left out") {
		t.Errorf("warnings = %q", got.Warnings)
	}
}
