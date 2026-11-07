package api

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/geofox/publisher/internal/store"
)

// /v1/tokens: API tokens with scopes and an optional expiry. All need the
// admin scope. Creating one is deliberately not idempotent: its response
// carries the secret, which is shown once and never stored.

const maxTokenName = 100

type v1TokenBody struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Scopes     []string `json:"scopes"`
	Status     string   `json:"status"` // active, expired or revoked
	CreatedAt  string   `json:"created_at"`
	LastUsedAt *string  `json:"last_used_at"`
	ExpiresAt  *string  `json:"expires_at"`
	RevokedAt  *string  `json:"revoked_at"`
}

func v1Token(t store.APIToken) v1TokenBody {
	status := "active"
	switch {
	case t.Revoked:
		status = "revoked"
	case !t.ExpiresAt.IsZero() && !t.ExpiresAt.After(time.Now()):
		status = "expired"
	}
	scopes := t.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	return v1TokenBody{
		ID: tokenID(t.ID), Name: t.Name, Scopes: scopes, Status: status,
		CreatedAt: rfc3339(t.CreatedAt), LastUsedAt: rfc3339p(t.LastUsedAt),
		ExpiresAt: rfc3339p(t.ExpiresAt), RevokedAt: rfc3339p(t.RevokedAt),
	}
}

func (a *API) v1ListTokens(w http.ResponseWriter, r *http.Request) {
	limit, after, ok := pageParams(w, r)
	if !ok {
		return
	}
	if a.Store == nil {
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "no database")
		return
	}
	p, _ := principalFrom(r.Context())
	all, err := a.Store.ListAPITokens()
	if err != nil {
		slog.ErrorContext(r.Context(), "v1: list tokens", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "listing tokens failed")
		return
	}
	mine := all[:0]
	for _, t := range all {
		if t.OwnerID == p.OwnerID {
			mine = append(mine, t)
		}
	}
	page, next := pageOf(mine, func(t store.APIToken) cursor { return cursor{t.CreatedAt, t.ID} }, true, after, limit)
	out := make([]v1TokenBody, 0, len(page))
	for _, t := range page {
		out = append(out, v1Token(t))
	}
	writeV1(w, http.StatusOK, newList(out, next))
}

// v1OwnToken reads the token named by the path, answering 404 when it is
// not the caller's owner's.
func (a *API) v1OwnToken(w http.ResponseWriter, r *http.Request) (store.APIToken, bool) {
	if a.Store == nil {
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "no database")
		return store.APIToken{}, false
	}
	id := r.PathValue("id")
	sid, ok := parseID(id, prefixToken)
	if !ok {
		problem(w, r, http.StatusNotFound, codeNotFound, "no token "+id)
		return store.APIToken{}, false
	}
	t, err := a.Store.APIToken(sid)
	p, _ := principalFrom(r.Context())
	if errors.Is(err, sql.ErrNoRows) || (err == nil && t.OwnerID != p.OwnerID) {
		problem(w, r, http.StatusNotFound, codeNotFound, "no token "+id)
		return store.APIToken{}, false
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "v1: get token", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "reading the token failed")
		return store.APIToken{}, false
	}
	return t, true
}

func (a *API) v1GetToken(w http.ResponseWriter, r *http.Request) {
	if t, ok := a.v1OwnToken(w, r); ok {
		writeV1(w, http.StatusOK, v1Token(t))
	}
}

// v1RevokeToken revokes a token. Revoking a revoked token succeeds, so a
// retried DELETE gets the same answer.
func (a *API) v1RevokeToken(w http.ResponseWriter, r *http.Request) {
	t, ok := a.v1OwnToken(w, r)
	if !ok {
		return
	}
	if !t.Revoked {
		if err := a.Store.RevokeAPIToken(t.ID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			slog.ErrorContext(r.Context(), "v1: revoke token", "err", err)
			problem(w, r, http.StatusInternalServerError, codeInternal, "revoking the token failed")
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

type v1CreateTokenReq struct {
	Name      string   `json:"name"`
	Scopes    []string `json:"scopes"`
	ExpiresAt *string  `json:"expires_at"`
}

type v1CreatedTokenBody struct {
	v1TokenBody
	// Secret is the bearer value, returned only here.
	Secret string `json:"secret"`
}

func (a *API) v1CreateToken(w http.ResponseWriter, r *http.Request) {
	var req v1CreateTokenReq
	if !decodeJSON(w, r, &req) {
		return
	}
	var errs []fieldError
	name := strings.TrimSpace(req.Name)
	switch {
	case name == "":
		errs = append(errs, fieldError{Pointer: "/name", Code: "required", Detail: "name the token, for example after the script that uses it"})
	case utf8.RuneCountInString(name) > maxTokenName:
		errs = append(errs, fieldError{Pointer: "/name", Code: "too_long", Detail: "at most 100 characters"})
	case nameProblem(name) != "":
		errs = append(errs, fieldError{Pointer: "/name", Code: "invalid", Detail: nameProblem(name)})
	}
	var scopes []string
	if len(req.Scopes) == 0 {
		errs = append(errs, fieldError{Pointer: "/scopes", Code: "required", Detail: "list at least one scope"})
	}
	for i, s := range req.Scopes {
		if !slices.Contains(v1Scopes, s) {
			errs = append(errs, fieldError{Pointer: "/scopes/" + strconv.Itoa(i), Code: "unknown_scope",
				Detail: "one of " + strings.Join(v1Scopes, ", ")})
		}
	}
	// A token can't grant what only the signed-in owner may (re-signing
	// in will be asked for once sign-in has it).
	if p, _ := principalFrom(r.Context()); p.Token != nil {
		for i, s := range req.Scopes {
			if slices.Contains(sessionOnlyScopes, s) {
				errs = append(errs, fieldError{Pointer: "/scopes/" + strconv.Itoa(i), Code: "exceeds_caller",
					Detail: "only the signed-in owner can create a token with " + s + "; a token can't"})
			}
		}
	}
	// Documentation order, without repeats.
	for _, s := range v1Scopes {
		if slices.Contains(req.Scopes, s) {
			scopes = append(scopes, s)
		}
	}
	var expires time.Time
	if req.ExpiresAt != nil {
		// RFC 3339 allows a lowercase "t" and "z"; the expiry is kept to
		// the second, as it is shown.
		t, err := time.Parse(time.RFC3339, strings.ToUpper(*req.ExpiresAt))
		t = t.Truncate(time.Second)
		switch {
		case err != nil:
			errs = append(errs, fieldError{Pointer: "/expires_at", Code: "invalid_time", Detail: "an RFC 3339 time, such as 2027-01-01T00:00:00Z"})
		case !t.After(time.Now()):
			errs = append(errs, fieldError{Pointer: "/expires_at", Code: "in_past", Detail: "the expiry must be in the future"})
		default:
			expires = t
		}
	}
	// A token (always admin here) can't mint a token that outlives it, so a
	// short-lived credential can't make itself permanent through a child.
	if p, _ := principalFrom(r.Context()); p.Token != nil {
		if parent := p.Token.ExpiresAt; !parent.IsZero() && (expires.IsZero() || expires.After(parent)) &&
			(req.ExpiresAt == nil || !slices.ContainsFunc(errs, func(e fieldError) bool { return e.Pointer == "/expires_at" })) {
			errs = append(errs, fieldError{Pointer: "/expires_at", Code: "exceeds_caller",
				Detail: "the calling token expires at " + rfc3339(parent) + "; the new one must expire by then"})
		}
	}
	if len(errs) > 0 {
		invalid(w, r, errs)
		return
	}
	if a.Store == nil {
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "no database")
		return
	}
	// A token that can make tokens or change accounts needs the owner to
	// confirm it's them, just now, in a browser (a token or the proxy
	// can't); a signed-in browser confirms for any token, since a token
	// outlives the session that made it.
	caller, _ := principalFrom(r.Context())
	privileged := slices.ContainsFunc(scopes, func(s string) bool { return slices.Contains(privilegedScopes, s) })
	if (privileged || (caller.Kind == "session" && a.mode() != ModeProxy)) && !a.requireReauth(w, r, opTokenCreate, "") {
		return
	}
	t, secret, err := a.Store.CreateScopedToken(name, scopes, expires)
	if err != nil {
		slog.ErrorContext(r.Context(), "v1: create token", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "creating the token failed")
		return
	}
	p, _ := principalFrom(r.Context())
	ev := store.EventData{TokenID: t.ID, Scopes: scopes, Actor: p.Kind, Addr: a.clientAddr(r).String()}
	a.signInEvent(r.Context(), store.EventTokenCreated, ev)
	body := v1CreatedTokenBody{v1TokenBody: v1Token(t), Secret: secret}
	w.Header().Set("Location", "/v1/tokens/"+body.ID)
	writeV1(w, http.StatusCreated, body)
}
