package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/geofox/publisher/internal/accounts"
	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/store"
)

// /v1/accounts: the owner's accounts. GET lists and reads them, each with
// its status, its profile as its network last reported it, what it can
// do, its configuration and which credentials are set (never their
// values); PATCH changes the owner's settings on one, and the
// configuration and credentials of one added in the app (accounts:write).
// Adding, deleting and the actions are in v1_accounts_write.go.

const maxAccountLabel = 64

type v1AccountBody struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Label string `json:"label"`
	// The profile: the handle, name, avatar and page the network reports.
	Handle      string `json:"handle,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Avatar      string `json:"avatar,omitempty"`
	ProfileURL  string `json:"profile_url,omitempty"`
	// RemoteID is the account's stable id on its network (a DID, an
	// account URL, a user id, a public key); absent until first read.
	RemoteID string `json:"remote_id,omitempty"`
	Source   string `json:"source"`
	Enabled  bool   `json:"enabled"`
	// Status says whether the account can be used now; StatusReason why
	// not, in the operator's terms.
	Status       string `json:"status"`
	StatusReason string `json:"status_reason,omitempty"`
	// Order is the account's place in the app's order (networks in a
	// fixed order, then oldest first): tabs and target rows follow it.
	Order           int  `json:"order"`
	DefaultSelected bool `json:"default_selected"`
	PublicFeed      bool `json:"public_feed"`
	// Defaults are what a post to the account gets when it says nothing
	// of them.
	Defaults v1AccountDefaults `json:"defaults"`
	// Capabilities is absent when no driver for the type is registered
	// (an account kept for its history after its driver went away).
	Capabilities *destination.Capabilities `json:"capabilities,omitempty"`
	// AuthMethod is how it connects (GET /v1/destinations lists them).
	AuthMethod string `json:"auth_method,omitempty"`
	// Config is its configuration: the type's fields and the method's
	// fields that aren't secret.
	Config map[string]any `json:"config"`
	// Credentials says which credentials are set and when; never what
	// they are.
	Credentials map[string]accounts.CredentialInfo `json:"credentials"`
	// LockedFields are the fields environment variables set (an account
	// from the environment): change them there, or adopt it.
	LockedFields []string `json:"locked_fields,omitempty"`
	// Health is the last authenticated check.
	Health    *v1AccountHealth `json:"health,omitempty"`
	CreatedAt string           `json:"created_at"`
	UpdatedAt string           `json:"updated_at"`
	// DeletedAt is set on a deleted account (?include=deleted).
	DeletedAt string `json:"deleted_at,omitempty"`
}

// v1AccountHealth is an account's last authenticated check.
type v1AccountHealth struct {
	OK        bool   `json:"ok"`
	Reachable bool   `json:"reachable"`
	AuthOK    *bool  `json:"auth_ok,omitempty"`
	Message   string `json:"message,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	// ReconnectBy: the network's grant ends then (a Meta grant).
	ReconnectBy string `json:"reconnect_by,omitempty"`
	CheckedAt   string `json:"checked_at"`
}

// accountETag is an account's version: its updated_at, which moves only
// when the owner changes something.
func accountETag(ac store.Account) string {
	return `"` + strconv.FormatInt(ac.UpdatedAt.UnixNano(), 10) + `"`
}

// v1AccountViews renders accounts, each with its profile as its network
// last reported it (handle, name, avatar). Profiles never wait on the
// network: a stale one is fetched in the background for the next call.
func (a *API) v1AccountViews(r *http.Request, accts []store.Account) []v1AccountBody {
	out := make([]v1AccountBody, 0, len(accts))
	for _, ac := range accts {
		v := v1AccountBody{
			ID: ac.ID, Type: ac.Type, Label: ac.Label, Handle: ac.Handle, RemoteID: ac.RemoteID, Source: ac.Source,
			Enabled: ac.Enabled, DefaultSelected: ac.DefaultSelected, PublicFeed: ac.PublicFeed, Defaults: accountDefaults(ac),
			Config: map[string]any{}, Credentials: map[string]accounts.CredentialInfo{},
			CreatedAt: rfc3339(ac.CreatedAt), UpdatedAt: rfc3339(ac.UpdatedAt),
		}
		if !ac.DeletedAt.IsZero() {
			v.DeletedAt = rfc3339(ac.DeletedAt)
		}
		if _, ok := destination.Lookup(ac.Type); ok && ac.DeletedAt.IsZero() {
			c := a.caps(r.Context(), ac.ID, ac.Type)
			v.Capabilities = &c
		}
		if aw, ok := a.Accounts.(AccountWriter); ok && ac.DeletedAt.IsZero() {
			d, err := aw.Details(r.Context(), ac)
			if err != nil {
				slog.ErrorContext(r.Context(), "v1: reading an account's settings failed", "account", ac.ID, "err", err)
			}
			v.AuthMethod, v.LockedFields = d.Method, d.LockedFields
			if d.Config != nil {
				v.Config = d.Config
			}
			if d.Credentials != nil {
				v.Credentials = d.Credentials
			}
		}
		v.Status, v.StatusReason, v.Order = string(accounts.NotConfigured), "this server hasn't loaded it; restart Publisher", -1
		if !ac.DeletedAt.IsZero() {
			v.Status, v.StatusReason = string(accounts.Deleted), "you deleted it"
		}
		if a.Accounts != nil {
			if en, ok := a.Accounts.Get(ac.ID); ok {
				v.Status, v.StatusReason, v.Order = string(en.Status), en.Reason, en.Order
				if en.Health != nil {
					h := en.Health
					v.Health = &v1AccountHealth{OK: h.OK, Reachable: h.Reachable, AuthOK: h.AuthOK, Message: h.Message,
						CheckedAt: rfc3339(en.CheckedAt)}
					if h.ExpiresAt != nil {
						v.Health.ExpiresAt = rfc3339(*h.ExpiresAt)
					}
					if h.ReconnectBy != nil {
						v.Health.ReconnectBy = rfc3339(*h.ReconnectBy)
					}
				}
			}
			if p, ok := a.Accounts.Profile(ac.ID); ok {
				if p.Handle != "" {
					v.Handle = p.Handle
				}
				if p.RemoteID != "" {
					v.RemoteID = p.RemoteID
				}
				v.DisplayName, v.Avatar, v.ProfileURL = p.DisplayName, absoluteHTTPURL(p.AvatarURL), absoluteHTTPURL(p.ProfileURL)
			}
		}
		out = append(out, v)
	}
	return out
}

// absoluteHTTPURL returns s when it is an absolute http(s) URL, else "":
// profiles carry whatever the network serves (a relative Mastodon default,
// a Nostr picture field holding anything).
func absoluteHTTPURL(s string) string {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return ""
	}
	return s
}

func (a *API) v1Accounts(w http.ResponseWriter, r *http.Request) {
	limit, after, ok := pageParams(w, r)
	if !ok {
		return
	}
	list := a.Store.Accounts
	switch inc := r.URL.Query().Get("include"); inc {
	case "":
	case "deleted":
		list = a.Store.AccountsWithDeleted
	default:
		problem(w, r, http.StatusBadRequest, codeInvalidParameter, "include takes deleted",
			fieldError{Parameter: "include", Code: "invalid", Detail: "deleted, or leave it out"})
		return
	}
	if a.Store == nil {
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "no database")
		return
	}
	p, _ := principalFrom(r.Context())
	accts, err := list(r.Context(), p.OwnerID)
	if err != nil {
		slog.ErrorContext(r.Context(), "v1: list accounts", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "listing accounts failed")
		return
	}
	page, next := pageOf(accts, func(ac store.Account) cursor { return cursor{ac.CreatedAt, ac.ID} }, false, after, limit)
	writeV1(w, http.StatusOK, newList(a.v1AccountViews(r, page), next))
}

// v1OwnAccount reads the caller's account named in the path, or answers
// 404 (another owner's account is not found either) or 500.
func (a *API) v1OwnAccount(w http.ResponseWriter, r *http.Request) (store.Account, bool) {
	if a.Store == nil {
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "no database")
		return store.Account{}, false
	}
	p, _ := principalFrom(r.Context())
	id := r.PathValue("id")
	ac, err := a.Store.Account(r.Context(), p.OwnerID, id)
	switch {
	case errors.Is(err, store.ErrAccountNotFound):
		problem(w, r, http.StatusNotFound, codeNotFound, "no account "+id)
		return store.Account{}, false
	case err != nil:
		slog.ErrorContext(r.Context(), "v1: get account", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "reading the account failed")
		return store.Account{}, false
	}
	return ac, true
}

func (a *API) v1Account(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.v1OwnAccount(w, r)
	if !ok {
		return
	}
	a.writeAccountV1(w, r, ac)
}

func (a *API) writeAccountV1(w http.ResponseWriter, r *http.Request, ac store.Account) {
	w.Header().Set("ETag", accountETag(ac))
	writeV1(w, http.StatusOK, a.v1AccountViews(r, []store.Account{ac})[0])
}

// v1AccountPatchIn is PATCH /v1/accounts/{id}'s body. Each field is raw so
// that null (which can't apply to any of them) is refused, not read as
// "leave it".
type v1AccountPatchIn struct {
	Label           json.RawMessage `json:"label"`
	Enabled         json.RawMessage `json:"enabled"`
	DefaultSelected json.RawMessage `json:"default_selected"`
	PublicFeed      json.RawMessage `json:"public_feed"`
	// Defaults replaces the account's defaults ({} clears them).
	Defaults json.RawMessage `json:"defaults"`
	// Method, Config and Credentials reconnect an account added in the
	// app (checkCredentialPatch).
	Method      json.RawMessage `json:"method"`
	Config      json.RawMessage `json:"config"`
	Credentials json.RawMessage `json:"credentials"`
}

// checkAccountPatch turns a patch into the store's change, or says what is
// wrong with it.
func checkAccountPatch(in v1AccountPatchIn) (store.AccountMeta, []fieldError) {
	var m store.AccountMeta
	var errs []fieldError
	add := func(ptr, code, detail string) {
		errs = append(errs, fieldError{Pointer: ptr, Code: code, Detail: detail})
	}
	if in.Label != nil {
		var s string
		if err := json.Unmarshal(in.Label, &s); err != nil || isJSONNull(in.Label) {
			add("/label", "invalid", "a string of 1 to 64 characters")
		} else if s, fe := checkLabel(s, false); fe != nil {
			errs = append(errs, *fe)
		} else {
			m.Label = &s
		}
	}
	for _, f := range []struct {
		ptr string
		raw json.RawMessage
		dst **bool
	}{
		{"/enabled", in.Enabled, &m.Enabled},
		{"/default_selected", in.DefaultSelected, &m.DefaultSelected},
		{"/public_feed", in.PublicFeed, &m.PublicFeed},
	} {
		if f.raw == nil {
			continue
		}
		var b bool
		if err := json.Unmarshal(f.raw, &b); err != nil || isJSONNull(f.raw) {
			add(f.ptr, "invalid", "true or false")
			continue
		}
		*f.dst = &b
	}
	return m, errs
}

func isJSONNull(m json.RawMessage) bool { return strings.TrimSpace(string(m)) == "null" }

// v1PatchAccount changes the owner's settings on an account: its label,
// whether it is on, whether new posts select it, and whether its posts go
// to the public feed; and, for an account added in the app, its
// configuration and credentials (opened and identified first: spec
// §5.4). If-Match (the ETag of a read) refuses a change made on a stale
// read; without it the change applies to the account as it is.
func (a *API) v1PatchAccount(w http.ResponseWriter, r *http.Request) {
	var in v1AccountPatchIn
	if !decodeJSON(w, r, &in) {
		return
	}
	want, ok := ifMatch(r)
	if !ok {
		problem(w, r, http.StatusBadRequest, codeInvalidParameter, `If-Match must be an ETag this server sent or *`,
			fieldError{Parameter: "If-Match", Code: "invalid", Detail: "the account's ETag, unchanged"})
		return
	}
	cur, ok := a.v1OwnAccount(w, r)
	if !ok {
		return
	}
	if want > 0 && want != cur.UpdatedAt.UnixNano() {
		w.Header().Set("ETag", accountETag(cur))
		problem(w, r, http.StatusPreconditionFailed, codePreconditionFailed,
			"the account changed since you read it; read it again, then decide")
		return
	}
	m, errs := checkAccountPatch(in)
	creds, cerrs := checkCredentialPatch(in)
	errs = append(errs, cerrs...)
	if in.Defaults != nil && cur.DeletedAt.IsZero() {
		d, derrs := a.checkAccountDefaults(r.Context(), cur, in.Defaults)
		if errs = append(errs, derrs...); len(derrs) == 0 && d != cur.Defaults {
			m.Defaults = &d
		}
	}
	if len(errs) > 0 {
		invalid(w, r, errs)
		return
	}
	if !cur.DeletedAt.IsZero() {
		problem(w, r, http.StatusNotFound, codeNotFound, "account "+cur.ID+" was deleted")
		return
	}
	// The connection first: when it is refused, nothing changes.
	if creds.set {
		next, ok := a.changeAccountCredentials(w, r, cur, creds)
		if !ok {
			return
		}
		cur = next
		if m == (store.AccountMeta{}) {
			a.writeAccountV1(w, r, cur)
			return
		}
	}
	p, _ := principalFrom(r.Context())
	// The change applies to the account as just read, so one landing in
	// between is never silently undone.
	m.IfUpdatedAt, m.Actor = cur.UpdatedAt, p.Kind
	if p.Token != nil {
		m.TokenID = p.Token.ID
	}
	next, err := a.Store.UpdateAccountMeta(r.Context(), p.OwnerID, cur.ID, m)
	switch {
	case errors.Is(err, store.ErrAccountConflict):
		w.Header().Set("ETag", accountETag(next))
		problem(w, r, http.StatusPreconditionFailed, codePreconditionFailed, "the account changed while saving; read it again")
		return
	case errors.Is(err, store.ErrAccountNotFound):
		problem(w, r, http.StatusNotFound, codeNotFound, "no account "+cur.ID)
		return
	case err != nil:
		slog.ErrorContext(r.Context(), "v1: update account", "account", cur.ID, "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "saving the account failed")
		return
	}
	// The registry holds the row the engine and composer go by.
	if a.Accounts != nil && !next.UpdatedAt.Equal(cur.UpdatedAt) {
		if err := a.Accounts.Update(next.ID, next); err != nil {
			slog.ErrorContext(r.Context(), "v1: account changed but the registry wasn't told", "account", next.ID, "err", err)
		}
	}
	a.writeAccountV1(w, r, next)
}
