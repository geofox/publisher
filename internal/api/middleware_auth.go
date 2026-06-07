package api

import (
	"context"
	"net/http"
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

// requireAPIToken gates machine endpoints via an Authorization: Bearer token.
func (a *API) requireAPIToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, prefix) {
			httpx.WriteError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		if _, err := a.Store.APITokenByRaw(strings.TrimPrefix(h, prefix)); err != nil {
			httpx.WriteError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		next.ServeHTTP(w, r)
	})
}
