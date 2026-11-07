package api

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/geofox/publisher/internal/accounts"
	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/localauth"
	"github.com/geofox/publisher/internal/store"
)

// Adding, changing, deleting, checking, adopting and revealing accounts
// (accounts:write). Each write that sets credentials opens the account
// with them and asks its network who it is before anything is stored.

// AccountWriter is the registry's side of the account writes
// (accounts.Registry).
type AccountWriter interface {
	Create(ctx context.Context, n accounts.NewAccount) (accounts.Entry, error)
	ChangeCredentials(ctx context.Context, u accounts.CredentialUpdate) (accounts.Entry, error)
	Adopt(ctx context.Context, owner, id, actor, tokenID string) (accounts.Entry, error)
	Delete(ctx context.Context, owner, id, actor, tokenID string) error
	Reveal(ctx context.Context, owner, id string) (map[string]string, error)
	Details(ctx context.Context, row store.Account) (accounts.Details, error)
	CheckNow(ctx context.Context, id string) (destination.Health, bool)
}

// Rate limits of the account writes: each create or credential change
// asks the network (and may spend the account's own limits there); a
// check is one authenticated call.
const (
	accountWritesPerMinute = 10
	accountCheckEvery      = 10 * time.Second
)

// accountLimits are the account writes' rate limits (per owner, and per
// account for checks), set up at first use.
type accountLimits struct {
	once                  sync.Once
	writes, checks, tests localauth.Limiter
	connects              localauth.Limiter
	// exports bounds exports and files read for import (each runs the
	// export file's KDF).
	exports localauth.Limiter
}

func (a *API) limits() *accountLimits {
	l := &a.acctLimits
	l.once.Do(func() {
		l.writes.N, l.writes.Window = accountWritesPerMinute, time.Minute
		if a.AccountWritesPerMinute > 0 {
			l.writes.N = a.AccountWritesPerMinute
		}
		l.checks.N, l.checks.Window = 1, accountCheckEvery
		l.tests.N, l.tests.Window = 1, settingsTestEvery
		l.connects.N, l.connects.Window = connectStartsPerMinute, time.Minute
		if a.AccountWritesPerMinute > 0 {
			l.connects.N = a.AccountWritesPerMinute
		}
		l.exports.N, l.exports.Window = exportsPerMinute, time.Minute
	})
	return l
}

func (a *API) accountWriteWait(owner string) time.Duration { return a.limits().writes.Take(owner) }

func (a *API) accountCheckWait(id string) time.Duration { return a.limits().checks.Take(id) }

// accountCreatePrint is what POST /v1/accounts's idempotency fingerprint
// covers: a keyed MAC of the body (which holds credentials), so the
// stored fingerprint can't be used to test a guess of them.
func (a *API) accountCreatePrint(body []byte) []byte {
	if a.IdemMAC != nil {
		if m := a.IdemMAC(body); m != nil {
			return m
		}
	}
	// No master key: a key of this process. Nothing is stored without a
	// master key, so no fingerprint outlives it.
	a.idemKeyOnce.Do(func() {
		a.idemKey = make([]byte, 32)
		_, _ = rand.Read(a.idemKey)
	})
	m := hmac.New(sha256.New, a.idemKey)
	m.Write(body)
	return m.Sum(nil)
}

// accountWriter is the registry's write side, or it answers 503.
func (a *API) accountWriter(w http.ResponseWriter, r *http.Request) (AccountWriter, bool) {
	if aw, ok := a.Accounts.(AccountWriter); ok {
		return aw, true
	}
	problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "this server can't change accounts")
	return nil, false
}

// actorOf is who makes a change, for its event.
func actorOf(p principal) (actor, tokenID string) {
	if p.Token != nil {
		return p.Kind, p.Token.ID
	}
	return p.Kind, ""
}

// v1AccountCreateIn is POST /v1/accounts's body.
type v1AccountCreateIn struct {
	Type  string `json:"type"`
	Label string `json:"label"`
	// Method is how it connects (GET /v1/destinations lists them); it may
	// be left out when the type has one.
	Method      string         `json:"method"`
	Config      map[string]any `json:"config"`
	Credentials map[string]any `json:"credentials"`
}

// checkLabel checks a label the owner typed ("" is allowed when empty
// means the default).
func checkLabel(s string, emptyOK bool) (string, *fieldError) {
	s = strings.TrimSpace(s)
	switch {
	case s == "" && emptyOK:
		return "", nil
	case s == "":
		return "", &fieldError{Pointer: "/label", Code: "required", Detail: "a label can't be empty"}
	case utf8.RuneCountInString(s) > maxAccountLabel:
		return "", &fieldError{Pointer: "/label", Code: "too_long", Detail: "at most 64 characters"}
	case nameProblem(s) != "":
		return "", &fieldError{Pointer: "/label", Code: "invalid", Detail: nameProblem(s)}
	}
	return s, nil
}

// v1CreateAccount adds an account in the app (spec §5.2): checked, its
// server found when left out, opened and identified, then stored with
// its credentials sealed. 201 with the account; never its credentials.
func (a *API) v1CreateAccount(w http.ResponseWriter, r *http.Request) {
	var in v1AccountCreateIn
	if !decodeJSON(w, r, &in) {
		return
	}
	var errs []fieldError
	if strings.TrimSpace(in.Type) == "" {
		errs = append(errs, fieldError{Pointer: "/type", Code: "required", Detail: "the network: GET /v1/destinations lists them"})
	}
	label, fe := checkLabel(in.Label, true)
	if fe != nil {
		errs = append(errs, *fe)
	}
	if len(errs) > 0 {
		invalid(w, r, errs)
		return
	}
	aw, ok := a.accountWriter(w, r)
	if !ok {
		return
	}
	p, _ := principalFrom(r.Context())
	if wait := a.accountWriteWait(p.OwnerID); wait > 0 {
		rateLimited(w, r, wait, "at most 10 accounts added or changed a minute; try again shortly")
		return
	}
	actor, tokenID := actorOf(p)
	en, err := aw.Create(r.Context(), accounts.NewAccount{Owner: p.OwnerID, Type: strings.TrimSpace(in.Type), Label: label,
		Method: in.Method, Config: in.Config, Credentials: in.Credentials, Actor: actor, TokenID: tokenID})
	if err != nil {
		a.accountWriteError(w, r, "adding the account", err)
		return
	}
	w.Header().Set("Location", "/v1/accounts/"+en.Row.ID)
	w.Header().Set("ETag", accountETag(en.Row))
	writeV1(w, http.StatusCreated, a.v1AccountViews(r, []store.Account{en.Row})[0])
}

// v1AccountCredentialsIn is the part of PATCH /v1/accounts/{id} that
// reconnects an account added in the app.
type v1AccountCredentialsIn struct {
	set         bool
	Method      string
	Config      map[string]any
	Credentials map[string]any
}

// checkCredentialPatch reads method, config and credentials from a
// patch: config is a merge patch (null puts a field back to its
// default); credentials, when sent, replace all of them.
func checkCredentialPatch(in v1AccountPatchIn) (v1AccountCredentialsIn, []fieldError) {
	var out v1AccountCredentialsIn
	var errs []fieldError
	if in.Method != nil {
		out.set = true
		if err := json.Unmarshal(in.Method, &out.Method); err != nil || isJSONNull(in.Method) || strings.TrimSpace(out.Method) == "" {
			errs = append(errs, fieldError{Pointer: "/method", Code: "invalid", Detail: "one of the type's ways of connecting"})
		}
	}
	if in.Config != nil {
		out.set = true
		if err := json.Unmarshal(in.Config, &out.Config); err != nil || isJSONNull(in.Config) {
			errs = append(errs, fieldError{Pointer: "/config", Code: "invalid", Detail: "an object of the fields to change (null puts one back to its default)"})
		}
	}
	if in.Credentials != nil {
		out.set = true
		if err := json.Unmarshal(in.Credentials, &out.Credentials); err != nil || isJSONNull(in.Credentials) {
			errs = append(errs, fieldError{Pointer: "/credentials", Code: "invalid", Detail: "an object of every credential of the way of connecting"})
		} else if out.Credentials == nil {
			out.Credentials = map[string]any{}
		}
	}
	return out, errs
}

// changeAccountCredentials applies the credential part of a patch; on
// false it has answered.
func (a *API) changeAccountCredentials(w http.ResponseWriter, r *http.Request, cur store.Account, c v1AccountCredentialsIn) (store.Account, bool) {
	p, _ := principalFrom(r.Context())
	if (c.Method != "" || c.Credentials != nil) && (p.Kind == "proxy" || a.mode() == ModeProxy) {
		problem(w, r, http.StatusForbidden, codeSignInRequired, signInRequired)
		return cur, false
	}
	aw, ok := a.accountWriter(w, r)
	if !ok {
		return cur, false
	}
	if wait := a.accountWriteWait(p.OwnerID); wait > 0 {
		rateLimited(w, r, wait, "at most 10 accounts added or changed a minute; try again shortly")
		return cur, false
	}
	actor, tokenID := actorOf(p)
	en, err := aw.ChangeCredentials(r.Context(), accounts.CredentialUpdate{Owner: p.OwnerID, ID: cur.ID, Method: c.Method,
		Config: c.Config, Credentials: c.Credentials, IfUpdatedAt: cur.UpdatedAt, Actor: actor, TokenID: tokenID})
	if err != nil {
		a.accountWriteError(w, r, "changing the account", err)
		return cur, false
	}
	return en.Row, true
}

// envChecker says whether the environment configures an env account now
// (accounts.Registry).
type envChecker interface {
	EnvConfigured(row store.Account) bool
}

// v1DeleteAccount deletes an account added in the app (spec §5.5), or one
// from the environment whose variables are no longer set: its
// credentials go, what it still had to send is canceled, its history
// stays. It needs re-authentication for this account.
func (a *API) v1DeleteAccount(w http.ResponseWriter, r *http.Request) {
	cur, ok := a.v1LiveAccount(w, r)
	if !ok {
		return
	}
	aw, ok := a.accountWriter(w, r)
	if !ok {
		return
	}
	if ec, isEC := aw.(envChecker); cur.Source != store.SourceDB && (!isEC || ec.EnvConfigured(cur)) {
		problem(w, r, http.StatusConflict, codeLockedByEnv, "environment variables configure this account: unset them (and restart) to remove it")
		return
	}
	if !a.requireReauth(w, r, opAccountDelete, cur.ID) {
		return
	}
	p, _ := principalFrom(r.Context())
	actor, tokenID := actorOf(p)
	if err := aw.Delete(r.Context(), p.OwnerID, cur.ID, actor, tokenID); err != nil {
		a.accountWriteError(w, r, "deleting the account", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// v1LiveAccount is v1OwnAccount, with a deleted account not found.
func (a *API) v1LiveAccount(w http.ResponseWriter, r *http.Request) (store.Account, bool) {
	cur, ok := a.v1OwnAccount(w, r)
	if ok && !cur.DeletedAt.IsZero() {
		problem(w, r, http.StatusNotFound, codeNotFound, "account "+cur.ID+" was deleted")
		return cur, false
	}
	return cur, ok
}

// v1AccountAction serves POST /v1/accounts/{id}:check, :reveal, :adopt
// and :reconnect.
func (a *API) v1AccountAction(w http.ResponseWriter, r *http.Request) {
	id, action, found := strings.Cut(r.PathValue("id"), ":")
	if !found {
		w.Header().Set("Allow", "DELETE, GET, HEAD, PATCH")
		problem(w, r, http.StatusMethodNotAllowed, codeMethodNotAllowed,
			"POST is not allowed here; use GET, HEAD, PATCH, DELETE, or POST to :check, :reveal, :adopt or :reconnect")
		return
	}
	r.SetPathValue("id", id)
	switch action {
	case "check":
		a.v1CheckAccount(w, r)
	case "reveal":
		a.v1RevealAccount(w, r)
	case "adopt":
		a.v1AdoptAccount(w, r)
	case "reconnect":
		a.v1ReconnectAccount(w, r)
	default:
		problem(w, r, http.StatusNotFound, codeNotFound, "no such action :"+action+"; accounts take :check, :reveal, :adopt and :reconnect")
	}
}

// v1CheckAccount asks the account's network, with its credentials,
// whether they work; once per 10 seconds per account.
func (a *API) v1CheckAccount(w http.ResponseWriter, r *http.Request) {
	cur, ok := a.v1LiveAccount(w, r)
	if !ok {
		return
	}
	aw, ok := a.accountWriter(w, r)
	if !ok {
		return
	}
	if wait := a.accountCheckWait(cur.ID); wait > 0 {
		rateLimited(w, r, wait, "an account can be checked once every 10 seconds")
		return
	}
	if _, ok := aw.CheckNow(r.Context(), cur.ID); !ok {
		reason := "it isn't open"
		if en, found := a.Accounts.Get(cur.ID); found && en.Reason != "" {
			reason = en.Reason
		}
		problem(w, r, http.StatusConflict, codeInvalidState, "the account can't be checked now: "+reason)
		return
	}
	a.writeAccountV1(w, r, cur)
}

// revealable reports whether an account of the type has a credential the
// owner may see again.
func revealable(typ string) bool {
	d, ok := destination.Lookup(typ)
	if !ok {
		return false
	}
	for _, m := range d.Manifest().Auth {
		for _, f := range m.Fields {
			if f.Secret && f.Revealable {
				return true
			}
		}
	}
	return false
}

// v1AccountRevealBody is what :reveal answers.
type v1AccountRevealBody struct {
	Credentials map[string]string `json:"credentials"`
}

// v1RevealAccount shows an account's credential that its type lets the
// owner see again (a Nostr key, to keep a copy): a signed-in owner who
// just re-authenticated for this account. Never stored (no-store, not
// idempotent).
func (a *API) v1RevealAccount(w http.ResponseWriter, r *http.Request) {
	cur, ok := a.v1LiveAccount(w, r)
	if !ok {
		return
	}
	switch {
	case cur.Source != store.SourceDB:
		problem(w, r, http.StatusConflict, codeLockedByEnv, "environment variables hold this account's credentials: read them there")
		return
	case !revealable(cur.Type):
		problem(w, r, http.StatusConflict, codeInvalidState, accounts.ErrNotRevealable.Error())
		return
	}
	aw, ok := a.accountWriter(w, r)
	if !ok {
		return
	}
	if err := a.MasterKey.WriteError(); err != nil {
		// Before re-authenticating: the key can't open what's stored.
		a.accountWriteError(w, r, "reading the credentials", err)
		return
	}
	if !a.requireReauth(w, r, opAccountReveal, cur.ID) {
		return
	}
	p, _ := principalFrom(r.Context())
	creds, err := aw.Reveal(r.Context(), p.OwnerID, cur.ID)
	if err != nil {
		a.accountWriteError(w, r, "reading the credentials", err)
		return
	}
	slog.InfoContext(r.Context(), "v1: an account's credential was shown to the owner", "account", cur.ID, "type", cur.Type)
	w.Header().Set("Cache-Control", "no-store")
	writeV1(w, http.StatusOK, v1AccountRevealBody{Credentials: creds})
}

// v1AdoptAccount moves an account from the environment into the app
// (spec §5.6).
func (a *API) v1AdoptAccount(w http.ResponseWriter, r *http.Request) {
	cur, ok := a.v1LiveAccount(w, r)
	if !ok {
		return
	}
	aw, ok := a.accountWriter(w, r)
	if !ok {
		return
	}
	p, _ := principalFrom(r.Context())
	actor, tokenID := actorOf(p)
	en, err := aw.Adopt(r.Context(), p.OwnerID, cur.ID, actor, tokenID)
	if err != nil {
		a.accountWriteError(w, r, "adopting the account", err)
		return
	}
	a.writeAccountV1(w, r, en.Row)
}

// accountWriteError answers an account write's error. Every detail is
// the owner's own words or the network's, scrubbed; none repeats a
// credential.
func (a *API) accountWriteError(w http.ResponseWriter, r *http.Request, doing string, err error) {
	var inv *accounts.InvalidError
	var rej *accounts.RejectedError
	var up *accounts.UpstreamError
	var dup *store.DuplicateAccountError
	switch {
	case errors.As(err, &inv):
		errs := make([]fieldError, 0, len(inv.Problems))
		for _, p := range inv.Problems {
			errs = append(errs, fieldError{Pointer: p.Pointer, Code: p.Code, Detail: p.Detail})
		}
		invalid(w, r, errs)
	case errors.As(err, &rej):
		problem(w, r, http.StatusUnprocessableEntity, codeCredentialsRejected, "the network refused these credentials",
			fieldError{Pointer: "/credentials", Code: "rejected", Detail: rej.Detail})
	case errors.As(err, &up):
		problem(w, r, http.StatusBadGateway, codeUpstream, up.Detail)
	case errors.As(err, &dup):
		writeProblem(w, r, problemBody{Status: http.StatusConflict, Code: codeDuplicateAccount, AccountID: dup.ID,
			Detail: "you already have this account" + labelSuffix(dup.Label) + "; change that one instead"})
	case errors.Is(err, store.ErrNotDBAccount):
		problem(w, r, http.StatusConflict, codeLockedByEnv, "environment variables configure this account: change them, or adopt it into the app first")
	case errors.Is(err, accounts.ErrIdentityChanged):
		problem(w, r, http.StatusConflict, codeIdentityChanged, err.Error())
	case errors.Is(err, accounts.ErrAlreadyInApp), errors.Is(err, accounts.ErrNothingToAdopt), errors.Is(err, accounts.ErrNotRevealable),
		errors.Is(err, accounts.ErrCredentialsUnreadable), errors.Is(err, store.ErrAlreadyAdopted):
		problem(w, r, http.StatusConflict, codeInvalidState, err.Error())
	case errors.Is(err, accounts.ErrMasterKeyRequired):
		problem(w, r, http.StatusConflict, codeMasterKeyRequired, err.Error())
	case errors.Is(err, accounts.ErrMasterKeyMismatch):
		problem(w, r, http.StatusConflict, codeMasterKeyMismatch, err.Error())
	case errors.Is(err, store.ErrAccountNotFound), errors.Is(err, store.ErrAccountDeleted), errors.Is(err, accounts.ErrNoAccount):
		problem(w, r, http.StatusNotFound, codeNotFound, "no account "+r.PathValue("id"))
	case errors.Is(err, store.ErrAccountConflict):
		problem(w, r, http.StatusPreconditionFailed, codePreconditionFailed, "the account changed meanwhile; read it again")
	default:
		slog.ErrorContext(r.Context(), "v1: "+doing+" failed", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, doing+" failed")
	}
}

func labelSuffix(label string) string {
	if label == "" {
		return ""
	}
	return " (" + label + ")"
}
