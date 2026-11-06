package api

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/accounts"
	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/store"
)

// addAccount adds an env account of typ (key key) created after the
// seeded ones.
func (e *v1Env) addAccount(id, typ, key string) {
	e.t.Helper()
	if err := e.st.InsertAccount(context.Background(), store.Account{ID: id, Type: typ, Label: typ + " " + key,
		Source: "env", EnvKey: key, Enabled: true, CreatedAt: time.Now().Add(time.Hour)}); err != nil {
		e.t.Fatal(err)
	}
}

var etagRe = regexp.MustCompile(`^"[0-9]+"$`)

// An account's view says whether it can be used, why not, where it goes in
// the app's order, and the owner's flags; the single view has an ETag.
func TestV1AccountStatusOrderAndFlags(t *testing.T) {
	e := newV1Env(t, true)
	e.addAccount("acc_work", "mastodon", "work")
	e.withAccounts()
	bsky, masto := e.acct("bluesky"), e.acct("mastodon")
	e.disable(masto)

	var list struct{ Data []v1AccountBody }
	e.do(req{method: "GET", path: "/v1/accounts"}).json(&list)
	byID := map[string]v1AccountBody{}
	for _, a := range list.Data {
		byID[a.ID] = a
	}
	if b := byID[bsky]; b.Status != "connected" || b.StatusReason != "" || !b.DefaultSelected || !b.PublicFeed {
		t.Errorf("bluesky = %+v", b)
	}
	if m := byID[masto]; m.Status != "disabled" || m.StatusReason != "turned off in the app" || m.Enabled {
		t.Errorf("turned-off mastodon = %+v", m)
	}
	// Registry order: networks in their fixed order, then oldest first.
	want := map[string]int{}
	for _, en := range e.reg.List(store.DefaultOwner) {
		want[en.Row.ID] = en.Order
	}
	for id, a := range byID {
		if a.Order != want[id] {
			t.Errorf("%s order = %d, want %d", id, a.Order, want[id])
		}
	}
	if byID[masto].Order >= byID["acc_work"].Order {
		t.Errorf("the older Mastodon account comes after the newer: %d, %d", byID[masto].Order, byID["acc_work"].Order)
	}

	r := e.do(req{method: "GET", path: "/v1/accounts/" + bsky})
	wantStatus(t, r, 200)
	if et := r.Header().Get("ETag"); !etagRe.MatchString(et) {
		t.Fatalf("ETag = %q", et)
	}
}

// PATCH changes the owner's settings, refuses a stale or malformed
// If-Match and bad values, and the registry follows.
func TestV1PatchAccount(t *testing.T) {
	e := newV1Env(t, true)
	e.withEngine()
	e.withAccounts()
	masto := e.acct("mastodon")
	path := "/v1/accounts/" + masto
	r := e.do(req{method: "GET", path: path})
	first := r.Header().Get("ETag")

	r = e.do(req{method: "PATCH", path: path, header: map[string]string{"If-Match": first},
		body: `{"label":"  Work  ","public_feed":false}`})
	wantStatus(t, r, 200)
	var got v1AccountBody
	r.json(&got)
	second := r.Header().Get("ETag")
	if got.Label != "Work" || got.PublicFeed || !got.Enabled || !got.DefaultSelected || second == first || !etagRe.MatchString(second) {
		t.Fatalf("patched = %+v (ETag %s → %s)", got, first, second)
	}
	if re := e.do(req{method: "GET", path: path}); re.Header().Get("ETag") != second {
		t.Fatalf("read back ETag %s, want the PATCH's %s", re.Header().Get("ETag"), second)
	}
	if en, _ := e.reg.Get(masto); en.Row.Label != "Work" || en.Row.PublicFeed {
		t.Fatalf("registry row = %+v", en.Row)
	}

	// A change made on the first read is refused, saying what is current.
	r = e.do(req{method: "PATCH", path: path, header: map[string]string{"If-Match": first}, body: `{"label":"Old"}`})
	wantProblem(t, r, http.StatusPreconditionFailed, codePreconditionFailed)
	if r.Header().Get("ETag") != second {
		t.Fatalf("412 ETag = %q, want %q", r.Header().Get("ETag"), second)
	}
	for _, bad := range []string{`W/` + second, `"x"`, second + `, "1"`} {
		wantProblem(t, e.do(req{method: "PATCH", path: path, header: map[string]string{"If-Match": bad}, body: `{}`}),
			400, codeInvalidParameter)
	}

	// Turned off: the registry says so and a new post can't pick it.
	r = e.do(req{method: "PATCH", path: path, header: map[string]string{"If-Match": "*"}, body: `{"enabled":false}`})
	wantStatus(t, r, 200)
	r.json(&got)
	if got.Status != "disabled" || got.Enabled {
		t.Fatalf("turned off = %+v", got)
	}
	if en, _ := e.reg.Get(masto); en.Status != accounts.Disabled {
		t.Fatalf("registry status = %s", en.Status)
	}
	p := wantProblem(t, e.do(req{method: "POST", path: "/v1/posts", badRequest: true,
		body: `{"content":{"text":"x"},"targets":[{"account":"` + masto + `"}]}`}), 422, codeValidation)
	if p.Errors[0].Code != "account_disabled" {
		t.Fatalf("errors = %+v", p.Errors)
	}
	wantStatus(t, e.do(req{method: "PATCH", path: path, body: `{"enabled":true}`}), 200)
	if en, _ := e.reg.Get(masto); en.Status != accounts.Connected {
		t.Fatalf("turned on again: registry status = %s", en.Status)
	}

	// Nothing to change: the same account and ETag.
	before := e.do(req{method: "GET", path: path}).Header().Get("ETag")
	if r := e.do(req{method: "PATCH", path: path, body: `{"enabled":true}`}); r.Header().Get("ETag") != before {
		t.Fatalf("a no-op PATCH moved the ETag")
	}

	for _, c := range []struct{ body, ptr, code string }{
		{`{"label":"   "}`, "/label", "required"},
		{`{"label":"` + strings.Repeat("é", 65) + `"}`, "/label", "too_long"},
		{`{"label":"a\u0007b"}`, "/label", "invalid"},
		{`{"label":null}`, "/label", "invalid"},
		{`{"label":7}`, "/label", "invalid"},
		{`{"enabled":null}`, "/enabled", "invalid"},
		{`{"default_selected":"yes"}`, "/default_selected", "invalid"},
		{`{"public_feed":1}`, "/public_feed", "invalid"},
	} {
		p := wantProblem(t, e.do(req{method: "PATCH", path: path, body: c.body, badRequest: true}), 422, codeValidation)
		if len(p.Errors) != 1 || p.Errors[0].Pointer != c.ptr || p.Errors[0].Code != c.code {
			t.Errorf("%s: errors = %+v", c.body, p.Errors)
		}
	}
	// 64 characters is fine, counted as characters, not bytes.
	wantStatus(t, e.do(req{method: "PATCH", path: path, body: `{"label":"` + strings.Repeat("é", 64) + `"}`}), 200)
	wantProblem(t, e.do(req{method: "PATCH", path: path, body: `{"type":"bluesky"}`, badRequest: true}), 400, codeInvalidJSON)
	wantProblem(t, e.do(req{method: "PATCH", path: "/v1/accounts/acc_nope", body: `{"label":"x"}`}), 404, codeNotFound)
	if en, _ := e.st.Account(context.Background(), store.DefaultOwner, masto); en.Type != "mastodon" {
		t.Fatalf("account = %+v", en)
	}
}

// accounts:write is granted by name: admin doesn't include it, a token
// can't hand it out, the signed-in owner can.
func TestV1AccountsWriteScope(t *testing.T) {
	e := newV1Env(t, true)
	e.withAccounts()
	path := "/v1/accounts/" + e.acct("bluesky")
	admin := e.token(scopeAdmin)
	wantProblem(t, e.do(req{method: "PATCH", path: path, auth: admin, body: `{"label":"x"}`}), 403, codeInsufficientScope)
	wantProblem(t, e.do(req{method: "PATCH", path: path, auth: e.token(scopeAccountsRead), body: `{"label":"x"}`}), 403, codeInsufficientScope)
	wantStatus(t, e.do(req{method: "GET", path: path, auth: admin}), 200)

	p := wantProblem(t, e.do(req{method: "POST", path: "/v1/tokens", auth: admin, badRequest: true,
		body: `{"name":"x","scopes":["accounts:write"]}`}), 422, codeValidation)
	if len(p.Errors) != 1 || p.Errors[0].Pointer != "/scopes/0" || p.Errors[0].Code != "exceeds_caller" {
		t.Fatalf("errors = %+v", p.Errors)
	}
	r := e.do(req{method: "POST", path: "/v1/tokens", body: `{"name":"settings","scopes":["accounts:write"]}`})
	wantStatus(t, r, 201)
	var tok v1CreatedTokenBody
	r.json(&tok)
	wantStatus(t, e.do(req{method: "PATCH", path: path, auth: tok.Secret, body: `{"label":"By token"}`}), 200)

	var me v1MeBody
	e.do(req{method: "GET", path: "/v1/me", auth: admin}).json(&me)
	if strings.Contains(strings.Join(me.Scopes, ","), "accounts:write") {
		t.Fatalf("admin's scopes = %v", me.Scopes)
	}
}

// A change is an account.updated event naming the fields and who made
// it, never the values, streamed only to a caller that may read accounts.
func TestV1AccountUpdatedEvent(t *testing.T) {
	e := newV1Env(t, true)
	e.withAccounts()
	bsky := e.acct("bluesky")
	last, _ := e.st.LastEventID(context.Background())
	after := "/v1/events?after=" + strconv64(last)
	r := e.do(req{method: "POST", path: "/v1/tokens", body: `{"name":"w","scopes":["accounts:write"]}`})
	var tok v1CreatedTokenBody
	r.json(&tok)
	wantStatus(t, e.do(req{method: "PATCH", path: "/v1/accounts/" + bsky, auth: tok.Secret,
		body: `{"label":"Private words","default_selected":false}`}), 200)
	wantStatus(t, e.do(req{method: "PATCH", path: "/v1/accounts/" + bsky, body: `{"public_feed":false}`}), 200)

	stream := func(auth string) []sseEvent {
		t.Helper()
		stop, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		r := e.do(req{method: "GET", path: after, auth: auth, ctx: stop})
		wantStatus(t, r, 200)
		if strings.Contains(r.Body.String(), "Private words") {
			t.Fatalf("an event carries a value: %s", r.Body.String())
		}
		_, evs := parseSSE(t, r.Body.String())
		return evs
	}
	evs := stream("")
	if len(evs) != 2 || evs[0].Event != "account.updated" {
		t.Fatalf("events = %+v", evs)
	}
	a, b := evs[0].Data, evs[1].Data
	if a.Account != bsky || a.Destination != "bluesky" || a.Post != "" || strings.Join(a.Fields, ",") != "label,default_selected" ||
		a.Actor != "token" || a.Token != tok.ID {
		t.Errorf("token's change = %+v", a)
	}
	if strings.Join(b.Fields, ",") != "public_feed" || b.Actor != "session" || b.Token != "" {
		t.Errorf("session's change = %+v", b)
	}
	if evs := stream(e.token(scopePostsRead)); len(evs) != 0 {
		t.Errorf("a posts:read token saw %+v", evs)
	}
	if evs := stream(e.token(scopePostsRead, scopeAccountsRead)); len(evs) != 2 {
		t.Errorf("a posts:read + accounts:read token saw %d events, want 2", len(evs))
	}
}

// Two accounts of one network are two targets, each counted by its own
// limits.
func TestV1TwoAccountsOfOneNetwork(t *testing.T) {
	e := newV1Env(t, true)
	e.withEngine()
	e.addAccount("acc_work", "mastodon", "work")
	e.withAccounts()
	home := e.acct("mastodon")
	e.api.Caps = func(_ context.Context, id, typ string) destination.Capabilities {
		d, _ := destination.Lookup(typ)
		c := d.Manifest().Defaults
		if id == "acc_work" {
			c.Text.MaxLength = 100
		}
		return c
	}
	text := strings.Repeat("word ", 50) // 250 characters
	body := `{"content":{"text":"` + text + `"},"targets":[{"account":"` + home + `"},{"account":"acc_work"}]}`
	r := e.do(req{method: "POST", path: "/v1/posts:preview", body: body})
	wantStatus(t, r, 200)
	var pv v1PreviewBody
	r.json(&pv)
	if len(pv.Targets) != 2 || pv.Targets[0].Account != home || pv.Targets[1].Account != "acc_work" {
		t.Fatalf("preview = %+v", pv)
	}
	if pv.Targets[0].Limit != 500 || len(pv.Targets[0].Items) != 1 || pv.Targets[1].Limit != 100 || len(pv.Targets[1].Items) < 3 {
		t.Fatalf("limits %d/%d, items %d/%d", pv.Targets[0].Limit, pv.Targets[1].Limit,
			len(pv.Targets[0].Items), len(pv.Targets[1].Items))
	}
	r = e.do(req{method: "POST", path: "/v1/posts", body: body})
	wantStatus(t, r, 202)
	var pb v1PostBody
	r.json(&pb)
	if len(pb.Targets) != 2 || pb.Targets[0].Account == pb.Targets[1].Account {
		t.Fatalf("targets = %+v", pb.Targets)
	}
	// The same account twice is still refused.
	twice := `{"content":{"text":"x"},"targets":[{"account":"acc_work"},{"account":"acc_work"}]}`
	p := wantProblem(t, e.do(req{method: "POST", path: "/v1/posts", body: twice, badRequest: true}), 422, codeValidation)
	if len(p.Errors) != 1 || p.Errors[0].Pointer != "/targets/1/account" || p.Errors[0].Code != "duplicate" {
		t.Fatalf("errors = %+v", p.Errors)
	}
}

// Each status that can't send is refused by name; a draft keeps the
// account (a draft isn't sent).
func TestV1EachUnavailableStatus(t *testing.T) {
	e := newV1Env(t, true)
	e.withEngine()
	bsky := e.acct("bluesky")
	for st, words := range map[accounts.Status]string{
		accounts.Disabled:       "is turned off",
		accounts.NotConfigured:  "is not configured",
		accounts.Locked:         "is locked",
		accounts.NeedsReconnect: "needs to be connected again",
	} {
		e.api.Accounts = statusOf{bsky: st}
		body := `{"content":{"text":"x"},"targets":[{"account":"` + bsky + `"}]}`
		p := wantProblem(t, e.do(req{method: "POST", path: "/v1/posts", body: body, badRequest: true}), 422, codeValidation)
		if len(p.Errors) != 1 || p.Errors[0].Code != "account_disabled" || !strings.Contains(p.Errors[0].Detail, words) {
			t.Errorf("%s: errors = %+v", st, p.Errors)
		}
		wantStatus(t, e.do(req{method: "POST", path: "/v1/drafts", body: `{"post":` + body + `}`}), 201)
	}
}

// One account acts on the post natively; a second account of the post's
// network is refused (the post exists once there).
func TestV1InteractionOneNativeActor(t *testing.T) {
	e := newV1Env(t, true)
	e.withEngine()
	e.addAccount("acc_work", "mastodon", "work")
	e.withAccounts()
	home := e.acct("mastodon")
	for _, action := range []string{"reply_to", "quote"} {
		content := `"content":{"text":"hi"},`
		body := `{` + content + `"targets":[{"account":"` + home + `"},{"account":"acc_work"}],"` + action + `":{"url":"` + mastoSource + `"}}`
		p := wantProblem(t, e.do(req{method: "POST", path: "/v1/posts", body: body, badRequest: true}), 422, codeValidation)
		if len(p.Errors) != 1 || p.Errors[0].Pointer != "/targets/1/account" || p.Errors[0].Code != "one_native_actor" ||
			!strings.Contains(p.Errors[0].Detail, "only one of your Mastodon accounts") {
			t.Errorf("%s: errors = %+v", action, p.Errors)
		}
	}
	if last := e.lookups[len(e.lookups)-1]; last != home {
		t.Errorf("looked up with %s, want the first target %s", last, home)
	}
}

// POST /v1/resolve with account: that account looks the post up, if it is
// the caller's, on the post's network, and connected.
func TestV1ResolveNamedAccount(t *testing.T) {
	e := newV1Env(t, true)
	e.withEngine()
	e.addAccount("acc_work", "mastodon", "work")
	reg := e.withAccounts()
	e.api.Accounts = withForeign{reg, t}
	resolve := func(account string) v1Resp {
		return e.do(req{method: "POST", path: "/v1/resolve", badRequest: true,
			body: `{"url":"` + mastoSource + `","account":"` + account + `"}`})
	}
	r := resolve("acc_work")
	wantStatus(t, r, 200)
	var got v1ResolveBody
	r.json(&got)
	if got.Account == nil || *got.Account != "acc_work" || e.lookups[len(e.lookups)-1] != "acc_work" {
		t.Fatalf("resolved with %v (lookups %v)", got.Account, e.lookups)
	}
	e.disable("acc_work")
	e.api.Accounts = withForeign{reg, t}
	for _, c := range []struct{ account, code string }{
		{"acc_nope", "unknown_account"},
		{"acc_theirs", "unknown_account"},
		{e.acct("bluesky"), "not_allowed"},
		{"acc_work", "account_disabled"},
	} {
		p := wantProblem(t, resolve(c.account), 422, codeValidation)
		if len(p.Errors) != 1 || p.Errors[0].Pointer != "/account" || p.Errors[0].Code != c.code {
			t.Errorf("%s: errors = %+v", c.account, p.Errors)
		}
	}
}
