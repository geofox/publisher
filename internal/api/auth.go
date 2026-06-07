package api

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"golang.org/x/oauth2"

	"github.com/geofox/publisher/internal/httpx"
)

const (
	sessionCookie    = "publisher_session"
	loginStateCookie = "publisher_login"
)

// loginState is stashed (JSON, in a short-lived HttpOnly cookie) between
// /auth/login and /auth/callback. The cookie self-expires in 10 minutes.
type loginState struct {
	State        string `json:"s"`
	Nonce        string `json:"n"`
	PKCEVerifier string `json:"v"`
}

func randB64() string {
	var b [24]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func (a *API) sessionTTL() time.Duration {
	if a.SessionTTL > 0 {
		return a.SessionTTL
	}
	return 168 * time.Hour
}

// handleAuthLogin starts the OIDC flow: stash state/nonce/PKCE, redirect to IdP.
func (a *API) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	ls := loginState{State: randB64(), Nonce: randB64(), PKCEVerifier: oauth2.GenerateVerifier()}
	raw, _ := json.Marshal(ls)
	http.SetCookie(w, &http.Cookie{
		Name: loginStateCookie, Value: base64.RawURLEncoding.EncodeToString(raw),
		Path: "/auth", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
		MaxAge: 600,
	})
	http.Redirect(w, r, a.Auth.AuthCodeURL(ls.State, ls.Nonce, ls.PKCEVerifier), http.StatusFound)
}

// handleAuthCallback verifies the login-state cookie + ID token, applies the
// allowlist, creates a session, and sets the session cookie.
func (a *API) handleAuthCallback(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(loginStateCookie)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "missing login state")
		return
	}
	rawJSON, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad login state")
		return
	}
	var ls loginState
	if err := json.Unmarshal(rawJSON, &ls); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad login state")
		return
	}
	// Clear the login-state cookie now that we've read it.
	http.SetCookie(w, &http.Cookie{Name: loginStateCookie, Path: "/auth", MaxAge: -1})

	if r.URL.Query().Get("state") != ls.State || ls.State == "" {
		httpx.WriteError(w, http.StatusBadRequest, "state mismatch")
		return
	}
	claims, err := a.Auth.Exchange(r.Context(), r.URL.Query().Get("code"), ls.Nonce, ls.PKCEVerifier)
	if err != nil {
		slog.Warn("oidc exchange failed", "err", err)
		httpx.WriteError(w, http.StatusUnauthorized, "authentication failed")
		return
	}
	if err := a.Allowlist.Check(claims); err != nil {
		slog.Warn("oidc identity not allowed", "subject", claims.Subject, "email", claims.Email)
		httpx.WriteError(w, http.StatusForbidden, "not authorized")
		return
	}
	u, err := a.Store.UpsertUser(claims.Subject, claims.Email, claims.Name)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "user upsert failed")
		return
	}
	raw, err := a.Store.CreateSession(u.ID, a.sessionTTL())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "session create failed")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: raw, Path: "/", HttpOnly: true, Secure: true,
		SameSite: http.SameSiteLaxMode, Expires: time.Now().Add(a.sessionTTL()),
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

// handleAuthLogout deletes the session and optionally redirects to the IdP's
// end-session endpoint for single-logout.
func (a *API) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = a.Store.DeleteSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1})
	if a.EndSession {
		if url := a.Auth.EndSessionURL(a.AppBaseURL + "/"); url != "" {
			http.Redirect(w, r, url, http.StatusFound)
			return
		}
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// authEnabled reports whether OIDC auth is active for this API instance.
func (a *API) authEnabled() bool { return a.Auth != nil && a.Allowlist != nil }
