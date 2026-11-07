package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/netip"
	"os"
	"time"

	"github.com/geofox/publisher/internal/httpx"
	"github.com/geofox/publisher/internal/localauth"
	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/web"
)

// Local sign-in (AUTH_MODE=local): passkeys and an optional password,
// first-run setup and re-authentication. Every POST here must come from
// PUBLIC_URL's own pages (Origin exactly equal to it) as JSON; answers are
// JSON, errors problem details.

const (
	ceremonyCookie = "publisher_ceremony"
	setupCookie    = "publisher_setup"
	// deviceCookie marks a browser that signed in as the owner (see
	// localauth.DeviceToken).
	deviceCookie = "publisher_device"
	// maxCredentialBody bounds a passkey response.
	maxCredentialBody = 64 << 10
)

// mountLocal registers local sign-in's routes.
func (a *API) mountLocal(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/methods", a.handleAuthMethods)
	mux.HandleFunc("POST /auth/passkey/begin", a.localPost(a.handlePasskeyBegin))
	mux.HandleFunc("POST /auth/passkey/finish", a.localPost(a.handlePasskeyFinish))
	mux.HandleFunc("POST /auth/password", a.localPost(a.handlePasswordSignIn))
	mux.HandleFunc("POST /auth/logout", a.localForm(a.handleAuthLogout))
	mux.HandleFunc("POST /auth/reauth/begin", a.localPost(a.handleReauthBegin))
	mux.HandleFunc("POST /auth/reauth", a.localPost(a.handleReauth))

	mux.HandleFunc("GET /setup", a.handleSetupPage)
	mux.HandleFunc("GET /setup/state", a.handleSetupStateCheck)
	mux.HandleFunc("POST /setup/exchange", a.setupPost(a.handleSetupExchange))
	mux.HandleFunc("POST /setup/passkey/begin", a.setupPost(a.handleSetupPasskeyBegin))
	mux.HandleFunc("POST /setup/passkey/finish", a.setupPost(a.handleSetupPasskeyFinish))
	mux.HandleFunc("POST /setup/password", a.setupPost(a.handleSetupPassword))
	mux.HandleFunc("/setup/", a.handleSetupGone)
}

// defaultClients reads the peer only (no proxy trusted).
var defaultClients = &httpx.ClientAddrs{}

// clientAddr is the request's client address (TRUSTED_PROXIES).
func (a *API) clientAddr(r *http.Request) netip.Addr {
	if a.Clients != nil {
		return a.Clients.Of(r)
	}
	return defaultClients.Of(r)
}

// localChecks refuses a request local sign-in can't trust: no PUBLIC_URL
// (passkeys and Secure cookies need it), an Origin other than PUBLIC_URL's
// (cross-site, or the app reached by another name), or (json) a body that
// isn't application/json, which a cross-site form can't send.
func (a *API) localChecks(w http.ResponseWriter, r *http.Request, json bool) bool {
	w.Header().Set("Cache-Control", "no-store")
	if a.PublicURL == "" {
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable,
			"sign-in needs PUBLIC_URL: set it to the https address you use for Publisher and restart")
		return false
	}
	if r.Header.Get("Origin") != a.PublicURL {
		problem(w, r, http.StatusForbidden, codeCrossOrigin,
			"sign in from "+a.PublicURL+" (PUBLIC_URL); this request came from elsewhere")
		return false
	}
	if json {
		mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mt != "application/json" {
			problem(w, r, http.StatusUnsupportedMediaType, codeUnsupportedMediaType, "send Content-Type: application/json")
			return false
		}
	}
	return true
}

func (a *API) localPost(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.localChecks(w, r, true) {
			h(w, r)
		}
	}
}

// localForm is localPost for a form post (sign out): Origin is checked,
// the content type isn't.
func (a *API) localForm(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.localChecks(w, r, false) {
			h(w, r)
		}
	}
}

// readBody reads a JSON body up to max bytes.
func readBody(w http.ResponseWriter, r *http.Request, max int64) ([]byte, bool) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			problem(w, r, http.StatusRequestEntityTooLarge, codeTooLarge, "the body is too large")
		} else {
			problem(w, r, http.StatusBadRequest, codeInvalidJSON, "the body could not be read")
		}
		return nil, false
	}
	return b, true
}

// methodsBody tells the sign-in and setup pages what they can offer.
type methodsBody struct {
	Mode      string `json:"mode"`
	PublicURL string `json:"public_url"`
	// Passkeys: PUBLIC_URL is set, so passkeys can be used.
	Passkeys bool `json:"passkeys"`
	// Password: a password is set.
	Password bool `json:"password"`
	// HasPasskey: a passkey for this address exists (made in some
	// browser or on some device).
	HasPasskey bool `json:"has_passkey"`
	// Setup: no way to sign in exists yet.
	Setup bool `json:"setup"`
}

// handleAuthMethods (GET /auth/methods) is open: it says only how one
// could sign in, never anything about the owner's data.
func (a *API) handleAuthMethods(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	done, err := a.setupDone(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "auth: read methods", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "reading the sign-in methods failed")
		return
	}
	body := methodsBody{Mode: ModeLocal, PublicURL: a.PublicURL, Passkeys: a.Local.Passkeys(), Setup: !done}
	if u, err := a.Store.LocalUser(ctx); err == nil {
		if _, err := a.Store.PasswordPHC(ctx, u.ID); err == nil {
			body.Password = true
		}
		if list, err := a.Store.WebAuthnCredentials(ctx, u.ID); err == nil && a.Local.RPID() != "" {
			for _, c := range list {
				body.HasPasskey = body.HasPasskey || c.RPID == a.Local.RPID()
			}
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeV1(w, http.StatusOK, body)
}

func setCeremonyCookie(w http.ResponseWriter, id string) {
	http.SetCookie(w, &http.Cookie{Name: ceremonyCookie, Value: id, Path: "/", HttpOnly: true, Secure: true,
		SameSite: http.SameSiteStrictMode, MaxAge: int(localauth.CeremonyTTL.Seconds())})
}

func clearCeremonyCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: ceremonyCookie, Path: "/", HttpOnly: true, Secure: true,
		SameSite: http.SameSiteStrictMode, MaxAge: -1})
}

func ceremonyID(r *http.Request) string {
	if c, err := r.Cookie(ceremonyCookie); err == nil {
		return c.Value
	}
	return ""
}

// answerLocalErr answers a local sign-in failure. Which part of a sign-in
// failed is never told.
func (a *API) answerLocalErr(w http.ResponseWriter, r *http.Request, err error) {
	var rl *localauth.ErrRateLimited
	var th *localauth.ErrThrottled
	switch {
	case errors.As(err, &rl):
		rateLimited(w, r, rl.Wait, "too many sign-in attempts; try again shortly")
	case errors.As(err, &th) && th.Everyone:
		rateLimited(w, r, th.Wait, "too many wrong passwords from many places: password sign-in is slowed down for a while; "+
			"try again shortly, or use a passkey (a browser you signed in with before isn't slowed down)")
	case errors.As(err, &th):
		rateLimited(w, r, th.Wait, "too many wrong passwords from here; try again later, or use a passkey")
	case errors.Is(err, localauth.ErrBusy):
		rateLimited(w, r, time.Second, localauth.ErrBusy.Error())
	case errors.Is(err, localauth.ErrNoPublicURL):
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, err.Error())
	case errors.Is(err, localauth.ErrCeremony):
		problem(w, r, http.StatusConflict, codeInvalidState, err.Error())
	case errors.Is(err, localauth.ErrTooManyPasskeys):
		problem(w, r, http.StatusConflict, codeInvalidState, err.Error())
	case errors.Is(err, localauth.ErrSignIn), errors.Is(err, localauth.ErrNoPassword):
		problem(w, r, http.StatusUnauthorized, codeUnauthenticated, "that didn't work: sign-in failed")
	default:
		slog.ErrorContext(r.Context(), "auth: local sign-in", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "sign-in failed on the server")
	}
}

// signInEvent records a sign-in event (best effort: logged when it fails).
func (a *API) signInEvent(ctx context.Context, typ string, d store.EventData) {
	if err := a.Store.AddSignInEvent(ctx, store.DefaultOwner, typ, d); err != nil {
		slog.WarnContext(ctx, "auth: record sign-in event", "type", typ, "err", err)
	}
}

// failedSignIn logs and records a failed sign-in from r: a wrong password
// or a passkey that didn't check out (not an expired or unknown passkey
// request, which anyone can send). At most 10 in ten minutes from one
// address are recorded as events; all are logged.
func (a *API) failedSignIn(r *http.Request, method string, err error) {
	ip := a.clientAddr(r)
	addr := ip.String()
	var th *localauth.ErrThrottled
	if errors.As(err, &th) {
		slog.WarnContext(r.Context(), "auth: sign-in throttled", "method", method, "addr", addr, "owner_wide", th.Everyone)
		return
	}
	if !errors.Is(err, localauth.ErrSignIn) && !errors.Is(err, localauth.ErrNoPassword) {
		return
	}
	slog.WarnContext(r.Context(), "auth: sign-in failed", "method", method, "addr", addr)
	if a.Local.FailedEvents.Take(localauth.AddrKey(ip)) == 0 {
		a.signInEvent(r.Context(), store.EventLoginFailed, store.EventData{Method: method, Addr: addr})
	}
}

// knownDevice reports whether r comes from a browser that signed in as
// the owner before (it skips the owner-wide password delay).
func (a *API) knownDevice(r *http.Request) bool {
	c, err := r.Cookie(deviceCookie)
	return err == nil && a.Local.KnownDevice(r.Context(), c.Value)
}

// startSession signs u in: a new session (the cookie's old one, if any, is
// ended) and its cookie.
func (a *API) startSession(w http.ResponseWriter, r *http.Request, u store.User, method string) error {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = a.Store.DeleteSession(c.Value)
	}
	raw, err := a.Store.CreateSessionWith(u.ID, a.sessionTTL(), store.SessionMeta{
		Method: method, UserAgent: r.UserAgent(), Addr: a.clientAddr(r).String()})
	if err != nil {
		return err
	}
	setSessionCookie(w, raw, a.sessionTTL())
	if tok, err := a.Local.DeviceToken(r.Context()); err == nil {
		http.SetCookie(w, &http.Cookie{Name: deviceCookie, Value: tok, Path: "/", HttpOnly: true, Secure: true,
			SameSite: http.SameSiteStrictMode, MaxAge: int(localauth.DeviceTTL.Seconds())})
	} else {
		slog.WarnContext(r.Context(), "auth: make a known-browser token", "err", err)
	}
	a.signInEvent(r.Context(), store.EventLoginSucceeded, store.EventData{Method: method, Addr: a.clientAddr(r).String()})
	return nil
}

func setSessionCookie(w http.ResponseWriter, raw string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: raw, Path: "/", HttpOnly: true, Secure: true,
		SameSite: http.SameSiteLaxMode, Expires: time.Now().Add(ttl),
		MaxAge: int(ttl.Seconds()), // MaxAge takes precedence per RFC 6265
	})
}

// handlePasskeyBegin (POST /auth/passkey/begin) starts a passkey sign-in.
func (a *API) handlePasskeyBegin(w http.ResponseWriter, r *http.Request) {
	opts, id, err := a.Local.BeginLogin(a.clientAddr(r))
	if err != nil {
		a.answerLocalErr(w, r, err)
		return
	}
	setCeremonyCookie(w, id)
	writeV1(w, http.StatusOK, opts)
}

// handlePasskeyFinish (POST /auth/passkey/finish) checks the assertion and
// signs in.
func (a *API) handlePasskeyFinish(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r, maxCredentialBody)
	if !ok {
		return
	}
	clearCeremonyCookie(w)
	u, err := a.Local.FinishLogin(r.Context(), a.clientAddr(r), ceremonyID(r), body)
	if err != nil {
		a.failedSignIn(r, "passkey", err)
		a.answerLocalErr(w, r, err)
		return
	}
	if err := a.startSession(w, r, u, "passkey"); err != nil {
		a.answerLocalErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type passwordReq struct {
	Password string `json:"password"`
}

// handlePasswordSignIn (POST /auth/password) signs in with the password.
func (a *API) handlePasswordSignIn(w http.ResponseWriter, r *http.Request) {
	var req passwordReq
	if !decodeJSON(w, r, &req) {
		return
	}
	u, err := a.Local.CheckPassword(r.Context(), a.clientAddr(r), a.knownDevice(r), req.Password)
	if err != nil {
		a.failedSignIn(r, "password", err)
		a.answerLocalErr(w, r, err)
		return
	}
	if err := a.startSession(w, r, u, "password"); err != nil {
		a.answerLocalErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// serveSignInPage answers a page request without a session in local
// mode: the sign-in page, at the address asked for, never cached.
func (a *API) serveSignInPage(w http.ResponseWriter, _ *http.Request) {
	servePage(w, http.StatusOK, web.SignInPage())
}

func servePage(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// --- First-run setup --------------------------------------------------

// setupDone reports whether a usable way to sign in exists (setup is
// closed). Passkeys made for another host (PUBLIC_URL changed) don't
// count: setup opens again, with a new link in the log.
func (a *API) setupDone(ctx context.Context) (bool, error) {
	return a.Store.HasLocalCredential(ctx, a.Local.RPID())
}

// handleSetupPage (GET /setup) is the setup page until setup is done,
// then 410.
func (a *API) handleSetupPage(w http.ResponseWriter, r *http.Request) {
	done, err := a.setupDone(r.Context())
	if err != nil {
		http.Error(w, "reading the setup state failed", http.StatusInternalServerError)
		return
	}
	if done {
		servePage(w, http.StatusGone, web.SetupDonePage())
		return
	}
	servePage(w, http.StatusOK, web.SetupPage())
}

// handleSetupGone answers any other /setup path: 410 once setup is done,
// else 404.
func (a *API) handleSetupGone(w http.ResponseWriter, r *http.Request) {
	if done, err := a.setupDone(r.Context()); err == nil && done {
		problem(w, r, http.StatusGone, codeGone, "setup is done; sign in instead")
		return
	}
	problem(w, r, http.StatusNotFound, codeNotFound, "no such setup route")
}

// setupPost guards a setup POST: setup still open (else 410), from
// PUBLIC_URL's page, as JSON.
func (a *API) setupPost(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		done, err := a.setupDone(r.Context())
		if err != nil {
			problem(w, r, http.StatusInternalServerError, codeInternal, "reading the setup state failed")
			return
		}
		if done {
			w.Header().Set("Cache-Control", "no-store")
			problem(w, r, http.StatusGone, codeGone, "setup is done; sign in instead")
			return
		}
		if a.localChecks(w, r, true) {
			h(w, r)
		}
	}
}

// setupState is the request's live setup state id, or "": its setup
// link must still be the newest (a newer link ends it).
func (a *API) setupState(r *http.Request) (string, error) {
	c, err := r.Cookie(setupCookie)
	if err != nil {
		return "", nil
	}
	tok, ok := a.Local.Setup.Token(c.Value)
	if !ok {
		return "", nil
	}
	if cur, err := a.Store.SetupTokenCurrent(r.Context(), tok); err != nil || !cur {
		return "", err
	}
	return c.Value, nil
}

func (a *API) needSetupState(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, err := a.setupState(r)
	if err != nil {
		slog.ErrorContext(r.Context(), "setup: read the setup state", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "reading the setup state failed")
		return "", false
	}
	if id == "" {
		problem(w, r, http.StatusUnauthorized, codeUnauthenticated,
			"open the setup link again (this page's setup expired after 10 minutes, or a newer link replaced it): "+
				"run `publisher setup-url` in the container for a new one")
	}
	return id, id != ""
}

// handleSetupStateCheck (GET /setup/state) answers 204 when this browser
// holds a live setup state (it opened a setup link), else 401; 410 once
// setup is done.
func (a *API) handleSetupStateCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	done, err := a.setupDone(r.Context())
	if err != nil {
		problem(w, r, http.StatusInternalServerError, codeInternal, "reading the setup state failed")
		return
	}
	if done {
		problem(w, r, http.StatusGone, codeGone, "setup is done; sign in instead")
		return
	}
	if _, ok := a.needSetupState(w, r); ok {
		w.WriteHeader(http.StatusNoContent)
	}
}

type setupExchangeReq struct {
	Token string `json:"token"`
}

// handleSetupExchange (POST /setup/exchange) trades the setup link's token,
// once, for a setup state held in a cookie only /setup sees.
func (a *API) handleSetupExchange(w http.ResponseWriter, r *http.Request) {
	var req setupExchangeReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Token == "" || len(req.Token) > 128 {
		problem(w, r, http.StatusUnauthorized, codeUnauthenticated, localauth.ErrSetupToken.Error())
		return
	}
	if err := a.Store.ConsumeSetupToken(r.Context(), req.Token); err != nil {
		if !errors.Is(err, store.ErrInvalidToken) {
			slog.ErrorContext(r.Context(), "setup: consume token", "err", err)
			problem(w, r, http.StatusInternalServerError, codeInternal, "checking the setup link failed")
			return
		}
		slog.WarnContext(r.Context(), "setup: a used, expired or unknown setup link was tried", "addr", a.clientAddr(r).String())
		problem(w, r, http.StatusUnauthorized, codeUnauthenticated, localauth.ErrSetupToken.Error())
		return
	}
	id := a.Local.Setup.Start(req.Token)
	http.SetCookie(w, &http.Cookie{Name: setupCookie, Value: id, Path: "/setup", HttpOnly: true, Secure: true,
		SameSite: http.SameSiteStrictMode, MaxAge: int(localauth.SetupStateTTL.Seconds())})
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) handleSetupPasskeyBegin(w http.ResponseWriter, r *http.Request) {
	state, ok := a.needSetupState(w, r)
	if !ok {
		return
	}
	opts, id, err := a.Local.BeginRegister(r.Context(), a.clientAddr(r), localauth.PurposeSetup, state)
	if err != nil {
		a.answerLocalErr(w, r, err)
		return
	}
	setCeremonyCookie(w, id)
	writeV1(w, http.StatusOK, opts)
}

// passkeyFinishReq is a new passkey: its name and the browser's response.
type passkeyFinishReq struct {
	Name       string          `json:"name"`
	Credential json.RawMessage `json:"credential"`
}

func readPasskeyFinish(w http.ResponseWriter, r *http.Request) (passkeyFinishReq, bool) {
	var req passkeyFinishReq
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		problem(w, r, http.StatusUnsupportedMediaType, codeUnsupportedMediaType, "send Content-Type: application/json")
		return req, false
	}
	body, ok := readBody(w, r, maxCredentialBody)
	if !ok {
		return req, false
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Credential) == 0 {
		problem(w, r, http.StatusBadRequest, codeInvalidJSON, "send {\"name\": …, \"credential\": <the browser's response>}")
		return req, false
	}
	return req, true
}

func (a *API) handleSetupPasskeyFinish(w http.ResponseWriter, r *http.Request) {
	state, ok := a.needSetupState(w, r)
	if !ok {
		return
	}
	req, ok := readPasskeyFinish(w, r)
	if !ok {
		return
	}
	clearCeremonyCookie(w)
	c, err := a.Local.FinishRegister(r.Context(), a.clientAddr(r), ceremonyID(r), localauth.PurposeSetup, state, req.Credential, req.Name)
	if err != nil {
		a.answerPasskeyErr(w, r, err)
		return
	}
	u, c, err := a.Store.CreateFirstPasskey(r.Context(), c)
	a.finishSetup(w, r, u, err, store.EventData{Method: "passkey", Passkey: c.ID})
}

func (a *API) handleSetupPassword(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.needSetupState(w, r); !ok {
		return
	}
	var req passwordReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := localauth.CheckPassword(req.Password); err != nil {
		invalid(w, r, []fieldError{{Pointer: "/password", Code: "weak", Detail: err.Error()}})
		return
	}
	phc, err := a.Local.Hasher.Hash(req.Password)
	if err != nil {
		a.answerLocalErr(w, r, err)
		return
	}
	u, err := a.Store.CreateFirstPassword(r.Context(), a.Local.RPID(), phc)
	a.finishSetup(w, r, u, err, store.EventData{Method: "password"})
}

// finishSetup closes setup after the first credential was stored (err
// from storing it) and signs the owner in.
func (a *API) finishSetup(w http.ResponseWriter, r *http.Request, u store.User, err error, ev store.EventData) {
	if errors.Is(err, store.ErrSetupDone) {
		problem(w, r, http.StatusGone, codeGone, "setup is done; sign in instead")
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "setup: store the first credential", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "saving the sign-in method failed")
		return
	}
	a.Local.Setup.End()
	if a.SetupTokenFile != "" {
		_ = os.Remove(a.SetupTokenFile)
	}
	http.SetCookie(w, &http.Cookie{Name: setupCookie, Path: "/setup", HttpOnly: true, Secure: true,
		SameSite: http.SameSiteStrictMode, MaxAge: -1})
	ev.Addr = a.clientAddr(r).String()
	a.signInEvent(r.Context(), store.EventSignInAdded, ev)
	slog.InfoContext(r.Context(), "setup: done; sign-in is set up", "method", ev.Method)
	if err := a.startSession(w, r, u, "setup"); err != nil {
		a.answerLocalErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
