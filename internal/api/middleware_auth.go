package api

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"github.com/geofox/publisher/internal/httpx"
	"github.com/geofox/publisher/internal/store"
)

type ctxKey int

const userCtxKey ctxKey = iota

// userFromContext returns the authenticated user, if any.
func userFromContext(ctx context.Context) (store.User, bool) {
	u, ok := ctx.Value(userCtxKey).(store.User)
	return u, ok
}

// requireSession gates browser routes. Page requests without a valid session
// redirect to /auth/login; XHR requests get a 401 JSON body.
func (a *API) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err == nil {
			if u, err := a.Store.SessionUser(c.Value); err == nil {
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userCtxKey, u)))
				return
			}
		}
		if wantsHTML(r) {
			http.Redirect(w, r, "/auth/login", http.StatusFound)
			return
		}
		httpx.WriteError(w, http.StatusUnauthorized, "unauthenticated")
	})
}

// wantsHTML distinguishes a browser navigation (redirect target) from a fetch()
// (JSON 401). Sec-Fetch-Mode is the reliable signal; Accept is the fallback.
func wantsHTML(r *http.Request) bool {
	if m := r.Header.Get("Sec-Fetch-Mode"); m != "" {
		return m == "navigate"
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// requireAPIToken gates machine endpoints via an Authorization: Bearer
// token holding scope (or admin): /publish needs posts:write and
// /upload-media media:write, what tokens made before scopes hold.
func (a *API) requireAPIToken(scope string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			httpx.WriteError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		t, err := a.Store.APITokenByRaw(raw)
		if err != nil {
			httpx.WriteError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		if !slices.Contains(t.Scopes, scope) && !slices.Contains(t.Scopes, scopeAdmin) {
			httpx.WriteError(w, http.StatusForbidden, "this token lacks the "+scope+" scope")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerToken reads "Bearer <token>"; the scheme is case-insensitive
// (RFC 9110 §11.1).
func bearerToken(h string) (string, bool) {
	scheme, raw, found := strings.Cut(h, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	raw = strings.TrimSpace(raw)
	return raw, raw != ""
}
