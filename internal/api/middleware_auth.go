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

// requireSession gates browser routes. Page requests without a valid session
// redirect to /auth/login; XHR requests get a 401 JSON body.
func (a *API) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err == nil {
			if u, ok := a.sessionUser(c.Value); ok {
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

// sessionUser resolves a session cookie to its user, when the user is
// still on the allowlist: removing someone from it ends their sessions.
func (a *API) sessionUser(raw string) (store.User, bool) {
	u, err := a.Store.SessionUser(raw)
	if err != nil {
		return store.User{}, false
	}
	if a.Allowlist != nil && !a.Allowlist.AllowsUser(u.Subject, u.Email) {
		return store.User{}, false
	}
	return u, true
}

// wantsHTML distinguishes a browser navigation (redirect target) from a fetch()
// (JSON 401). Sec-Fetch-Mode is the reliable signal; Accept is the fallback.
func wantsHTML(r *http.Request) bool {
	if m := r.Header.Get("Sec-Fetch-Mode"); m != "" {
		return m == "navigate"
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
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
