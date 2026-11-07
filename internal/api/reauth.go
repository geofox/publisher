package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/geofox/publisher/internal/localauth"
	"github.com/geofox/publisher/internal/store"
)

// Re-authentication: some operations need the owner to prove again, just
// now, that they are the owner (someone at an unlocked browser isn't
// enough). A fresh passkey assertion or the password (local mode), or a
// fresh OIDC login (prompt=login, max_age=0, auth_time after the request
// started), gives a nonce good once, for five minutes, for one operation,
// in this browser session. The operation's request carries it in the
// Publisher-Reauth header.

// Operations that need re-authentication (a closed list; later parts add
// theirs).
const (
	opSignInAdd    = "signin.add"
	opSignInRemove = "signin.remove"
	opPasswordSet  = "password.set"
	// opTokenCreate: a token outlives the session, so a signed-in browser
	// creating one confirms it's the owner (any scopes).
	opTokenCreate = "token.create"
	// opAccountDelete and opAccountReveal act on one account, named as
	// the nonce's target.
	opAccountDelete = "account.delete"
	opAccountReveal = "account.reveal"
)

var reauthOps = []string{opSignInAdd, opSignInRemove, opPasswordSet, opTokenCreate, opAccountDelete, opAccountReveal, opInstanceSecrets,
	opExport, opImport}

// targetedOps are the operations whose nonce names what it is for (an
// account id): it can't be spent on another.
var targetedOps = []string{opAccountDelete, opAccountReveal}

// maxReauthTarget bounds a target's length (ids are far shorter).
const maxReauthTarget = 64

const (
	reauthNonceTTL = 5 * time.Minute
	reauthHeader   = "Publisher-Reauth"
	// oidcReauthSkew is how far before the request an identity provider's
	// auth_time may be (clocks differ a little).
	oidcReauthSkew = 10 * time.Second
	// oidcReauthTTL bounds a fresh OIDC login started for re-authentication.
	oidcReauthTTL = 10 * time.Minute
)

// requireReauth checks the request's re-authentication nonce for op (on
// target, "" for none) and spends it. On false it has answered: 403
// sign_in_required behind a proxy, session_required for a token,
// reauth_required (naming the operation) without a valid nonce.
func (a *API) requireReauth(w http.ResponseWriter, r *http.Request, op, target string) bool {
	return a.reauthNonce(w, r, op, target, true)
}

// checkReauth is requireReauth without spending a good nonce: for the
// first step of an operation whose last step spends it (adding a
// passkey). A nonce for something else is spent all the same.
func (a *API) checkReauth(w http.ResponseWriter, r *http.Request, op, target string) bool {
	return a.reauthNonce(w, r, op, target, false)
}

func (a *API) reauthNonce(w http.ResponseWriter, r *http.Request, op, target string, spend bool) bool {
	p, _ := principalFrom(r.Context())
	switch {
	case p.Kind == "proxy" || a.mode() == ModeProxy:
		problem(w, r, http.StatusForbidden, codeSignInRequired,
			"set up sign-in (AUTH_MODE=local, or OIDC) to do this; behind the reverse proxy alone it isn't allowed")
		return false
	case p.Kind != "session" || p.Session == nil:
		problem(w, r, http.StatusForbidden, codeSessionRequired, "only the owner, signed in in a browser, can do this; a token can't")
		return false
	}
	nonce := r.Header.Get(reauthHeader)
	if nonce != "" && len(nonce) <= 128 {
		err := a.Store.ConsumeReauthNonce(r.Context(), nonce, p.Session.ID, op, target, spend)
		if err == nil {
			return true
		}
		if !errors.Is(err, store.ErrInvalidToken) {
			slog.ErrorContext(r.Context(), "reauth: consume nonce", "err", err)
			problem(w, r, http.StatusInternalServerError, codeInternal, "checking the re-authentication failed")
			return false
		}
	}
	writeProblem(w, r, problemBody{Status: http.StatusForbidden, Code: codeReauthRequired, Operation: op, Target: target,
		Detail: "confirm it's you first: re-authenticate for " + op + " and send the nonce in " + reauthHeader})
	return false
}

// reauthSession is the browser session of a /auth/reauth request, or it
// answers 401.
func (a *API) reauthSession(w http.ResponseWriter, r *http.Request) (store.Session, store.User, bool) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		if s, u, ok := a.sessionUser(c.Value); ok {
			return s, u, true
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	problem(w, r, http.StatusUnauthorized, codeUnauthenticated, "sign in first")
	return store.Session{}, store.User{}, false
}

type reauthReq struct {
	Operation string `json:"operation"`
	// Target names what a targeted operation acts on (an account id).
	Target     string          `json:"target,omitempty"`
	Password   *string         `json:"password,omitempty"`
	Credential json.RawMessage `json:"credential,omitempty"`
}

// reauthFor is the operation (and target) a re-authentication is for.
type reauthFor struct {
	Operation string `json:"operation"`
	Target    string `json:"target,omitempty"`
}

type reauthNonceBody struct {
	Nonce     string `json:"nonce"`
	Operation string `json:"operation"`
	Target    string `json:"target,omitempty"`
	ExpiresAt string `json:"expires_at"`
}

// knownOp checks an operation and its target: a targeted operation needs
// one, any other takes none.
func knownOp(w http.ResponseWriter, r *http.Request, f reauthFor) bool {
	switch {
	case !slices.Contains(reauthOps, f.Operation):
		invalid(w, r, []fieldError{{Pointer: "/operation", Code: "unknown_operation", Detail: "one of the operations that need re-authentication"}})
		return false
	case slices.Contains(targetedOps, f.Operation) && (f.Target == "" || len(f.Target) > maxReauthTarget):
		invalid(w, r, []fieldError{{Pointer: "/target", Code: "required", Detail: f.Operation + " needs the id of what it acts on"}})
		return false
	case !slices.Contains(targetedOps, f.Operation) && f.Target != "":
		invalid(w, r, []fieldError{{Pointer: "/target", Code: "not_allowed", Detail: f.Operation + " takes no target"}})
		return false
	}
	return true
}

// issueNonce answers a fresh nonce for f in session s.
func (a *API) issueNonce(w http.ResponseWriter, r *http.Request, s store.Session, f reauthFor) {
	raw, exp, err := a.Store.CreateReauthNonce(r.Context(), s.ID, f.Operation, f.Target, reauthNonceTTL)
	if err != nil {
		slog.ErrorContext(r.Context(), "reauth: create nonce", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "re-authentication failed on the server")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeV1(w, http.StatusOK, reauthNonceBody{Nonce: raw, Operation: f.Operation, Target: f.Target, ExpiresAt: rfc3339(exp)})
}

// handleReauthBegin (POST /auth/reauth/begin {operation}) starts a passkey
// assertion for one operation (local mode).
func (a *API) handleReauthBegin(w http.ResponseWriter, r *http.Request) {
	s, u, ok := a.reauthSession(w, r)
	if !ok {
		return
	}
	var req reauthFor
	if !decodeJSON(w, r, &req) || !knownOp(w, r, req) {
		return
	}
	opts, id, err := a.Local.BeginReauth(r.Context(), a.clientAddr(r), u, s.ID, req.Operation)
	if err != nil {
		if errors.Is(err, localauth.ErrNoPasskey) || errors.Is(err, localauth.ErrNoPublicURL) {
			problem(w, r, http.StatusConflict, codeInvalidState, err.Error())
			return
		}
		a.answerLocalErr(w, r, err)
		return
	}
	setCeremonyCookie(w, id)
	writeV1(w, http.StatusOK, opts)
}

// handleReauth (POST /auth/reauth {operation, credential | password})
// checks the passkey assertion or the password and answers a nonce
// (local mode).
func (a *API) handleReauth(w http.ResponseWriter, r *http.Request) {
	s, u, ok := a.reauthSession(w, r)
	if !ok {
		return
	}
	body, ok := readBody(w, r, maxCredentialBody)
	if !ok {
		return
	}
	var req reauthReq
	if err := json.Unmarshal(body, &req); err != nil {
		problem(w, r, http.StatusBadRequest, codeInvalidJSON, "the body must be {operation, credential | password}")
		return
	}
	if !knownOp(w, r, reauthFor{req.Operation, req.Target}) {
		return
	}
	switch {
	case len(req.Credential) > 0 && req.Password == nil:
		clearCeremonyCookie(w)
		if err := a.Local.FinishReauth(r.Context(), a.clientAddr(r), ceremonyID(r), u, s.ID, req.Operation, req.Credential); err != nil {
			a.failedSignIn(r, "passkey", err)
			a.answerReauthErr(w, r, err, "/credential")
			return
		}
	case req.Password != nil && len(req.Credential) == 0:
		owner, err := a.Local.CheckPassword(r.Context(), a.clientAddr(r), a.knownDevice(r), *req.Password)
		if err == nil && owner.ID != u.ID {
			err = localauth.ErrSignIn
		}
		if err != nil {
			a.failedSignIn(r, "password", err)
			a.answerReauthErr(w, r, err, "/password")
			return
		}
	default:
		invalid(w, r, []fieldError{{Pointer: "", Code: "required", Detail: "send either credential (a passkey) or password"}})
		return
	}
	a.issueNonce(w, r, s, reauthFor{req.Operation, req.Target})
}

// answerReauthErr answers a failed re-authentication. The caller is
// signed in, so a wrong password or a passkey that doesn't check out is a
// 422 on that field, never a 401 (which reads as "signed out"); an
// expired passkey request is a 409.
func (a *API) answerReauthErr(w http.ResponseWriter, r *http.Request, err error, field string) {
	switch {
	case errors.Is(err, localauth.ErrSignIn), errors.Is(err, localauth.ErrNoPassword):
		invalid(w, r, []fieldError{{Pointer: field, Code: "rejected", Detail: "that didn't work; try again"}})
	case errors.Is(err, localauth.ErrCeremony):
		problem(w, r, http.StatusConflict, codeInvalidState, err.Error())
	default:
		a.answerLocalErr(w, r, err)
	}
}

// --- OIDC re-authentication -------------------------------------------

// oidcReauth is a fresh OIDC login started for re-authentication, kept
// on the server (by its state) so the browser can't change when it began
// or what it is for.
type oidcReauth struct {
	op, session, subject string
	nonce, verifier      string
	started              time.Time
}

// oidcReauths holds the logins in flight and the marks they left: a
// completed one lets POST /auth/reauth/claim turn it into a nonce, once,
// within five minutes.
type oidcReauths struct {
	mu       sync.Mutex
	inFlight map[string]oidcReauth // by state
	marks    map[string]time.Time  // session|operation → expiry
}

func (o *oidcReauths) start(state string, x oidcReauth) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.inFlight == nil {
		o.inFlight = map[string]oidcReauth{}
	}
	now := time.Now()
	for k, v := range o.inFlight {
		if now.Sub(v.started) > oidcReauthTTL {
			delete(o.inFlight, k)
		}
	}
	if len(o.inFlight) >= 64 {
		var oldest string
		for k, v := range o.inFlight {
			if oldest == "" || v.started.Before(o.inFlight[oldest].started) {
				oldest = k
			}
		}
		delete(o.inFlight, oldest)
	}
	o.inFlight[state] = x
}

func (o *oidcReauths) take(state string) (oidcReauth, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	x, ok := o.inFlight[state]
	delete(o.inFlight, state)
	if ok && time.Since(x.started) > oidcReauthTTL {
		return oidcReauth{}, false
	}
	return x, ok
}

func (o *oidcReauths) mark(session, op string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.marks == nil {
		o.marks = map[string]time.Time{}
	}
	now := time.Now()
	for k, exp := range o.marks {
		if !now.Before(exp) {
			delete(o.marks, k)
		}
	}
	o.marks[session+"|"+op] = now.Add(reauthNonceTTL)
}

func (o *oidcReauths) claim(session, op string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	k := session + "|" + op
	exp, ok := o.marks[k]
	delete(o.marks, k)
	return ok && time.Now().Before(exp)
}

// handleOIDCReauthStart (GET /auth/reauth/oidc?operation=…) sends the
// signed-in owner to the identity provider for a fresh login.
func (a *API) handleOIDCReauthStart(w http.ResponseWriter, r *http.Request) {
	s, u, ok := a.reauthSession(w, r)
	if !ok {
		return
	}
	op := r.URL.Query().Get("operation")
	if !slices.Contains(reauthOps, op) {
		invalid(w, r, []fieldError{{Parameter: "operation", Code: "unknown_operation", Detail: "one of the operations that need re-authentication"}})
		return
	}
	state, nonce, verifier := randB64(), randB64(), oauth2.GenerateVerifier()
	a.oidcReauth.start(state, oidcReauth{op: op, session: s.ID, subject: u.Subject, nonce: nonce, verifier: verifier, started: time.Now()})
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, a.Auth.AuthCodeURLFresh(state, nonce, verifier), http.StatusFound)
}

// finishOIDCReauth completes a re-authentication login (the callback, for
// a state started by handleOIDCReauthStart). It creates no session and
// leaves the session cookie alone; it leaves a mark that claim turns into
// a nonce.
func (a *API) finishOIDCReauth(w http.ResponseWriter, r *http.Request, x oidcReauth) {
	w.Header().Set("Cache-Control", "no-store")
	c, err := r.Cookie(sessionCookie)
	if err != nil || store.SessionHash(c.Value) != x.session {
		httpx400(w, "this re-authentication was started in another session")
		return
	}
	claims, err := a.Auth.Exchange(r.Context(), r.URL.Query().Get("code"), x.nonce, x.verifier)
	if err != nil {
		slog.WarnContext(r.Context(), "oidc reauth: exchange failed", "err", err)
		http.Error(w, "re-authentication failed", http.StatusUnauthorized)
		return
	}
	switch {
	case claims.Subject != x.subject:
		slog.WarnContext(r.Context(), "oidc reauth: another identity signed in", "subject", claims.Subject)
		http.Error(w, "re-authentication failed: sign in as the same identity", http.StatusForbidden)
		return
	case claims.AuthTime == 0:
		slog.WarnContext(r.Context(), "oidc reauth: the identity provider sent no auth_time (Authelia needs 4.39 or later)")
		http.Error(w, "re-authentication failed: the identity provider didn't say when you signed in (auth_time)", http.StatusForbidden)
		return
	case time.Unix(claims.AuthTime, 0).Before(x.started.Add(-oidcReauthSkew)):
		slog.WarnContext(r.Context(), "oidc reauth: the login wasn't fresh (the identity provider didn't ask again)",
			"auth_time", time.Unix(claims.AuthTime, 0).UTC().Format(time.RFC3339))
		http.Error(w, "re-authentication failed: the identity provider didn't ask you to sign in again (it must honour prompt=login; Authelia 4.39 or later)", http.StatusForbidden)
		return
	}
	a.oidcReauth.mark(x.session, x.op)
	http.Redirect(w, r, "/reauth-done.html", http.StatusFound)
}

func httpx400(w http.ResponseWriter, msg string) { http.Error(w, msg, http.StatusBadRequest) }

// handleOIDCReauthClaim (POST /auth/reauth/claim {operation}) turns a
// completed OIDC re-authentication into a nonce, once.
func (a *API) handleOIDCReauthClaim(w http.ResponseWriter, r *http.Request) {
	s, _, ok := a.reauthSession(w, r)
	if !ok {
		return
	}
	var req reauthFor
	if !decodeJSON(w, r, &req) || !knownOp(w, r, req) {
		return
	}
	if !a.oidcReauth.claim(s.ID, req.Operation) {
		writeProblem(w, r, problemBody{Status: http.StatusForbidden, Code: codeReauthRequired, Operation: req.Operation,
			Detail: "sign in again with your identity provider first (GET /auth/reauth/oidc?operation=" + req.Operation + ")"})
		return
	}
	a.issueNonce(w, r, s, req)
}
