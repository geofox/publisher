package api

import (
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
	scopePostsRead    = "posts:read"
	scopePostsWrite   = "posts:write"
	scopeMediaWrite   = "media:write"
	scopeDraftsRead   = "drafts:read"
	scopeDraftsWrite  = "drafts:write"
	scopeAccountsRead = "accounts:read"
	scopeAdmin        = "admin"
)

// v1Scopes are the scopes a token can hold, in documentation order.
var v1Scopes = []string{scopePostsRead, scopePostsWrite, scopeMediaWrite, scopeDraftsRead, scopeDraftsWrite,
	scopeAccountsRead, scopeAdmin}

// principal is who is calling: the owner in a browser session, a token, or
// (with OIDC off) whoever the reverse proxy let through.
type principal struct {
	Kind    string // "session", "token" or "proxy"
	ID      string // the user's or the token's id; "proxy" otherwise
	OwnerID string
	Scopes  []string // nil: every scope (a session or the proxy)
	User    *store.User
	Token   *store.APIToken
}

// key identifies the principal for per-caller state (idempotency keys).
func (p principal) key() string { return p.Kind + ":" + p.ID }

func (p principal) can(scope string) bool {
	return p.Scopes == nil || slices.Contains(p.Scopes, scope) || slices.Contains(p.Scopes, scopeAdmin)
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
	// Actions are the custom methods (AIP-136) a POST route serves as
	// Path:action, documented one by one; the handler splits them off,
	// since a ServeMux wildcard must be a whole segment.
	Actions []string
	Handler http.HandlerFunc
}

// v1Routes lists every /v1 operation. Handlers are added here and nowhere
// else, so the conformance test sees them all.
func (a *API) v1Routes() []v1Route {
	rs := []v1Route{
		{Method: "GET", Path: "/v1/openapi.json", Public: true, Handler: a.v1OpenAPI},
		{Method: "GET", Path: "/v1/instance", Handler: a.v1Instance},
		{Method: "GET", Path: "/v1/destinations", Scope: scopeAccountsRead, Handler: a.v1Destinations},
		{Method: "GET", Path: "/v1/accounts", Scope: scopeAccountsRead, Handler: a.v1Accounts},
		{Method: "GET", Path: "/v1/accounts/{id}", Scope: scopeAccountsRead, Handler: a.v1Account},
		{Method: "GET", Path: "/v1/me", Handler: a.v1Me},
		{Method: "GET", Path: "/v1/tokens", Scope: scopeAdmin, Handler: a.v1ListTokens},
		// Not idempotent: the response carries a secret we never store.
		{Method: "POST", Path: "/v1/tokens", Scope: scopeAdmin, Handler: a.v1CreateToken},
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
		if rt.Idempotent {
			h = a.withIdempotency(h)
		}
		if !rt.Public {
			h = a.v1Auth(rt.Scope, h)
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

// v1Auth authenticates the request and checks scope. A bearer token wins
// over a session cookie: a script that sends one means it.
func (a *API) v1Auth(scope string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := a.v1Principal(w, r)
		if !ok {
			return
		}
		if scope != "" && !p.can(scope) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="publisher", error="insufficient_scope", scope="`+scope+`"`)
			problem(w, r, http.StatusForbidden, codeInsufficientScope, "this token lacks the "+scope+" scope")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}

func (a *API) v1Principal(w http.ResponseWriter, r *http.Request) (principal, bool) {
	if h := r.Header.Get("Authorization"); h != "" {
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
	if !a.authEnabled() {
		// OIDC off: the reverse proxy in front is the gate, as for the rest
		// of the app.
		return principal{Kind: "proxy", ID: "proxy", OwnerID: store.DefaultOwner}, true
	}
	if c, err := r.Cookie(sessionCookie); err == nil && a.Store != nil {
		if u, err := a.Store.SessionUser(c.Value); err == nil {
			return principal{Kind: "session", ID: u.ID, OwnerID: store.DefaultOwner, User: &u}, true
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

// decodeJSON reads a JSON request body into v, rejecting unknown fields
// and trailing data; on failure it answers a problem and returns false.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	// Content-Type is required: a cross-site form or no-cors fetch can't
	// send application/json without a CORS preflight, so this backs up the
	// Origin check for cookie-authenticated writes.
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (mt != "application/json" && (r.Method != http.MethodPatch || mt != "application/merge-patch+json")) {
		problem(w, r, http.StatusUnsupportedMediaType, codeUnsupportedMediaType, "send Content-Type: application/json")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxV1Body))
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
