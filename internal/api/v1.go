package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"slices"
	"strings"

	"github.com/geofox/publisher/internal/store"
)

// The /v1 API (docs/api/openapi.yaml). Every route authenticates with the
// browser session or a bearer token, checks the token's scope, answers
// errors as RFC 9457 problem details, and is listed in v1Routes so the
// OpenAPI conformance test can check that each one is documented.

// Token scopes.
const (
	scopePostsRead     = "posts:read"
	scopePostsWrite    = "posts:write"
	scopeMediaWrite    = "media:write"
	scopeDraftsRead    = "drafts:read"
	scopeDraftsWrite   = "drafts:write"
	scopeAccountsRead  = "accounts:read"
	scopeAccountsWrite = "accounts:write"
	scopeSettingsRead  = "settings:read"
	scopeSettingsWrite = "settings:write"
	scopeInstanceAdmin = "instance:admin"
	scopeAdmin         = "admin"
)

// v1Scopes are the scopes a token can hold, in documentation order.
var v1Scopes = []string{scopePostsRead, scopePostsWrite, scopeMediaWrite, scopeDraftsRead, scopeDraftsWrite,
	scopeAccountsRead, scopeAccountsWrite, scopeSettingsRead, scopeSettingsWrite, scopeInstanceAdmin, scopeAdmin}

// adminImplies are the scopes admin includes: those that existed before
// accounts could be changed. A scope added since (accounts:write) must be
// granted by name, so an old admin token gains nothing new.
var adminImplies = []string{scopePostsRead, scopePostsWrite, scopeMediaWrite, scopeDraftsRead, scopeDraftsWrite,
	scopeAccountsRead}

// privilegedScopes are the scopes a new token can only get with the
// owner's fresh re-authentication (token.create_privileged): admin, and
// every scope added since step 4.
var privilegedScopes = []string{scopeAdmin, scopeAccountsWrite, scopeSettingsWrite, scopeInstanceAdmin}

// sessionOnlyScopes are the scopes only a signed-in owner (a session, or
// the reverse proxy when sign-in is off) may grant to a new token: a
// token, admin included, can't hand them out.
var sessionOnlyScopes = []string{scopeAccountsWrite, scopeSettingsWrite, scopeInstanceAdmin}

// principal is who is calling: the owner in a browser session, a token, or
// (in proxy mode) whoever the reverse proxy let through.
type principal struct {
	Kind    string // "session", "token" or "proxy"
	ID      string // the user's or the token's id; "proxy" otherwise
	OwnerID string
	Scopes  []string // nil: every scope (a session)
	User    *store.User
	Token   *store.APIToken
	// Session is the browser session (Kind "session").
	Session *store.Session
}

// proxyScopes are what the reverse proxy's callers may do (proxy mode):
// the scopes from before step 4's settings, named one by one, so a scope
// added later is never granted to whoever reaches the proxy.
// The owner's settings are allowed (their secrets are refused by the
// handlers); the server's (instance:admin) never are.
var proxyScopes = []string{scopePostsRead, scopePostsWrite, scopeMediaWrite, scopeDraftsRead, scopeDraftsWrite,
	scopeAccountsRead, scopeAccountsWrite, scopeSettingsRead, scopeSettingsWrite, scopeAdmin}

// key identifies the principal for per-caller state (idempotency keys).
func (p principal) key() string { return p.Kind + ":" + p.ID }

func (p principal) can(scope string) bool {
	return p.Scopes == nil || slices.Contains(p.Scopes, scope) ||
		slices.Contains(p.Scopes, scopeAdmin) && slices.Contains(adminImplies, scope)
}

type principalKey struct{}

func principalFrom(ctx context.Context) (principal, bool) {
	p, ok := ctx.Value(principalKey{}).(principal)
	return p, ok
}

// v1Route is one /v1 operation.
type v1Route struct {
	Method, Path string // Path in ServeMux syntax: /v1/accounts/{id}
	Scope        string // "" = any authenticated caller
	Idempotent   bool   // a create that honors Idempotency-Key
	Public       bool   // no authentication (the OpenAPI document)
	// SessionOnly: only a signed-in browser session may call it (a token
	// gets 403 session_required; the proxy, 403 sign_in_required).
	SessionOnly bool
	// NoProxy: behind the reverse proxy alone (proxy mode) it answers 403
	// sign_in_required: it writes or shows credentials, which needs the
	// owner signed in.
	NoProxy bool
	// ReturnsSecret: its answer carries a secret or a capability (a new
	// token, a revealed key), so it is never Idempotent (a stored answer
	// would keep the secret).
	ReturnsSecret bool
	// Actions are the custom methods (AIP-136) a POST route serves as
	// Path:action, documented one by one; the handler splits them off,
	// since a ServeMux wildcard must be a whole segment.
	Actions []string
	// ActionRules tighten the route's own rules for some of its actions.
	ActionRules map[string]v1ActionRule
	// IdemPrint, on an Idempotent route, is what the request fingerprint
	// covers instead of the body as sent: a body holding credentials has
	// them replaced by a keyed MAC, so the stored fingerprint can't be
	// used to test guesses.
	IdemPrint func(body []byte) []byte
	Handler   http.HandlerFunc
}

// v1ActionRule is one action's rules, on top of its route's.
type v1ActionRule struct {
	Scope         string
	SessionOnly   bool
	NoProxy       bool
	ReturnsSecret bool
}

// v1Gate is who may call a route (or one of its actions).
type v1Gate struct {
	Scope                string
	SessionOnly, NoProxy bool
}

// v1Routes lists every /v1 operation. Handlers are added here and nowhere
// else, so the conformance test sees them all.
func (a *API) v1Routes() []v1Route {
	rs := []v1Route{
		{Method: "GET", Path: "/v1/openapi.json", Public: true, Handler: a.v1OpenAPI},
		{Method: "GET", Path: "/v1/instance", Handler: a.v1Instance},
		{Method: "GET", Path: "/v1/destinations", Scope: scopeAccountsRead, Handler: a.v1Destinations},
		{Method: "GET", Path: "/v1/accounts", Scope: scopeAccountsRead, Handler: a.v1Accounts},
		// Adding an account stores its credentials; a replay answers the
		// account, which never carries them.
		{Method: "POST", Path: "/v1/accounts", Scope: scopeAccountsWrite, Idempotent: true, NoProxy: true,
			IdemPrint: a.accountCreatePrint, Handler: a.v1CreateAccount},
		{Method: "GET", Path: "/v1/accounts/{id}", Scope: scopeAccountsRead, Handler: a.v1Account},
		// Changing credentials behind the proxy alone is refused by the
		// handler; labels and flags work there.
		{Method: "PATCH", Path: "/v1/accounts/{id}", Scope: scopeAccountsWrite, Handler: a.v1PatchAccount},
		// Deleting needs re-authentication, so a signed-in owner.
		{Method: "DELETE", Path: "/v1/accounts/{id}", Scope: scopeAccountsWrite, SessionOnly: true, Handler: a.v1DeleteAccount},
		{Method: "POST", Path: "/v1/accounts/{id}", Scope: scopeAccountsWrite, Actions: []string{"check", "reveal", "adopt", "reconnect"},
			ActionRules: map[string]v1ActionRule{
				"reveal": {SessionOnly: true, NoProxy: true, ReturnsSecret: true},
				"adopt":  {SessionOnly: true, NoProxy: true},
				// The answer's sign-in address holds the flow's state.
				"reconnect": {SessionOnly: true, NoProxy: true, ReturnsSecret: true},
			}, Handler: a.v1AccountAction},
		// Connecting by signing in at the network (spec §6.1): a browser
		// flow, the owner's own; the sign-in address holds its state.
		{Method: "POST", Path: "/v1/accounts:connect", Scope: scopeAccountsWrite, SessionOnly: true, NoProxy: true,
			ReturnsSecret: true, Handler: a.v1ConnectAccount},
		{Method: "GET", Path: "/v1/connect-flows/{id}", Scope: scopeAccountsWrite, SessionOnly: true, NoProxy: true,
			Handler: a.v1ConnectFlow},
		{Method: "POST", Path: "/v1/connect-flows/{id}", Scope: scopeAccountsWrite, SessionOnly: true, NoProxy: true,
			Actions: []string{"choose"}, Handler: a.v1ConnectFlowAction},
		// Account sets: selections of accounts the composer starts from.
		{Method: "GET", Path: "/v1/account-sets", Scope: scopeAccountsRead, Handler: a.v1AccountSets},
		{Method: "POST", Path: "/v1/account-sets", Scope: scopeAccountsWrite, Handler: a.v1CreateAccountSet},
		{Method: "GET", Path: "/v1/account-sets/{id}", Scope: scopeAccountsRead, Handler: a.v1AccountSet},
		{Method: "PATCH", Path: "/v1/account-sets/{id}", Scope: scopeAccountsWrite, Handler: a.v1PatchAccountSet},
		{Method: "DELETE", Path: "/v1/account-sets/{id}", Scope: scopeAccountsWrite, Handler: a.v1DeleteAccountSet},
		// Settings (spec §7.4). Secret settings behind the proxy alone, and
		// instance secrets without re-authentication, are refused by the
		// handlers.
		{Method: "GET", Path: "/v1/settings", Scope: scopeSettingsRead, Handler: a.v1GetSettings},
		{Method: "PATCH", Path: "/v1/settings", Scope: scopeSettingsWrite, Handler: a.v1PatchSettings},
		{Method: "POST", Path: "/v1/settings/{id}", Scope: scopeSettingsWrite, Actions: []string{"copy-from-env", "generate", "test"},
			ActionRules: map[string]v1ActionRule{
				// The answer is the new value, shown once.
				"generate": {NoProxy: true, ReturnsSecret: true},
			}, Handler: a.v1SettingAction},
		{Method: "GET", Path: "/v1/instance/settings", Scope: scopeInstanceAdmin, NoProxy: true, Handler: a.v1GetInstanceSettings},
		{Method: "PATCH", Path: "/v1/instance/settings", Scope: scopeInstanceAdmin, NoProxy: true, Handler: a.v1PatchInstanceSettings},
		{Method: "POST", Path: "/v1/instance/settings/{id}", Scope: scopeInstanceAdmin, NoProxy: true,
			Actions: []string{"copy-from-env", "generate", "test"}, ActionRules: map[string]v1ActionRule{
				"generate": {NoProxy: true, ReturnsSecret: true},
			}, Handler: a.v1InstanceSettingAction},
		{Method: "GET", Path: "/v1/instance/system", Scope: scopeInstanceAdmin, NoProxy: true, Handler: a.v1System},
		{Method: "GET", Path: "/v1/me", Handler: a.v1Me},
		// The signed-in owner's ways of signing in and sessions.
		{Method: "GET", Path: "/v1/me/sign-in", SessionOnly: true, Handler: a.v1SignIn},
		{Method: "POST", Path: "/v1/me/passkeys:begin", SessionOnly: true, Handler: a.v1BeginPasskey},
		{Method: "POST", Path: "/v1/me/passkeys", SessionOnly: true, Handler: a.v1AddPasskey},
		{Method: "PATCH", Path: "/v1/me/passkeys/{id}", SessionOnly: true, Handler: a.v1RenamePasskey},
		{Method: "DELETE", Path: "/v1/me/passkeys/{id}", SessionOnly: true, Handler: a.v1RemovePasskey},
		{Method: "PUT", Path: "/v1/me/password", SessionOnly: true, Handler: a.v1SetPassword},
		{Method: "DELETE", Path: "/v1/me/password", SessionOnly: true, Handler: a.v1RemovePassword},
		{Method: "GET", Path: "/v1/me/sessions", SessionOnly: true, Handler: a.v1ListSessions},
		{Method: "DELETE", Path: "/v1/me/sessions/{id}", SessionOnly: true, Handler: a.v1EndSession},
		{Method: "GET", Path: "/v1/me/sign-in-events", SessionOnly: true, Handler: a.v1SignInEvents},
		{Method: "GET", Path: "/v1/tokens", Scope: scopeAdmin, Handler: a.v1ListTokens},
		// Not idempotent: the response carries a secret we never store.
		{Method: "POST", Path: "/v1/tokens", Scope: scopeAdmin, ReturnsSecret: true, Handler: a.v1CreateToken},
		{Method: "GET", Path: "/v1/tokens/{id}", Scope: scopeAdmin, Handler: a.v1GetToken},
		{Method: "DELETE", Path: "/v1/tokens/{id}", Scope: scopeAdmin, Handler: a.v1RevokeToken},
		{Method: "GET", Path: "/v1/posts", Scope: scopePostsRead, Handler: a.v1ListPosts},
		{Method: "POST", Path: "/v1/posts", Scope: scopePostsWrite, Idempotent: true, Handler: a.v1CreatePost},
		{Method: "POST", Path: "/v1/posts:preview", Scope: scopePostsWrite, Handler: a.v1PreviewPost},
		{Method: "POST", Path: "/v1/resolve", Scope: scopePostsWrite, Handler: a.v1Resolve},
		{Method: "GET", Path: "/v1/posts/{id}", Scope: scopePostsRead, Handler: a.v1GetPost},
		{Method: "PATCH", Path: "/v1/posts/{id}", Scope: scopePostsWrite, Handler: a.v1PatchPost},
		{Method: "POST", Path: "/v1/posts/{id}", Scope: scopePostsWrite, Actions: []string{"cancel", "retry", "resolve"}, Handler: a.v1PostAction},
		{Method: "GET", Path: "/v1/posts/{id}/events", Scope: scopePostsRead, Handler: a.v1PostEvents},
		{Method: "GET", Path: "/v1/events", Scope: scopePostsRead, Handler: a.v1Events},
		{Method: "GET", Path: "/v1/drafts", Scope: scopeDraftsRead, Handler: a.v1ListDrafts},
		{Method: "POST", Path: "/v1/drafts", Scope: scopeDraftsWrite, Handler: a.v1CreateDraft},
		{Method: "GET", Path: "/v1/drafts/{id}", Scope: scopeDraftsRead, Handler: a.v1GetDraft},
		{Method: "PATCH", Path: "/v1/drafts/{id}", Scope: scopeDraftsWrite, Handler: a.v1PatchDraft},
		{Method: "DELETE", Path: "/v1/drafts/{id}", Scope: scopeDraftsWrite, Handler: a.v1DeleteDraft},
		{Method: "POST", Path: "/v1/drafts/{id}", Scope: scopeDraftsWrite, Actions: []string{"translate"}, Handler: a.v1DraftAction},
		// Not idempotent: a repeat upload stores the same content-addressed
		// file again, under a new id.
		{Method: "POST", Path: "/v1/media", Scope: scopeMediaWrite, Handler: a.v1UploadMedia},
		{Method: "POST", Path: "/v1/media:compress", Scope: scopeMediaWrite, Handler: a.v1CompressMedia},
		{Method: "GET", Path: "/v1/media/{id}", Scope: scopeMediaWrite, Handler: a.v1GetMedia},
		{Method: "PATCH", Path: "/v1/media/{id}", Scope: scopeMediaWrite, Handler: a.v1PatchMedia},
		// Tools. Translating spends the DeepL quota, so it needs posts:write.
		{Method: "POST", Path: "/v1/translate", Scope: scopePostsWrite, Handler: a.v1Translate},
		{Method: "POST", Path: "/v1/verify", Scope: scopePostsRead, Handler: a.v1Verify},
		{Method: "GET", Path: "/v1/accounts/{id}/nostr/relays", Scope: scopeAdmin, Handler: a.v1GetRelays},
		{Method: "PUT", Path: "/v1/accounts/{id}/nostr/relays", Scope: scopeAdmin, Handler: a.v1PutRelays},
		{Method: "POST", Path: "/v1/accounts/{id}/nostr/relays:scan", Scope: scopeAdmin, Handler: a.v1RelaysAction("scan")},
		{Method: "POST", Path: "/v1/accounts/{id}/nostr/relays:sync", Scope: scopeAdmin, Handler: a.v1RelaysAction("sync")},
		// Its own token (PUBLIC_FEED_TOKEN), checked by the handler.
		{Method: "GET", Path: "/v1/public/feed", Public: true, Handler: a.v1PublicFeed},
	}
	return rs
}

// mountV1 registers the /v1 routes on mux, a problem-details 405 (with
// Allow) for a known path called with another method, and a 404 for any
// other /v1 path.
func (a *API) mountV1(mux *http.ServeMux) {
	allow := map[string][]string{}
	var paths []string
	for _, rt := range a.v1Routes() {
		h := http.Handler(rt.Handler)
		if len(rt.ActionRules) > 0 {
			h = a.v1ActionGate(rt.ActionRules, h)
		}
		if rt.Idempotent {
			h = a.withIdempotency(rt.IdemPrint, h)
		}
		if !rt.Public {
			h = a.v1Auth(v1Gate{Scope: rt.Scope, SessionOnly: rt.SessionOnly, NoProxy: rt.NoProxy}, h)
		}
		mux.Handle(rt.Method+" "+rt.Path, v1Headers(v1Recover(h)))
		if _, seen := allow[rt.Path]; !seen {
			paths = append(paths, rt.Path)
		}
		if len(rt.Actions) == 0 { // POST to {id}:action, not to {id}
			allow[rt.Path] = append(allow[rt.Path], rt.Method)
		}
		if rt.Method == http.MethodGet {
			allow[rt.Path] = append(allow[rt.Path], http.MethodHead)
		}
	}
	for _, p := range paths {
		methods := strings.Join(allow[p], ", ")
		mux.Handle(p, v1Headers(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Allow", methods)
			problem(w, r, http.StatusMethodNotAllowed, codeMethodNotAllowed, r.Method+" is not allowed here; use "+methods)
		})))
	}
	mux.Handle("/v1/", v1Headers(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		problem(w, r, http.StatusNotFound, codeNotFound, "no such /v1 route: "+r.Method+" "+r.URL.Path)
	})))
}

// v1Headers marks every /v1 response uncacheable: answers depend on who
// asks, and some carry secrets.
func v1Headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// v1Auth authenticates the request and checks it against g. A bearer
// token wins over a session cookie: a script that sends one means it.
func (a *API) v1Auth(g v1Gate, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := a.v1Principal(w, r)
		if !ok {
			return
		}
		if !a.pass(w, r, p, g) {
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}

// signInRequired is the answer behind the reverse proxy alone to what
// needs the owner signed in.
const signInRequired = "set up sign-in (AUTH_MODE=local, or OIDC) to do this; the reverse proxy alone can't"

// pass checks the principal against g; on false it has answered. In
// proxy mode a NoProxy route is refused to tokens too: credentials are
// managed only where the owner signs in.
func (a *API) pass(w http.ResponseWriter, r *http.Request, p principal, g v1Gate) bool {
	switch {
	case p.Kind == "proxy" && g.SessionOnly, g.NoProxy && (p.Kind == "proxy" || a.mode() == ModeProxy):
		problem(w, r, http.StatusForbidden, codeSignInRequired, signInRequired)
		return false
	case g.SessionOnly && p.Kind != "session":
		problem(w, r, http.StatusForbidden, codeSessionRequired, "only the owner, signed in in a browser, can do this; a token can't")
		return false
	case g.Scope != "" && !p.can(g.Scope):
		w.Header().Set("WWW-Authenticate", `Bearer realm="publisher", error="insufficient_scope", scope="`+g.Scope+`"`)
		problem(w, r, http.StatusForbidden, codeInsufficientScope, "this token lacks the "+g.Scope+" scope")
		return false
	}
	return true
}

// v1ActionGate applies an action's own rules (inside v1Auth, which
// checked the route's).
func (a *API) v1ActionGate(rules map[string]v1ActionRule, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, action, _ := strings.Cut(r.PathValue("id"), ":")
		if rule, ok := rules[action]; ok {
			p, _ := principalFrom(r.Context())
			if !a.pass(w, r, p, v1Gate{Scope: rule.Scope, SessionOnly: rule.SessionOnly, NoProxy: rule.NoProxy}) {
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (a *API) v1Principal(w http.ResponseWriter, r *http.Request) (principal, bool) {
	h := r.Header.Get("Authorization")
	if _, found := bearerToken(h); h != "" && !found && a.mode() == ModeProxy {
		// Proxy mode: the proxy in front is the gate, and one that uses
		// Basic auth has the browser send its Authorization on every
		// request. Only a Bearer token is ours to read.
		h = ""
	}
	if h != "" {
		raw, found := bearerToken(h)
		if !found {
			w.Header().Set("WWW-Authenticate", `Bearer realm="publisher"`)
			problem(w, r, http.StatusUnauthorized, codeUnauthenticated, "Authorization must be a Bearer token")
			return principal{}, false
		}
		if a.Store == nil {
			problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "no database")
			return principal{}, false
		}
		t, err := a.Store.APITokenByRaw(raw)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="publisher", error="invalid_token"`)
			problem(w, r, http.StatusUnauthorized, codeUnauthenticated, "the token is unknown, revoked or expired")
			return principal{}, false
		}
		scopes := t.Scopes
		if scopes == nil {
			scopes = []string{}
		}
		return principal{Kind: "token", ID: t.ID, OwnerID: t.OwnerID, Scopes: scopes, Token: &t}, true
	}
	if a.mode() == ModeProxy {
		// Proxy mode: the reverse proxy in front is the gate, as for the
		// rest of the app.
		return principal{Kind: "proxy", ID: "proxy", OwnerID: store.DefaultOwner, Scopes: proxyScopes}, true
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		if s, u, ok := a.sessionUser(c.Value); ok {
			return principal{Kind: "session", ID: u.ID, OwnerID: store.DefaultOwner, User: &u, Session: &s}, true
		}
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="publisher"`)
	problem(w, r, http.StatusUnauthorized, codeUnauthenticated, "sign in, or send Authorization: Bearer <token>")
	return principal{}, false
}

// v1Recover answers a panicking handler with a 500 problem.
// If the handler had already started its response, the connection is
// aborted instead: a problem can't be appended to a half-sent body.
func v1Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &startedWriter{ResponseWriter: w}
		defer func() {
			if p := recover(); p != nil {
				if p == http.ErrAbortHandler {
					panic(p)
				}
				slog.ErrorContext(r.Context(), "v1: handler panicked", "path", r.URL.Path, "panic", p)
				if sw.started {
					panic(http.ErrAbortHandler)
				}
				problem(w, r, http.StatusInternalServerError, codeInternal, "internal error")
			}
		}()
		next.ServeHTTP(sw, r)
	})
}

// startedWriter notes whether a response has begun.
type startedWriter struct {
	http.ResponseWriter
	started bool
}

func (s *startedWriter) WriteHeader(code int) {
	s.started = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *startedWriter) Write(b []byte) (int, error) {
	s.started = true
	return s.ResponseWriter.Write(b)
}

func (s *startedWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// maxV1Body caps a JSON request body.
const maxV1Body = 1 << 20

// decodeJSON reads a JSON object from the request body into v, rejecting
// unknown fields, any other JSON value (`null` would read as an empty
// object) and trailing data; on failure it answers a problem and returns
// false.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	// Content-Type is required: a cross-site form or no-cors fetch can't
	// send application/json without a CORS preflight, so this backs up the
	// Origin check for cookie-authenticated writes.
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (mt != "application/json" && (r.Method != http.MethodPatch || mt != "application/merge-patch+json")) {
		problem(w, r, http.StatusUnsupportedMediaType, codeUnsupportedMediaType, "send Content-Type: application/json")
		return false
	}
	body := bufio.NewReader(http.MaxBytesReader(w, r.Body, maxV1Body))
	if c, err := firstNonSpace(body); err == nil && c != '{' {
		problem(w, r, http.StatusBadRequest, codeInvalidJSON, "the body must be a JSON object")
		return false
	}
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		switch {
		case errors.As(err, &mbe):
			problem(w, r, http.StatusRequestEntityTooLarge, codeTooLarge, "the body is over 1 MiB")
		case errors.Is(err, io.EOF):
			problem(w, r, http.StatusBadRequest, codeInvalidJSON, "the body is empty")
		default:
			problem(w, r, http.StatusBadRequest, codeInvalidJSON, err.Error())
		}
		return false
	}
	if dec.More() {
		problem(w, r, http.StatusBadRequest, codeInvalidJSON, "the body has data after its JSON value")
		return false
	}
	return true
}

// firstNonSpace returns the first byte after JSON whitespace, leaving it
// unread.
func firstNonSpace(r *bufio.Reader) (byte, error) {
	for {
		c, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			return c, r.UnreadByte()
		}
	}
}

// writeV1 writes a JSON resource.
func writeV1(w http.ResponseWriter, status int, body any) {
	b, err := json.Marshal(body)
	if err != nil {
		http.Error(w, "marshal", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}
