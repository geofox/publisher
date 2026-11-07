package api

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/geofox/publisher/internal/localauth"
	"github.com/geofox/publisher/internal/store"
)

// /v1/me/…: the signed-in owner's ways of signing in, sessions and recent
// sign-in events. Browser sessions only. Adding or removing a way to sign
// in and setting the first password need re-authentication (changing the
// password takes the current one instead); passkeys and the password
// exist in local mode only (with OIDC, the identity provider manages
// sign-in).

type v1PasskeyBody struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	CreatedAt  string  `json:"created_at"`
	LastUsedAt *string `json:"last_used_at"`
	// Synced: the passkey may be copied to the owner's other devices
	// (backup eligible).
	Synced bool `json:"synced"`
	// Usable: made for this address. False: made for another one
	// (PUBLIC_URL changed), so it can't sign in here (RPID says where).
	Usable bool   `json:"usable"`
	RPID   string `json:"rp_id"`
}

func (a *API) v1Passkey(c store.WebAuthnCredential) v1PasskeyBody {
	return v1PasskeyBody{ID: c.ID, Name: c.Name, CreatedAt: rfc3339(c.CreatedAt), LastUsedAt: rfc3339p(c.LastUsedAt),
		Synced: c.Flags&0x08 != 0, // BE (backup eligible) is bit 3 of the authenticator flags
		Usable: c.RPID == a.Local.RPID(), RPID: c.RPID}
}

type v1PasswordBody struct {
	Set       bool    `json:"set"`
	UpdatedAt *string `json:"updated_at"`
}

type v1SignInBody struct {
	Mode     string          `json:"mode"`
	Passkeys []v1PasskeyBody `json:"passkeys"`
	Password v1PasswordBody  `json:"password"`
	// PasskeysAvailable: passkeys can be added here (local mode with
	// PUBLIC_URL).
	PasskeysAvailable bool `json:"passkeys_available"`
}

// localOwner answers 409 outside local mode, where passkeys and the
// password don't exist.
func (a *API) localOwner(w http.ResponseWriter, r *http.Request) (principal, bool) {
	p, _ := principalFrom(r.Context())
	if a.mode() != ModeLocal || p.User == nil || p.User.Source != store.UserLocal {
		problem(w, r, http.StatusConflict, codeInvalidState, "sign-in is managed by your identity provider here (AUTH_MODE="+a.mode()+")")
		return p, false
	}
	return p, true
}

func (a *API) v1SignIn(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	body := v1SignInBody{Mode: a.mode(), Passkeys: []v1PasskeyBody{}}
	if a.mode() == ModeLocal && p.User != nil {
		body.PasskeysAvailable = a.Local.Passkeys()
		list, err := a.Store.WebAuthnCredentials(r.Context(), p.User.ID)
		if err != nil {
			slog.ErrorContext(r.Context(), "v1: list passkeys", "err", err)
			problem(w, r, http.StatusInternalServerError, codeInternal, "reading the passkeys failed")
			return
		}
		for _, c := range list {
			body.Passkeys = append(body.Passkeys, a.v1Passkey(c))
		}
		at, err := a.Store.PasswordSetAt(r.Context(), p.User.ID)
		if err != nil {
			slog.ErrorContext(r.Context(), "v1: read password", "err", err)
			problem(w, r, http.StatusInternalServerError, codeInternal, "reading the password state failed")
			return
		}
		body.Password = v1PasswordBody{Set: !at.IsZero(), UpdatedAt: rfc3339p(at)}
	}
	writeV1(w, http.StatusOK, body)
}

// v1BeginPasskey (POST /v1/me/passkeys:begin) starts adding a passkey: the
// re-authentication for signin.add is checked here and spent by
// v1AddPasskey, so a passkey prompt the owner cancels can be tried again
// without confirming again.
func (a *API) v1BeginPasskey(w http.ResponseWriter, r *http.Request) {
	p, ok := a.localOwner(w, r)
	if !ok || !a.checkReauth(w, r, opSignInAdd, "") {
		return
	}
	opts, id, err := a.Local.BeginRegister(r.Context(), a.clientAddr(r), localauth.PurposeRegister, p.Session.ID)
	if err != nil {
		a.answerPasskeyErr(w, r, err)
		return
	}
	setCeremonyCookie(w, id)
	writeV1(w, http.StatusOK, opts)
}

// v1AddPasskey (POST /v1/me/passkeys {name, credential}) finishes adding a
// passkey begun by v1BeginPasskey in this session, with the same
// re-authentication nonce, which it spends.
func (a *API) v1AddPasskey(w http.ResponseWriter, r *http.Request) {
	p, ok := a.localOwner(w, r)
	if !ok || !a.checkReauth(w, r, opSignInAdd, "") {
		return
	}
	req, ok := readPasskeyFinish(w, r)
	if !ok {
		return
	}
	clearCeremonyCookie(w)
	c, err := a.Local.FinishRegister(r.Context(), a.clientAddr(r), ceremonyID(r), localauth.PurposeRegister, p.Session.ID, req.Credential, req.Name)
	if err != nil {
		a.answerPasskeyErr(w, r, err)
		return
	}
	if !a.requireReauth(w, r, opSignInAdd, "") {
		return
	}
	c, err = a.Store.AddWebAuthnCredential(r.Context(), p.User.ID, c)
	if errors.Is(err, store.ErrDuplicatePasskey) {
		problem(w, r, http.StatusConflict, codeInvalidState, err.Error())
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "v1: store passkey", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "saving the passkey failed")
		return
	}
	a.signInEvent(r.Context(), store.EventSignInAdded, store.EventData{Method: "passkey", Passkey: c.ID, Addr: a.clientAddr(r).String()})
	w.Header().Set("Location", "/v1/me/passkeys/"+c.ID)
	writeV1(w, http.StatusCreated, a.v1Passkey(c))
}

// answerPasskeyErr answers a failed attempt to add a passkey. The caller
// is signed in, so a credential that doesn't check out is a 422 on the
// credential, never a 401 (which would read as "signed out").
func (a *API) answerPasskeyErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, localauth.ErrNoPublicURL):
		problem(w, r, http.StatusConflict, codeInvalidState, err.Error())
	case errors.Is(err, localauth.ErrCeremony):
		problem(w, r, http.StatusConflict, codeInvalidState, "the passkey request expired or isn't this session's: start again")
	case errors.Is(err, localauth.ErrSignIn):
		invalid(w, r, []fieldError{{Pointer: "/credential", Code: "rejected", Detail: "the passkey didn't check out; try again"}})
	default:
		a.answerLocalErr(w, r, err)
	}
}

// canRemove answers 404 when the owner has no such passkey (id) or no
// password (id ""), and 409 when it is the last usable way to sign in, so
// the owner isn't asked to confirm for a removal that can't happen. The
// store checks again when it removes.
func (a *API) canRemove(w http.ResponseWriter, r *http.Request, p principal, id string) bool {
	ctx := r.Context()
	list, err := a.Store.WebAuthnCredentials(ctx, p.User.ID)
	if err != nil {
		a.answerMethodErr(w, r, err, "")
		return false
	}
	at, err := a.Store.PasswordSetAt(ctx, p.User.ID)
	if err != nil {
		a.answerMethodErr(w, r, err, "")
		return false
	}
	found, usableIt, others := false, !at.IsZero(), 0
	if id == "" {
		found = !at.IsZero()
	} else if !at.IsZero() {
		others++
	}
	for _, c := range list {
		usable := c.RPID == a.Local.RPID() || a.Local.RPID() == ""
		if c.ID == id {
			found, usableIt = true, usable
		} else if usable {
			others++
		}
	}
	switch {
	case !found && id == "":
		a.answerMethodErr(w, r, sql.ErrNoRows, "no password is set")
	case !found:
		a.answerMethodErr(w, r, sql.ErrNoRows, "no passkey "+id)
	case usableIt && others == 0:
		a.answerMethodErr(w, r, store.ErrLastSignInMethod, "")
	default:
		return true
	}
	return false
}

type v1RenamePasskeyReq struct {
	Name string `json:"name"`
}

func (a *API) v1RenamePasskey(w http.ResponseWriter, r *http.Request) {
	p, ok := a.localOwner(w, r)
	if !ok {
		return
	}
	var req v1RenamePasskeyReq
	if !decodeJSON(w, r, &req) {
		return
	}
	name := localauth.CleanPasskeyName(req.Name)
	if name == "" {
		invalid(w, r, []fieldError{{Pointer: "/name", Code: "invalid", Detail: "1 to 60 characters, no control characters"}})
		return
	}
	id := r.PathValue("id")
	if err := a.Store.RenameWebAuthnCredential(r.Context(), p.User.ID, id, name); err != nil {
		a.answerMethodErr(w, r, err, "no passkey "+id)
		return
	}
	list, _ := a.Store.WebAuthnCredentials(r.Context(), p.User.ID)
	for _, c := range list {
		if c.ID == id {
			writeV1(w, http.StatusOK, a.v1Passkey(c))
			return
		}
	}
	problem(w, r, http.StatusNotFound, codeNotFound, "no passkey "+id)
}

// answerMethodErr answers a failed change to a sign-in method.
func (a *API) answerMethodErr(w http.ResponseWriter, r *http.Request, err error, notFound string) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		problem(w, r, http.StatusNotFound, codeNotFound, notFound)
	case errors.Is(err, store.ErrLastSignInMethod):
		problem(w, r, http.StatusConflict, codeLastSignInMethod, "this is your last way to sign in: add a passkey or set a password first")
	default:
		slog.ErrorContext(r.Context(), "v1: change sign-in method", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "changing the sign-in method failed")
	}
}

// endOtherSessions ends every session of the owner but the caller's (and
// their nonces): after a way to sign in was removed or the password
// changed, whoever used it elsewhere is signed out.
func (a *API) endOtherSessions(r *http.Request, p principal) {
	if err := a.Store.DeleteOtherSessions(r.Context(), p.User.ID, p.Session.ID); err != nil {
		slog.ErrorContext(r.Context(), "v1: end other sessions", "err", err)
	}
}

func (a *API) v1RemovePasskey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, ok := a.localOwner(w, r)
	if !ok || !a.canRemove(w, r, p, id) || !a.requireReauth(w, r, opSignInRemove, "") {
		return
	}
	if err := a.Store.RemoveWebAuthnCredential(r.Context(), p.User.ID, id, a.Local.RPID()); err != nil {
		a.answerMethodErr(w, r, err, "no passkey "+id)
		return
	}
	a.endOtherSessions(r, p)
	a.signInEvent(r.Context(), store.EventSignInRemoved, store.EventData{Method: "passkey", Passkey: id, Addr: a.clientAddr(r).String()})
	w.WriteHeader(http.StatusNoContent)
}

type v1SetPasswordReq struct {
	Password        string  `json:"password"`
	CurrentPassword *string `json:"current_password,omitempty"`
}

// v1SetPassword (PUT /v1/me/password) sets or changes the password. The
// first one needs re-authentication for password.set; changing it needs
// the current password, which is itself a fresh proof (the same check as
// re-authenticating with it), so no nonce. Other sessions are ended.
func (a *API) v1SetPassword(w http.ResponseWriter, r *http.Request) {
	p, ok := a.localOwner(w, r)
	if !ok {
		return
	}
	var req v1SetPasswordReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := localauth.CheckPassword(req.Password); err != nil {
		invalid(w, r, []fieldError{{Pointer: "/password", Code: "weak", Detail: err.Error()}})
		return
	}
	_, err := a.Store.PasswordPHC(r.Context(), p.User.ID)
	hasOne := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		a.answerMethodErr(w, r, err, "")
		return
	}
	if hasOne && req.CurrentPassword == nil {
		invalid(w, r, []fieldError{{Pointer: "/current_password", Code: "required", Detail: "enter your current password to change it"}})
		return
	}
	if !hasOne && !a.requireReauth(w, r, opPasswordSet, "") {
		return
	}
	if hasOne {
		if _, err := a.Local.CheckPassword(r.Context(), a.clientAddr(r), a.knownDevice(r), *req.CurrentPassword); err != nil {
			a.failedSignIn(r, "password", err)
			if errors.Is(err, localauth.ErrSignIn) {
				invalid(w, r, []fieldError{{Pointer: "/current_password", Code: "wrong", Detail: "that isn't your current password"}})
				return
			}
			a.answerLocalErr(w, r, err)
			return
		}
	}
	phc, err := a.Local.Hasher.Hash(req.Password)
	if err != nil {
		a.answerLocalErr(w, r, err)
		return
	}
	if err := a.Store.SetPassword(r.Context(), p.User.ID, phc); err != nil {
		a.answerMethodErr(w, r, err, "")
		return
	}
	a.endOtherSessions(r, p)
	ev := store.EventSignInAdded
	if hasOne {
		ev = store.EventSignInChanged
	}
	a.signInEvent(r.Context(), ev, store.EventData{Method: "password", Addr: a.clientAddr(r).String()})
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) v1RemovePassword(w http.ResponseWriter, r *http.Request) {
	p, ok := a.localOwner(w, r)
	if !ok || !a.canRemove(w, r, p, "") || !a.requireReauth(w, r, opSignInRemove, "") {
		return
	}
	if err := a.Store.RemovePassword(r.Context(), p.User.ID, a.Local.RPID()); err != nil {
		a.answerMethodErr(w, r, err, "no password is set")
		return
	}
	a.endOtherSessions(r, p)
	a.signInEvent(r.Context(), store.EventSignInRemoved, store.EventData{Method: "password", Addr: a.clientAddr(r).String()})
	w.WriteHeader(http.StatusNoContent)
}

type v1SessionBody struct {
	ID         string  `json:"id"`
	Current    bool    `json:"current"`
	Method     string  `json:"method"`
	UserAgent  string  `json:"user_agent"`
	Addr       string  `json:"addr"`
	CreatedAt  string  `json:"created_at"`
	LastSeenAt *string `json:"last_seen_at"`
	ExpiresAt  string  `json:"expires_at"`
}

func (a *API) v1ListSessions(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	list, err := a.Store.ListSessions(r.Context(), p.User.ID)
	if err != nil {
		slog.ErrorContext(r.Context(), "v1: list sessions", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "reading the sessions failed")
		return
	}
	out := make([]v1SessionBody, 0, len(list))
	for _, s := range list {
		out = append(out, v1SessionBody{ID: "ses_" + s.PublicID, Current: s.ID == p.Session.ID, Method: s.Method,
			UserAgent: s.UserAgent, Addr: s.Addr, CreatedAt: rfc3339(s.CreatedAt), LastSeenAt: rfc3339p(s.LastSeenAt),
			ExpiresAt: rfc3339(s.ExpiresAt)})
	}
	writeV1(w, http.StatusOK, newList(out, nil))
}

// v1EndSession (DELETE /v1/me/sessions/{id}) signs a session out (the
// caller's own included).
func (a *API) v1EndSession(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	id := r.PathValue("id")
	pub, ok := strings.CutPrefix(id, "ses_")
	if !ok || pub == "" {
		problem(w, r, http.StatusNotFound, codeNotFound, "no session "+id)
		return
	}
	if err := a.Store.DeleteSessionByPublicID(r.Context(), p.User.ID, pub); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			problem(w, r, http.StatusNotFound, codeNotFound, "no session "+id)
			return
		}
		slog.ErrorContext(r.Context(), "v1: end session", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "ending the session failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type v1SignInEventBody struct {
	ID        int64    `json:"id"`
	Type      string   `json:"type"`
	CreatedAt string   `json:"created_at"`
	Method    string   `json:"method,omitempty"`
	Addr      string   `json:"addr,omitempty"`
	Passkey   string   `json:"passkey,omitempty"`
	Token     string   `json:"token,omitempty"`
	Scopes    []string `json:"scopes,omitempty"`
}

// Sign-in events shown: the newest 50 sign-ins and changes, and apart the
// newest 20 failed attempts, so failures can't push the rest out.
const (
	signInEventsShown = 50
	failedEventsShown = 20
)

// v1SignInEvents (GET /v1/me/sign-in-events) lists the newest sign-ins
// and changes to sign-in methods and tokens, and the newest failed
// attempts, newest first.
func (a *API) v1SignInEvents(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	evs, err := a.Store.SignInEvents(r.Context(), p.OwnerID, signInEventsShown, failedEventsShown)
	if err != nil {
		slog.ErrorContext(r.Context(), "v1: sign-in events", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "reading the sign-in events failed")
		return
	}
	out := make([]v1SignInEventBody, 0, len(evs))
	for _, e := range evs {
		b := v1SignInEventBody{ID: e.ID, Type: e.Type, CreatedAt: rfc3339(e.CreatedAt), Method: e.Data.Method,
			Addr: e.Data.Addr, Passkey: e.Data.Passkey, Scopes: e.Data.Scopes}
		if e.Data.TokenID != "" {
			b.Token = tokenID(e.Data.TokenID)
		}
		out = append(out, b)
	}
	writeV1(w, http.StatusOK, newList(out, nil))
}
