package api

import (
	"net/http"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/translate"
)

// GET /v1/instance: what this server is and can do.

type v1InstanceBody struct {
	Version string `json:"version"`
	Auth    string `json:"auth"` // "oidc", "local" or "proxy"
	// SignInRecommended: the app has no sign-in of its own (proxy mode);
	// the web app offers to set it up.
	SignInRecommended bool             `json:"sign_in_recommended,omitempty"`
	Features          v1Features       `json:"features"`
	UserLanguages     []string         `json:"user_languages"`
	TranslateTargets  []string         `json:"translate_targets"`
	Limits            v1Limits         `json:"limits"`
	Idempotency       v1IdemPolicy     `json:"idempotency"`
	Scopes            []string         `json:"scopes"`
	Destinations      []string         `json:"destinations"`
	Pagination        v1PaginationInfo `json:"pagination"`
}

type v1Features struct {
	// Media: a media store is configured, so posts can carry images and
	// videos.
	Media      bool `json:"media"`
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
	authMode := a.mode()
	version := a.Version
	if version == "" {
		version = "dev"
	}
	writeV1(w, http.StatusOK, v1InstanceBody{
		Version:           version,
		Auth:              authMode,
		SignInRecommended: authMode == ModeProxy,
		Features: v1Features{Media: a.media != nil, Translate: a.Translator != nil, Video: a.media != nil && a.VideoJobs != nil,
			PublicFeed: a.PublicFeedToken != "",
			RelaySync:  a.relaySyncAvailable(r.Context())},
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
