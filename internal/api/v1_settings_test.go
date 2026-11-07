package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/netguard"
	pubnostr "github.com/geofox/publisher/internal/nostr"
	"github.com/geofox/publisher/internal/secrets"
	"github.com/geofox/publisher/internal/settings"
)

// settingsEnv is newV1Env with a settings service (a master key, the
// variables given) and a test sender that answers what it is told.
type settingsEnv struct {
	*v1Env
	svc *settings.Service

	mu     sync.Mutex
	status int
	err    error
	tested []string
	clock  time.Time // the test limiter's
}

func newSettingsEnv(t *testing.T, signIn bool, env map[string]string) *settingsEnv {
	t.Helper()
	k, err := secrets.NewKey([]byte("settings-api-test-master-key-32b"))
	if err != nil {
		t.Fatal(err)
	}
	e := &settingsEnv{v1Env: newV1Env(t, signIn), status: 204}
	svc, err := settings.New(context.Background(), settings.Options{Store: e.st, Keys: secrets.Keyring{Current: k}, Env: env,
		URL: func(raw string, rule destination.URLRule) (string, error) {
			return netguard.Policy{}.OwnerURL(raw, netguard.URLOptions{Secure: rule.Secure, Plain: rule.Plain, Path: rule.Path, Query: rule.Query})
		}})
	if err != nil {
		t.Fatal(err)
	}
	e.svc, e.api.Settings = svc, svc
	e.clock = time.Now()
	e.api.limits().tests.Now = func() time.Time {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.clock
	}
	e.api.TestSettings = func(_ context.Context, group string) (int, bool, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.tested = append(e.tested, group)
		cur := svc.Current()
		switch group {
		case "alerts":
			if cur.Secret(settings.AlertsWebhookURL) == "" {
				return 0, false, nil
			}
		case "feed":
			if cur.Secret(settings.FeedWebhookURL) == "" {
				return 0, false, nil
			}
		}
		return e.status, true, e.err
	}
	return e
}

// answer sets what the next test answers, past the test limiter's window.
func (e *settingsEnv) answer(status int, err error) {
	e.mu.Lock()
	e.status, e.err = status, err
	e.clock = e.clock.Add(settingsTestEvery)
	e.mu.Unlock()
}

// v1SettingsRead is a read of the settings, by group then name.
type v1SettingsRead map[string]map[string]struct {
	Value    any    `json:"value"`
	Source   string `json:"source"`
	Env      string `json:"env"`
	Locked   bool   `json:"locked"`
	Generate bool   `json:"generate"`
	Field    struct {
		Label string `json:"label"`
		Type  string `json:"type"`
	} `json:"field"`
}

func (e *settingsEnv) read(path string) (v1SettingsRead, string) {
	e.t.Helper()
	r := e.do(req{method: "GET", path: path})
	wantStatus(e.t, r, 200)
	var out v1SettingsRead
	r.json(&out)
	return out, r.Header().Get("ETag")
}

// settingsConformance calls each settings route once, signed in, for
// TestV1Conformance.
func settingsConformance(t *testing.T) map[string]bool {
	t.Helper()
	e := newSettingsEnv(t, true, map[string]string{"SCHEDULE_GRACE": "3h", "BLOSSOM_URL": "https://blossom.example"})
	seen := map[string]bool{}
	call := func(pattern string, rq req, status int) {
		t.Helper()
		if r := e.do(rq); r.Code != status {
			t.Errorf("%s: status %d, want %d; body %s", pattern, r.Code, status, r.Body.String())
		}
		seen[pattern] = true
	}
	call("GET /v1/settings", req{method: "GET", path: "/v1/settings"}, 200)
	call("PATCH /v1/settings", req{method: "PATCH", path: "/v1/settings", header: map[string]string{"If-Match": "*"},
		body: `{"publishing":{"max_attempts":3},"alerts":{"webhook_url":"https://alerts.example/hook"}}`}, 200)
	call("POST /v1/settings/{id}:copy-from-env", req{method: "POST", path: "/v1/settings/publishing.schedule_grace:copy-from-env"}, 200)
	call("POST /v1/settings/{id}:generate", req{method: "POST", path: "/v1/settings/feed.token:generate"}, 200)
	call("POST /v1/settings/{id}:test", req{method: "POST", path: "/v1/settings/alerts:test"}, 200)
	call("GET /v1/instance/settings", req{method: "GET", path: "/v1/instance/settings"}, 200)
	call("PATCH /v1/instance/settings", req{method: "PATCH", path: "/v1/instance/settings", body: `{"media":{"public":false}}`}, 200)
	call("POST /v1/instance/settings/{id}:copy-from-env", req{method: "POST", path: "/v1/instance/settings/media.blossom_url:copy-from-env"}, 200)
	call("POST /v1/instance/settings/{id}:generate", req{method: "POST", path: "/v1/instance/settings/media.blossom_key:generate",
		header: e.reauth(opInstanceSecrets)}, 200)
	call("POST /v1/instance/settings/{id}:test", req{method: "POST", path: "/v1/instance/settings/media:test"}, 200)
	e.api.System = func() []SystemValue {
		return SystemValues(envMap{"PUBLIC_URL": "https://pub.example"}.get, []SystemVar{{Env: "PUBLIC_URL"}})
	}
	call("GET /v1/instance/system", req{method: "GET", path: "/v1/instance/system"}, 200)
	return seen
}

type envMap map[string]string

func (m envMap) get(k string) string { return m[k] }

// System lists the variables the server started with: a secret only as
// set, an address without its user and password.
func TestV1System(t *testing.T) {
	e := newSettingsEnv(t, true, nil)
	env := envMap{"PUBLIC_URL": " https://pub.example ", "OIDC_CLIENT_SECRET": "hunter2", "PLC_DIRECTORY_URL": "https://u:p@plc.example/x",
		"ALLOWED_HOSTS": "a.example,b.example"}
	e.api.System = func() []SystemValue {
		return SystemValues(env.get, []SystemVar{{Env: "PUBLIC_URL"}, {Env: "LOG_LEVEL"}, {Env: "PLC_DIRECTORY_URL"},
			{Env: "ALLOWED_HOSTS"}, {Env: "OIDC_CLIENT_SECRET", Secret: true}, {Env: "PUBLISHER_MASTER_KEY", Secret: true}})
	}
	r := e.do(req{method: "GET", path: "/v1/instance/system"})
	wantStatus(t, r, 200)
	if strings.Contains(r.Body.String(), "hunter2") || strings.Contains(r.Body.String(), "u:p@") {
		t.Fatalf("a secret shown: %s", r.Body.String())
	}
	var got v1SystemBody
	r.json(&got)
	want := []SystemValue{{Env: "PUBLIC_URL", Set: true, Value: "https://pub.example"}, {Env: "LOG_LEVEL"},
		{Env: "PLC_DIRECTORY_URL", Set: true, Value: "https://plc.example/x"}, {Env: "ALLOWED_HOSTS", Set: true, Value: "a.example,b.example"},
		{Env: "OIDC_CLIENT_SECRET", Set: true, Secret: true}, {Env: "PUBLISHER_MASTER_KEY", Secret: true}}
	if got.Version == "" || got.Auth == "" || !reflect.DeepEqual(got.Values, want) {
		t.Fatalf("system = %+v", got)
	}
	// Behind the reverse proxy alone, the server's own aren't shown.
	p := newSettingsEnv(t, false, nil)
	wantProblem(t, p.do(req{method: "GET", path: "/v1/instance/system", auth: "none"}), 403, codeSignInRequired)
}

// A read is the settings by group, each with its value, where it comes
// from and its field; a write merges, and null puts a value back.
func TestV1SettingsReadAndWrite(t *testing.T) {
	e := newSettingsEnv(t, true, map[string]string{"AUTO_RETRY_MAX_ATTEMPTS": "4"})
	got, etag := e.read("/v1/settings")
	if etag == "" || !strings.HasPrefix(etag, `"`) {
		t.Fatalf("ETag = %q", etag)
	}
	if _, ok := got["media"]; ok {
		t.Error("the owner's settings list an instance setting")
	}
	grace := got["publishing"]["schedule_grace"]
	if grace.Value != "2h" || grace.Source != "default" || grace.Env != "SCHEDULE_GRACE" || grace.Locked || grace.Field.Type != "duration" {
		t.Errorf("schedule_grace = %+v", grace)
	}
	if ma := got["publishing"]["max_attempts"]; ma.Value != float64(4) || ma.Source != "env" || !ma.Locked {
		t.Errorf("max_attempts = %+v", ma)
	}

	// A merge patch: what isn't named stays.
	r := e.do(req{method: "PATCH", path: "/v1/settings", header: map[string]string{"If-Match": etag},
		body: `{"publishing":{"schedule_grace":"30m","auto_retry":false},"composing":{"languages":["fr","en"]}}`})
	wantStatus(t, r, 200)
	next := r.Header().Get("ETag")
	if next == etag {
		t.Error("the ETag didn't change with the settings")
	}
	cur := e.svc.Current()
	if cur.Duration(settings.PublishingGrace).String() != "30m0s" || cur.Bool(settings.PublishingAutoRetry) ||
		!slices.Equal(cur.Strings(settings.ComposingLanguages), []string{"fr", "en"}) {
		t.Errorf("after the write: grace %v retry %v languages %v", cur.Duration(settings.PublishingGrace),
			cur.Bool(settings.PublishingAutoRetry), cur.Strings(settings.ComposingLanguages))
	}

	// null puts a value back to its default.
	wantStatus(t, e.do(req{method: "PATCH", path: "/v1/settings", body: `{"publishing":{"auto_retry":null}}`}), 200)
	if v, _ := e.svc.Current().Get(settings.PublishingAutoRetry); v.Source != settings.SourceDefault || v.Value != true {
		t.Errorf("auto_retry after null = %+v", v)
	}
	if e.svc.Current().Duration(settings.PublishingGrace).String() != "30m0s" {
		t.Error("a write changed a setting it didn't name")
	}

	// An empty patch changes nothing and answers the settings.
	wantStatus(t, e.do(req{method: "PATCH", path: "/v1/settings", body: `{}`}), 200)

	// A stale If-Match: 412 and the current ETag; * and none write.
	p := wantProblem(t, e.do(req{method: "PATCH", path: "/v1/settings", header: map[string]string{"If-Match": etag},
		body: `{"publishing":{"schedule_grace":"1h"}}`}), 412, codePreconditionFailed)
	_ = p
	r = e.do(req{method: "PATCH", path: "/v1/settings", header: map[string]string{"If-Match": `W/` + etag},
		body: `{"publishing":{"schedule_grace":"1h"}}`, badRequest: true})
	wantProblem(t, r, 412, codePreconditionFailed)
	if r.Header().Get("ETag") != e.svc.Current().Version(settings.Owner) {
		t.Errorf("412 ETag = %q, want the current %q", r.Header().Get("ETag"), e.svc.Current().Version(settings.Owner))
	}
	wantStatus(t, e.do(req{method: "PATCH", path: "/v1/settings", header: map[string]string{"If-Match": "*"},
		body: `{"publishing":{"schedule_grace":"1h"}}`}), 200)
}

// Each refused write says which setting and why, as a JSON Pointer into
// the nested body; nothing of a refused write is saved.
func TestV1SettingsRefusals(t *testing.T) {
	e := newSettingsEnv(t, true, map[string]string{"AUTO_RETRY_MAX_ATTEMPTS": "4"})
	before := e.svc.Current().Version(settings.Owner)

	p := wantProblem(t, e.do(req{method: "PATCH", path: "/v1/settings",
		body: `{"publishing":{"max_attempts":3,"schedule_grace":"1h"}}`}), 409, codeLockedByEnv)
	if len(p.Errors) != 1 || p.Errors[0].Pointer != "/publishing/max_attempts" || !strings.Contains(p.Detail, "AUTO_RETRY_MAX_ATTEMPTS") {
		t.Errorf("locked = %+v", p)
	}

	cases := []struct{ body, pointer string }{
		{`{"publishing":{"schedule_grace":"10s"}}`, "/publishing/schedule_grace"},
		{`{"composing":{"languages":["english"]}}`, "/composing/languages"},
		{`{"publishing":{"nope":1}}`, "/publishing/nope"},
		{`{"media":{"store":"local"}}`, "/media/store"},
		{`{"publishing":null}`, "/publishing"},
		{`{"publishing":[1]}`, "/publishing"},
		{`{"alerts":{"webhook_url":"http://alerts.example/hook"}}`, "/alerts/webhook_url"},
	}
	for _, c := range cases {
		p := wantProblem(t, e.do(req{method: "PATCH", path: "/v1/settings", body: c.body, badRequest: true}), 422, codeValidation)
		found := false
		for _, fe := range p.Errors {
			found = found || fe.Pointer == c.pointer
		}
		if !found {
			t.Errorf("%s: errors %+v, want one at %s", c.body, p.Errors, c.pointer)
		}
	}
	if e.svc.Current().Version(settings.Owner) != before {
		t.Error("a refused write changed the settings")
	}

	// copy-from-env needs a value from the environment.
	wantProblem(t, e.do(req{method: "POST", path: "/v1/settings/publishing.schedule_grace:copy-from-env"}), 409, codeInvalidState)
	wantProblem(t, e.do(req{method: "POST", path: "/v1/settings/media.store:copy-from-env"}), 404, codeNotFound)
	// Publisher makes only some values.
	wantProblem(t, e.do(req{method: "POST", path: "/v1/settings/publishing.schedule_grace:generate"}), 404, codeNotFound)
	wantProblem(t, e.do(req{method: "POST", path: "/v1/settings/publishing.max_attempts:frob", undocumented: true}), 404, codeNotFound)
}

// A secret setting is never answered: a read says it is set (and an
// address's origin), and nothing of it is in any answer.
func TestV1SettingsSecretsNeverAnswered(t *testing.T) {
	e := newSettingsEnv(t, true, nil)
	const hook = "https://hooks.example/services/T0/B0/very-secret-path"
	r := e.do(req{method: "PATCH", path: "/v1/settings",
		body: `{"feed":{"webhook_url":"` + hook + `","webhook_token":"tok-secret-123"},"translate":{"deepl_key":"deepl-secret:fx"}}`})
	wantStatus(t, r, 200)
	for _, s := range []string{"very-secret-path", "tok-secret-123", "deepl-secret"} {
		if strings.Contains(r.Body.String(), s) {
			t.Errorf("the answer carries %q: %s", s, r.Body.String())
		}
	}
	got, _ := e.read("/v1/settings")
	u, _ := got["feed"]["webhook_url"].Value.(map[string]any)
	if u["set"] != true || u["origin"] != "https://hooks.example" {
		t.Errorf("webhook_url = %+v", got["feed"]["webhook_url"])
	}
	if e.svc.Current().Secret(settings.FeedWebhookURL) != hook {
		t.Error("the secret wasn't saved")
	}
	// null clears a secret.
	wantStatus(t, e.do(req{method: "PATCH", path: "/v1/settings", body: `{"translate":{"deepl_key":null}}`}), 200)
	if e.svc.Current().Secret(settings.TranslateDeepLKey) != "" {
		t.Error("null didn't clear the secret")
	}
}

// Generate makes a new feed token, saves it and shows it this once.
func TestV1SettingsGenerate(t *testing.T) {
	e := newSettingsEnv(t, true, nil)
	r := e.do(req{method: "POST", path: "/v1/settings/feed.token:generate"})
	wantStatus(t, r, 200)
	var g v1GeneratedSetting
	r.json(&g)
	if len(g.Value) < 40 || e.svc.Current().Secret(settings.FeedToken) != g.Value {
		t.Fatalf("generated %q, saved %q", g.Value, e.svc.Current().Secret(settings.FeedToken))
	}
	got, _ := e.read("/v1/settings")
	if strings.Contains(fmt.Sprint(got), g.Value) {
		t.Error("a read shows the generated token")
	}
	if !got["feed"]["token"].Generate || got["feed"]["webhook_token"].Generate || got["translate"]["deepl_key"].Generate {
		t.Errorf("generate = %v %v %v", got["feed"]["token"].Generate, got["feed"]["webhook_token"].Generate, got["translate"]["deepl_key"].Generate)
	}
	// Each call makes a new one.
	var g2 v1GeneratedSetting
	e.do(req{method: "POST", path: "/v1/settings/feed.token:generate"}).json(&g2)
	if g2.Value == g.Value {
		t.Error("generate made the same token twice")
	}

	// The Blossom key: its public key is answered, never the key, and a
	// read shows the public key.
	r = e.do(req{method: "POST", path: "/v1/instance/settings/media.blossom_key:generate", header: e.reauth(opInstanceSecrets)})
	wantStatus(t, r, 200)
	r.json(&g)
	sk := e.svc.Current().Secret(settings.MediaBlossomKey)
	if !strings.HasPrefix(g.Value, "npub1") || sk == "" || pubnostr.Npub(sk) != g.Value || strings.Contains(r.Body.String(), sk) {
		t.Fatalf("generated %q for key %q", g.Value, sk)
	}
	inst, _ := e.read("/v1/instance/settings")
	if v, _ := inst["media"]["blossom_key"].Value.(map[string]any); v["public_key"] != g.Value || v["set"] != true {
		t.Errorf("blossom_key read = %+v", inst["media"]["blossom_key"])
	}
}

// Behind the reverse proxy alone, the owner's plain settings can be
// changed, but no secret one, and nothing of the server's.
func TestV1SettingsProxyRefusals(t *testing.T) {
	e := newSettingsEnv(t, false, map[string]string{"FEED_WEBHOOK_TOKEN": "env-token"})
	none := "none"
	wantStatus(t, e.do(req{method: "GET", path: "/v1/settings", auth: none}), 200)
	wantStatus(t, e.do(req{method: "PATCH", path: "/v1/settings", auth: none, body: `{"publishing":{"max_attempts":2}}`}), 200)
	refused := []req{
		{method: "PATCH", path: "/v1/settings", body: `{"translate":{"deepl_key":"k"}}`},
		{method: "PATCH", path: "/v1/settings", body: `{"publishing":{"max_attempts":3},"feed":{"token":null}}`},
		{method: "POST", path: "/v1/settings/feed.token:generate"},
		{method: "POST", path: "/v1/settings/feed.webhook_token:copy-from-env"},
		{method: "GET", path: "/v1/instance/settings"},
		{method: "PATCH", path: "/v1/instance/settings", body: `{"media":{"public":true}}`},
		{method: "POST", path: "/v1/instance/settings/media.blossom_url:copy-from-env"},
		{method: "POST", path: "/v1/instance/settings/media:test"},
	}
	for _, rq := range refused {
		rq.auth = none
		r := e.do(rq)
		if r.Code != 403 || r.problem().Code != codeSignInRequired {
			t.Errorf("%s %s %s: %d %s, want 403 sign_in_required", rq.method, rq.path, rq.body, r.Code, r.Body.String())
		}
	}
	if e.svc.Current().Int(settings.PublishingMaxAttempts) != 2 {
		t.Error("a refused write changed a plain setting with it")
	}
}

// The server's secret settings need the owner to confirm it's them; a
// token can't write them, whatever its scopes.
func TestV1InstanceSecretsNeedReauth(t *testing.T) {
	e := newSettingsEnv(t, true, nil)
	key := "1111111111111111111111111111111111111111111111111111111111111111"
	p := wantProblem(t, e.do(req{method: "PATCH", path: "/v1/instance/settings", body: `{"media":{"blossom_key":"` + key + `"}}`}),
		403, codeReauthRequired)
	if p.Operation != opInstanceSecrets {
		t.Errorf("operation = %q", p.Operation)
	}
	wantStatus(t, e.do(req{method: "PATCH", path: "/v1/instance/settings", header: e.reauth(opInstanceSecrets),
		body: `{"media":{"blossom_key":"` + key + `"}}`}), 200)
	if e.svc.Current().Secret(settings.MediaBlossomKey) != key {
		t.Error("the key wasn't saved")
	}
	// A plain instance setting needs no re-authentication.
	wantStatus(t, e.do(req{method: "PATCH", path: "/v1/instance/settings", body: `{"media":{"local_quota":100}}`}), 200)

	tok := e.token(scopeInstanceAdmin)
	wantStatus(t, e.do(req{method: "GET", path: "/v1/instance/settings", auth: tok}), 200)
	wantProblem(t, e.do(req{method: "PATCH", path: "/v1/instance/settings", auth: tok, body: `{"media":{"blossom_key":null}}`}),
		403, codeSessionRequired)
	wantStatus(t, e.do(req{method: "PATCH", path: "/v1/instance/settings", auth: tok, body: `{"media":{"public":false}}`}), 200)
}

// Each route takes its own scope; admin doesn't include the settings.
func TestV1SettingsScopes(t *testing.T) {
	e := newSettingsEnv(t, true, nil)
	read, write, inst, admin := e.token(scopeSettingsRead), e.token(scopeSettingsWrite), e.token(scopeInstanceAdmin), e.token(scopeAdmin)
	patch := `{"publishing":{"max_attempts":5}}`
	wantStatus(t, e.do(req{method: "GET", path: "/v1/settings", auth: read}), 200)
	wantProblem(t, e.do(req{method: "PATCH", path: "/v1/settings", auth: read, body: patch}), 403, codeInsufficientScope)
	wantStatus(t, e.do(req{method: "PATCH", path: "/v1/settings", auth: write, body: patch}), 200)
	for _, tok := range []string{admin, inst} {
		wantProblem(t, e.do(req{method: "GET", path: "/v1/settings", auth: tok}), 403, codeInsufficientScope)
	}
	for _, tok := range []string{admin, read, write} {
		wantProblem(t, e.do(req{method: "GET", path: "/v1/instance/settings", auth: tok}), 403, codeInsufficientScope)
	}
	// A token can write a secret owner setting (the owner gave it
	// settings:write, which only a session can grant).
	wantStatus(t, e.do(req{method: "PATCH", path: "/v1/settings", auth: write, body: `{"translate":{"deepl_key":"k:fx"}}`}), 200)
}

// A test says whether the server answered with success, else only a class
// of failure; at most one per group every 10 seconds.
func TestV1SettingsTest(t *testing.T) {
	e := newSettingsEnv(t, true, nil)
	// Nothing set yet: nothing to test.
	wantProblem(t, e.do(req{method: "POST", path: "/v1/settings/alerts:test"}), 409, codeInvalidState)
	wantStatus(t, e.do(req{method: "PATCH", path: "/v1/settings",
		body: `{"alerts":{"webhook_url":"https://alerts.example/hook"},"feed":{"webhook_url":"https://feed.example/hook"}}`}), 200)
	e.answer(204, nil)

	test := func(path string) v1SettingsTest {
		t.Helper()
		r := e.do(req{method: "POST", path: path})
		wantStatus(t, r, 200)
		var out v1SettingsTest
		r.json(&out)
		return out
	}
	if got := test("/v1/settings/alerts:test"); !got.OK || got.Error != "" {
		t.Errorf("success = %+v", got)
	}
	// Once every 10 seconds, per group.
	p := wantProblem(t, e.do(req{method: "POST", path: "/v1/settings/alerts:test"}), 429, codeRateLimited)
	_ = p
	if got := test("/v1/settings/feed:test"); !got.OK {
		t.Errorf("feed = %+v", got)
	}

	blocked := &net.OpError{Op: "dial", Net: "tcp", Err: destination.ErrBlockedAddress}
	cases := []struct {
		status int
		err    error
		class  string
	}{
		{500, nil, "http_error"},
		{302, nil, "http_error"},
		{0, fmt.Errorf("post: %w", blocked), "blocked_destination"},
		{0, fmt.Errorf("post: %w", destination.ErrRedirectRefused), "http_error"},
		{0, context.DeadlineExceeded, "timeout"},
		{0, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, "refused"},
		{0, &net.DNSError{Err: "no such host", Name: "alerts.example", IsNotFound: true}, "unreachable"},
		{0, errors.New("anything else"), "unreachable"},
	}
	for _, c := range cases {
		e.answer(c.status, c.err)
		got := test("/v1/settings/alerts:test")
		if got.OK || got.Error != c.class {
			t.Errorf("%d %v: %+v, want %s", c.status, c.err, got, c.class)
		}
	}

	// Each scope tests its own groups.
	wantProblem(t, e.do(req{method: "POST", path: "/v1/settings/media:test"}), 404, codeNotFound)
	wantProblem(t, e.do(req{method: "POST", path: "/v1/instance/settings/alerts:test"}), 404, codeNotFound)
	wantProblem(t, e.do(req{method: "POST", path: "/v1/settings/composing:test"}), 404, codeNotFound)

	// Without a tester (no server to reach), there is no test.
	e.api.TestSettings = nil
	wantProblem(t, e.do(req{method: "POST", path: "/v1/settings/feed:test"}), 404, codeNotFound)
}

// Without a settings service, the routes say so.
func TestV1SettingsUnavailable(t *testing.T) {
	e := newV1Env(t, true)
	wantProblem(t, e.do(req{method: "GET", path: "/v1/settings"}), 503, codeUnavailable)
	wantProblem(t, e.do(req{method: "PATCH", path: "/v1/settings", body: `{"publishing":{"max_attempts":2}}`}), 503, codeUnavailable)
}
