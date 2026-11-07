package api

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/geofox/publisher/internal/httpx"
	"github.com/geofox/publisher/internal/store"
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
// allowlist, creates a session, and sets the session cookie. A login
// started for re-authentication (its state is held on the server) is
// finished by finishOIDCReauth instead.
func (a *API) handleAuthCallback(w http.ResponseWriter, r *http.Request) {
	if x, ok := a.oidcReauth.take(r.URL.Query().Get("state")); ok {
		a.finishOIDCReauth(w, r, x)
		return
	}
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
	// Clear the login-state cookie now that we've read it. Carry the same
	// security attributes as when it was set (defense in depth; also keeps
	// static cookie scanners happy — every Set-Cookie is HttpOnly+Secure).
	http.SetCookie(w, &http.Cookie{
		Name: loginStateCookie, Path: "/auth", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})

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
		if claims.Email != "" && !claims.EmailVerified && a.Allowlist.AllowsUser("", claims.Email) {
			// Listed, but the provider doesn't say the email is verified
			// (some leave email_verified out): allow by subject instead.
			slog.Warn("oidc email is allowed but not verified by the provider: add the subject to OIDC_ALLOWED_SUBJECTS",
				"subject", claims.Subject, "email", claims.Email)
		} else {
			slog.Warn("oidc identity not allowed", "subject", claims.Subject, "email", claims.Email)
		}
		httpx.WriteError(w, http.StatusForbidden, "not authorized")
		return
	}
	// Only a verified email is kept: it is what a session is re-checked
	// against (sessionUser).
	email := claims.Email
	if !claims.EmailVerified {
		email = ""
	}
	u, err := a.Store.UpsertUser(claims.Subject, email, claims.Name)
	if errors.Is(err, store.ErrNotOIDCUser) {
		slog.Warn("oidc identity holds the local owner's subject; refused", "subject", claims.Subject)
		httpx.WriteError(w, http.StatusForbidden, "not authorized")
		return
	}
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
		MaxAge: int(a.sessionTTL().Seconds()), // MaxAge takes precedence per RFC 6265
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

// handleAuthLogout deletes the session and optionally redirects to the IdP's
// end-session endpoint for single-logout.
func (a *API) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = a.Store.DeleteSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	target := "/"
	if a.EndSession && a.Auth != nil {
		if url := a.Auth.EndSessionURL(a.AppBaseURL + "/"); url != "" {
			target = url
		}
	}
	// The web app signs out with fetch, which sends the page's Origin: a
	// form post from a page with Referrer-Policy no-referrer sends "Origin:
	// null", which the cross-site guard refuses. It then goes where this
	// says.
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		w.Header().Set("Cache-Control", "no-store")
		writeV1(w, http.StatusOK, map[string]string{"redirect": target})
		return
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// Sign-in modes (AUTH_MODE).
const (
	ModeOIDC  = "oidc"
	ModeLocal = "local"
	ModeProxy = "proxy"
)

// mode is how callers sign in: Mode, or for an API built without one
// (tests), OIDC when an authenticator is set, else proxy. Every gate asks
// it: a mode that isn't proxy is never open.
func (a *API) mode() string {
	if a.Mode != "" {
		return a.Mode
	}
	if a.Auth != nil && a.Allowlist != nil {
		return ModeOIDC
	}
	return ModeProxy
}

// checkMode panics when the mode lacks what it needs: a server that can't
// check sign-ins must not start serving.
func (a *API) checkMode() {
	switch a.mode() {
	case ModeOIDC:
		if a.Auth == nil || a.Allowlist == nil {
			panic("api: AUTH_MODE=oidc without an authenticator and allowlist")
		}
	case ModeLocal:
		if a.Local == nil {
			panic("api: AUTH_MODE=local without the local sign-in service")
		}
	case ModeProxy:
	default:
		panic("api: unknown AUTH_MODE " + a.mode())
	}
}
