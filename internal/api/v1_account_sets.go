package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/geofox/publisher/internal/store"
)

// /v1/account-sets: named selections of the owner's accounts the composer
// can start from ("Work": Mastodon and Bluesky), at most one of them the
// default (spec §7.4). Reads need accounts:read, writes accounts:write;
// each set has an ETag, and If-Match refuses a write on a stale read.

const (
	maxAccountSetName     = 64
	maxAccountSetAccounts = maxPostTargets
	maxAccountSets        = 50
)

type v1AccountSetBody struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Accounts are the set's accounts that still exist, in the set's order.
	Accounts  []string `json:"accounts"`
	IsDefault bool     `json:"is_default"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
}

func accountSetETag(s store.AccountSet) string {
	return `"` + strconv.FormatInt(s.UpdatedAt.UnixNano(), 10) + `"`
}

// accountSetView is a set as answered: its accounts that still exist (one
// deleted since is left out).
func accountSetView(s store.AccountSet, live map[string]bool) v1AccountSetBody {
	out := v1AccountSetBody{ID: s.ID, Name: s.Name, Accounts: []string{}, IsDefault: s.IsDefault,
		CreatedAt: rfc3339(s.CreatedAt), UpdatedAt: rfc3339(s.UpdatedAt)}
	for _, id := range s.Accounts {
		if live[id] {
			out.Accounts = append(out.Accounts, id)
		}
	}
	return out
}

// liveAccounts is the owner's accounts that exist (not deleted), by id.
func (a *API) liveAccounts(r *http.Request, owner string) (map[string]bool, error) {
	accts, err := a.Store.Accounts(r.Context(), owner)
	if err != nil {
		return nil, err
	}
	live := make(map[string]bool, len(accts))
	for _, ac := range accts {
		live[ac.ID] = true
	}
	return live, nil
}

func (a *API) v1AccountSets(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	sets, err := a.Store.AccountSets(r.Context(), p.OwnerID)
	var live map[string]bool
	if err == nil {
		live, err = a.liveAccounts(r, p.OwnerID)
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "v1: list account sets", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "listing the account sets failed")
		return
	}
	out := make([]v1AccountSetBody, 0, len(sets))
	for _, s := range sets {
		out = append(out, accountSetView(s, live))
	}
	writeV1(w, http.StatusOK, newList(out, nil))
}

// v1AccountSetIn is a set's body: all three to create one; any of them to
// change one. Raw, so that null is refused rather than read as "leave it".
type v1AccountSetIn struct {
	Name      json.RawMessage `json:"name"`
	Accounts  json.RawMessage `json:"accounts"`
	IsDefault json.RawMessage `json:"is_default"`
}

// checkAccountSet turns a body into the store's change, or says what is
// wrong with it. create: name and accounts are required.
func checkAccountSet(in v1AccountSetIn, live map[string]bool, create bool) (store.AccountSetChange, []fieldError) {
	var c store.AccountSetChange
	var errs []fieldError
	add := func(ptr, code, detail string) {
		errs = append(errs, fieldError{Pointer: ptr, Code: code, Detail: detail})
	}
	switch {
	case in.Name == nil && create:
		add("/name", "required", "the set's name")
	case in.Name != nil:
		var s string
		if err := json.Unmarshal(in.Name, &s); err != nil || isJSONNull(in.Name) {
			add("/name", "invalid", "a string of 1 to 64 characters")
			break
		}
		s = strings.TrimSpace(s)
		switch {
		case s == "":
			add("/name", "required", "a name can't be empty")
		case utf8.RuneCountInString(s) > maxAccountSetName:
			add("/name", "too_long", "at most 64 characters")
		case nameProblem(s) != "":
			add("/name", "invalid", nameProblem(s))
		default:
			c.Name = &s
		}
	}
	switch {
	case in.Accounts == nil && create:
		add("/accounts", "required", "the accounts in the set, by id")
	case in.Accounts != nil:
		var ids []string
		if err := json.Unmarshal(in.Accounts, &ids); err != nil || isJSONNull(in.Accounts) {
			add("/accounts", "invalid", "a list of account ids")
			break
		}
		switch {
		case len(ids) == 0:
			add("/accounts", "required", "name at least one account")
		case len(ids) > maxAccountSetAccounts:
			add("/accounts", "too_many", "at most 20 accounts")
		}
		seen := map[string]bool{}
		for i, id := range ids {
			ptr := "/accounts/" + strconv.Itoa(i)
			switch {
			case !live[id]:
				add(ptr, "unknown_account", "no account "+id+"; list them with GET /v1/accounts")
			case seen[id]:
				add(ptr, "duplicate", "this account is already in the set")
			}
			seen[id] = true
		}
		c.Accounts = &ids
	}
	if in.IsDefault != nil {
		var b bool
		if err := json.Unmarshal(in.IsDefault, &b); err != nil || isJSONNull(in.IsDefault) {
			add("/is_default", "invalid", "true or false")
		} else {
			c.IsDefault = &b
		}
	}
	return c, errs
}

// nameTaken: another of the owner's sets has this name, whatever its
// case ("Work" and "work" would be told apart by nothing but case).
func nameTaken(sets []store.AccountSet, name, self string) bool {
	for _, s := range sets {
		if s.ID != self && strings.EqualFold(s.Name, name) {
			return true
		}
	}
	return false
}

var errNameTaken = fieldError{Pointer: "/name", Code: "duplicate", Detail: "you have a set of that name already"}

func (a *API) v1CreateAccountSet(w http.ResponseWriter, r *http.Request) {
	var in v1AccountSetIn
	if !decodeJSON(w, r, &in) {
		return
	}
	p, _ := principalFrom(r.Context())
	live, err := a.liveAccounts(r, p.OwnerID)
	var sets []store.AccountSet
	if err == nil {
		sets, err = a.Store.AccountSets(r.Context(), p.OwnerID)
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "v1: create account set", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "saving the account set failed")
		return
	}
	c, errs := checkAccountSet(in, live, true)
	if c.Name != nil && nameTaken(sets, *c.Name, "") {
		errs = append(errs, errNameTaken)
	}
	if len(errs) > 0 {
		invalid(w, r, errs)
		return
	}
	if len(sets) >= maxAccountSets {
		problem(w, r, http.StatusConflict, codeInvalidState, "at most 50 account sets; delete one first")
		return
	}
	s, err := a.Store.CreateAccountSet(r.Context(), p.OwnerID, c)
	if !a.accountSetWriteOK(w, r, err) {
		return
	}
	w.Header().Set("Location", "/v1/account-sets/"+s.ID)
	w.Header().Set("ETag", accountSetETag(s))
	writeV1(w, http.StatusCreated, accountSetView(s, live))
}

// accountSetWriteOK answers a set write's error; false when it has.
func (a *API) accountSetWriteOK(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, store.ErrAccountSetName):
		invalid(w, r, []fieldError{errNameTaken})
	case errors.Is(err, store.ErrAccountSetNotFound):
		problem(w, r, http.StatusNotFound, codeNotFound, "no account set "+r.PathValue("id"))
	default:
		slog.ErrorContext(r.Context(), "v1: save account set", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "saving the account set failed")
	}
	return false
}

// v1OwnAccountSet reads the set named in the path, or answers.
func (a *API) v1OwnAccountSet(w http.ResponseWriter, r *http.Request) (store.AccountSet, map[string]bool, bool) {
	p, _ := principalFrom(r.Context())
	s, err := a.Store.AccountSet(r.Context(), p.OwnerID, r.PathValue("id"))
	var live map[string]bool
	if err == nil {
		live, err = a.liveAccounts(r, p.OwnerID)
	}
	if !a.accountSetWriteOK(w, r, err) {
		return store.AccountSet{}, nil, false
	}
	return s, live, true
}

func (a *API) v1AccountSet(w http.ResponseWriter, r *http.Request) {
	s, live, ok := a.v1OwnAccountSet(w, r)
	if !ok {
		return
	}
	w.Header().Set("ETag", accountSetETag(s))
	writeV1(w, http.StatusOK, accountSetView(s, live))
}

// accountSetIfMatch checks If-Match against the set as read; false when
// it has answered (a bad header, or a stale read: 412 with the ETag).
func accountSetIfMatch(w http.ResponseWriter, r *http.Request, s store.AccountSet) bool {
	want, ok := ifMatch(r)
	if !ok {
		problem(w, r, http.StatusBadRequest, codeInvalidParameter, `If-Match must be an ETag this server sent or *`,
			fieldError{Parameter: "If-Match", Code: "invalid", Detail: "the set's ETag, unchanged"})
		return false
	}
	if want > 0 && want != s.UpdatedAt.UnixNano() {
		w.Header().Set("ETag", accountSetETag(s))
		problem(w, r, http.StatusPreconditionFailed, codePreconditionFailed, "the set changed since you read it; read it again, then decide")
		return false
	}
	return true
}

func (a *API) v1PatchAccountSet(w http.ResponseWriter, r *http.Request) {
	var in v1AccountSetIn
	if !decodeJSON(w, r, &in) {
		return
	}
	cur, live, ok := a.v1OwnAccountSet(w, r)
	if !ok || !accountSetIfMatch(w, r, cur) {
		return
	}
	c, errs := checkAccountSet(in, live, false)
	p, _ := principalFrom(r.Context())
	if c.Name != nil {
		sets, err := a.Store.AccountSets(r.Context(), p.OwnerID)
		if err != nil {
			slog.ErrorContext(r.Context(), "v1: change account set", "err", err)
			problem(w, r, http.StatusInternalServerError, codeInternal, "saving the account set failed")
			return
		}
		if nameTaken(sets, *c.Name, cur.ID) {
			errs = append(errs, errNameTaken)
		}
	}
	if len(errs) > 0 {
		invalid(w, r, errs)
		return
	}
	if c.Name == nil && c.Accounts == nil && c.IsDefault == nil {
		w.Header().Set("ETag", accountSetETag(cur))
		writeV1(w, http.StatusOK, accountSetView(cur, live))
		return
	}
	c.IfUpdatedAt = cur.UpdatedAt
	next, err := a.Store.UpdateAccountSet(r.Context(), p.OwnerID, cur.ID, c)
	if errors.Is(err, store.ErrAccountSetConflict) {
		if latest, rerr := a.Store.AccountSet(r.Context(), p.OwnerID, cur.ID); rerr == nil {
			w.Header().Set("ETag", accountSetETag(latest))
		}
		problem(w, r, http.StatusPreconditionFailed, codePreconditionFailed, "the set changed while saving; read it again")
		return
	}
	if !a.accountSetWriteOK(w, r, err) {
		return
	}
	w.Header().Set("ETag", accountSetETag(next))
	writeV1(w, http.StatusOK, accountSetView(next, live))
}

func (a *API) v1DeleteAccountSet(w http.ResponseWriter, r *http.Request) {
	cur, _, ok := a.v1OwnAccountSet(w, r)
	if !ok || !accountSetIfMatch(w, r, cur) {
		return
	}
	p, _ := principalFrom(r.Context())
	if !a.accountSetWriteOK(w, r, a.Store.DeleteAccountSet(r.Context(), p.OwnerID, cur.ID)) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
