package api

import (
	"log/slog"
	"net/http"
	"net/url"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/translate"
)

// GET /v1/instance: what this server is and can do.

type v1InstanceBody struct {
	Version          string           `json:"version"`
	Auth             string           `json:"auth"` // "oidc" or "proxy"
	Features         v1Features       `json:"features"`
	UserLanguages    []string         `json:"user_languages"`
	TranslateTargets []string         `json:"translate_targets"`
	Limits           v1Limits         `json:"limits"`
	Idempotency      v1IdemPolicy     `json:"idempotency"`
	Scopes           []string         `json:"scopes"`
	Destinations     []string         `json:"destinations"`
	Pagination       v1PaginationInfo `json:"pagination"`
}

type v1Features struct {
	Translate  bool `json:"translate"`
	Video      bool `json:"video"`
	PublicFeed bool `json:"public_feed"`
	RelaySync  bool `json:"relay_sync"`
}

type v1Limits struct {
	MaxJSONBodyBytes     int64 `json:"max_json_body_bytes"`
	MaxUploadBytes       int64 `json:"max_upload_bytes"`
	MaxVideoUploadBytes  int64 `json:"max_video_upload_bytes"`
	MaxImagesPerRequest  int   `json:"max_images_per_request"`
	MaxIdempotencyKeyLen int   `json:"max_idempotency_key_length"`
}

type v1IdemPolicy struct {
	TTLSecs int64 `json:"ttl_secs"`
}

type v1PaginationInfo struct {
	DefaultLimit int `json:"default_limit"`
	MaxLimit     int `json:"max_limit"`
}

func (a *API) v1Instance(w http.ResponseWriter, r *http.Request) {
	langs := a.UserLanguages
	if langs == nil {
		langs = []string{}
	}
	targets := []string{}
	if a.Translator != nil {
		targets = translate.Intersect(langs)
	}
	authMode := "proxy"
	if a.authEnabled() {
		authMode = "oidc"
	}
	version := a.Version
	if version == "" {
		version = "dev"
	}
	writeV1(w, http.StatusOK, v1InstanceBody{
		Version: version,
		Auth:    authMode,
		Features: v1Features{Translate: a.Translator != nil, Video: a.VideoJobs != nil, PublicFeed: a.PublicFeedToken != "",
			RelaySync: a.relaySyncAvailable(r.Context())},
		UserLanguages:    langs,
		TranslateTargets: targets,
		Limits: v1Limits{
			MaxJSONBodyBytes: maxV1Body, MaxUploadBytes: maxUploadRequestBytes, MaxVideoUploadBytes: maxVideoUploadBytes,
			MaxImagesPerRequest: maxImagesPerRequest, MaxIdempotencyKeyLen: maxIdempotencyKey,
		},
		Idempotency:  v1IdemPolicy{TTLSecs: int64(store.IdempotencyTTL.Seconds())},
		Scopes:       v1Scopes,
		Destinations: destination.Types(),
		Pagination:   v1PaginationInfo{DefaultLimit: defaultPageLimit, MaxLimit: maxPageLimit},
	})
}

// GET /v1/destinations: the registered destination types.

func (a *API) v1Destinations(w http.ResponseWriter, _ *http.Request) {
	out := []destination.Manifest{}
	for _, t := range destination.Types() {
		if d, ok := destination.Lookup(t); ok {
			out = append(out, d.Manifest())
		}
	}
	writeV1(w, http.StatusOK, newList(out, nil))
}

// GET /v1/accounts, GET /v1/accounts/{id}: the owner's accounts.

type v1AccountBody struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Label       string `json:"label"`
	Handle      string `json:"handle,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Avatar      string `json:"avatar,omitempty"`
	Source      string `json:"source"`
	Enabled     bool   `json:"enabled"`
	// Capabilities is absent when no driver for the type is registered
	// (an account kept for its history after its driver went away).
	Capabilities *destination.Capabilities `json:"capabilities,omitempty"`
	CreatedAt    string                    `json:"created_at"`
	UpdatedAt    string                    `json:"updated_at"`
}

// v1AccountViews renders accounts, each with its profile as its network
// last reported it (handle, name, avatar). Profiles never wait on the
// network: a stale one is fetched in the background for the next call.
func (a *API) v1AccountViews(r *http.Request, accts []store.Account) []v1AccountBody {
	out := make([]v1AccountBody, 0, len(accts))
	for _, ac := range accts {
		v := v1AccountBody{
			ID: ac.ID, Type: ac.Type, Label: ac.Label, Handle: ac.Handle, Source: ac.Source, Enabled: ac.Enabled,
			CreatedAt: rfc3339(ac.CreatedAt), UpdatedAt: rfc3339(ac.UpdatedAt),
		}
		if _, ok := destination.Lookup(ac.Type); ok {
			c := a.caps(r.Context(), ac.ID, ac.Type)
			v.Capabilities = &c
		}
		if a.Accounts != nil {
			if p, ok := a.Accounts.Profile(ac.ID); ok {
				if p.Handle != "" {
					v.Handle = p.Handle
				}
				v.DisplayName, v.Avatar = p.DisplayName, absoluteHTTPURL(p.AvatarURL)
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
	if a.Store == nil {
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "no database")
		return
	}
	p, _ := principalFrom(r.Context())
	accts, err := a.Store.Accounts(r.Context(), p.OwnerID)
	if err != nil {
		slog.ErrorContext(r.Context(), "v1: list accounts", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "listing accounts failed")
		return
	}
	page, next := pageOf(accts, func(ac store.Account) cursor { return cursor{ac.CreatedAt, ac.ID} }, false, after, limit)
	writeV1(w, http.StatusOK, newList(a.v1AccountViews(r, page), next))
}

func (a *API) v1Account(w http.ResponseWriter, r *http.Request) {
	if a.Store == nil {
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "no database")
		return
	}
	p, _ := principalFrom(r.Context())
	accts, err := a.Store.Accounts(r.Context(), p.OwnerID)
	if err != nil {
		slog.ErrorContext(r.Context(), "v1: get account", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "reading the account failed")
		return
	}
	id := r.PathValue("id")
	for _, ac := range accts {
		if ac.ID == id {
			writeV1(w, http.StatusOK, a.v1AccountViews(r, []store.Account{ac})[0])
			return
		}
	}
	problem(w, r, http.StatusNotFound, codeNotFound, "no account "+id)
}

// GET /v1/me: who is calling, and what they may do.

type v1MeBody struct {
	Kind   string       `json:"kind"`
	Scopes []string     `json:"scopes"`
	User   *v1UserBody  `json:"user,omitempty"`
	Token  *v1TokenBody `json:"token,omitempty"`
}

type v1UserBody struct {
	ID      string `json:"id"`
	Subject string `json:"subject"`
	Email   string `json:"email,omitempty"`
	Name    string `json:"name,omitempty"`
}

func (a *API) v1Me(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	body := v1MeBody{Kind: p.Kind, Scopes: p.Scopes}
	if p.Scopes == nil {
		body.Scopes = v1Scopes
	}
	if p.User != nil {
		body.User = &v1UserBody{ID: p.User.ID, Subject: p.User.Subject, Email: p.User.Email, Name: p.User.Name}
	}
	if p.Token != nil {
		t := v1Token(*p.Token)
		body.Token = &t
	}
	writeV1(w, http.StatusOK, body)
}
