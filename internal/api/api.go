package api

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"fiatjaf.com/nostr"

	"github.com/geofox/publisher/internal/auth"
	"github.com/geofox/publisher/internal/dispatch"
	"github.com/geofox/publisher/internal/feed"
	"github.com/geofox/publisher/internal/httpx"
	"github.com/geofox/publisher/internal/identity"
	"github.com/geofox/publisher/internal/media"
	"github.com/geofox/publisher/internal/metrics"
	pubnostr "github.com/geofox/publisher/internal/nostr"
	"github.com/geofox/publisher/internal/progress"
	"github.com/geofox/publisher/internal/relaysync"
	"github.com/geofox/publisher/internal/resolve"
	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/thread"
	"github.com/geofox/publisher/internal/transcode"
	"github.com/geofox/publisher/internal/translate"
	"github.com/geofox/publisher/internal/unfurl"
	"github.com/geofox/publisher/internal/verify"
	"github.com/geofox/publisher/internal/videojob"
	"github.com/geofox/publisher/internal/web"
)

// Hard caps on incoming request bodies.
//
// Both endpoints wrap r.Body with http.MaxBytesReader before any reader
// touches it. MaxBytesReader is the only reliable body-size guard in Go's
// stdlib — ParseMultipartForm's `maxMemory` argument is just the in-memory
// spill threshold, not a request-body limit, so without this wrapper an
// arbitrarily large upload would fully resolve (memory + /tmp + ReadAll-back).
//
// Sized for the n8n cross-posting workflow:
//   - /publish receives small JSON (text + imeta + pow). 256 KB is generous.
//   - /upload-media receives form-data with a single image/video. 64 MB
//     covers reasonable user-generated content; tune up for video.
const (
	maxPublishRequestBytes int64 = 256 << 10 // 256 KB
	maxUploadRequestBytes  int64 = 64 << 20  // 64 MB
	maxPostRequestBytes    int64 = 256 << 20 // 256 MB (spec + up to 40 phone-sized images)
	maxVerifyRequestBytes  int64 = 512 << 10 // 512 KB (pasted event JSON or a URL)
	maxThreadPreviewBytes  int64 = 256 << 10 // 256 KB (draft text for split preview)
)

// maxImagesPerRequest is the whole-request image cap across a thread. Per-post
// platform caps (Bluesky 10 / Mastodon 4 / Threads 10 / Nostr ∞) are enforced
// separately by thread.MaxImagesFor; this only bounds the total a single
// publish/preview request may carry.
const maxImagesPerRequest = 40

// maxVideoUploadBytes caps the async video endpoint (spec: 1 GB, streamed to
// disk — never RAM). The +1 MiB slack covers multipart framing.
const maxVideoUploadBytes = 1<<30 + 1<<20

// Dispatcher is implemented by dispatch.Dispatcher; extracted as an interface
// so the api package has no concrete dependency on the dispatcher and tests
// can substitute a fake.
type Dispatcher interface {
	Post(ctx context.Context, spec dispatch.PostSpec) *store.Post
	PostWithID(ctx context.Context, id string, spec dispatch.PostSpec) *store.Post
	Retry(ctx context.Context, id string, platforms []string) (*store.Post, error)
	RetryRelay(ctx context.Context, id, relay string) (*store.Post, error)
	Schedule(ctx context.Context, spec dispatch.PostSpec, at time.Time) (*store.Post, error)
	Interact(ctx context.Context, spec dispatch.InteractSpec) *store.Post
	InteractWithID(ctx context.Context, id string, spec dispatch.InteractSpec) *store.Post
}

// Syncer is implemented by *relaysync.Sync.
type Syncer interface {
	Scan(ctx context.Context, targets []relaysync.Target) []relaysync.RelayDiff
	Apply(ctx context.Context, targets []relaysync.Target, direction string) []relaysync.ApplyResult
}

// Verifier is implemented by *verify.Service; extracted so the api package can
// be tested with a stub and has no hard dependency on the concrete verify service.
type Verifier interface {
	Verify(ctx context.Context, in verify.Input) verify.Verdict
}

// Resolver is implemented by *resolve.Service; resolves a pasted URL/identifier.
type Resolver interface {
	Resolve(ctx context.Context, input string) (*resolve.SourceRef, error)
}

// Translator proxies a text-translation request to an upstream provider (DeepL).
// target is ISO 639-1 (lowercase). Returns the translated text and the provider's
// detected source language (lowercase). nil when DEEPL_API_KEY is not configured;
// /api/translate then returns 503 and the UI hides the action.
type Translator interface {
	Translate(ctx context.Context, text, target string) (translated, detectedSource string, err error)
}

// UnfurlService is the preview's view of the unfurl service.
type UnfurlService interface {
	Unfurl(ctx context.Context, url string) (*unfurl.Card, error)
}

// API holds the dependencies for the HTTP handlers.
type API struct {
	np        *pubnostr.Publisher
	media     *media.Pipeline
	Dispatch  Dispatcher   // set by cmd/publisher (exported so tests can inject fakes)
	Store     *store.Store // set by cmd/publisher; used by history endpoints
	Sync      Syncer       // set by cmd/publisher; used by relay-sync endpoints
	HomeRelay string       // set by cmd/publisher; the home relay URL
	Verify    Verifier     // set by cmd/publisher; verifies pasted events / post URLs
	Resolve   Resolver     // set by cmd/publisher; resolves pasted post URLs/ids

	// UserLanguages is the operator's spoken-languages list (ISO 639-1 codes);
	// exposed via GET /api/config so the frontend can default the Bluesky and
	// Mastodon language fields and offer a dropdown when there's more than one.
	UserLanguages []string

	// Translator powers POST /api/translate. nil → feature disabled (handler
	// returns 503 and /api/config emits an empty translate_targets array).
	Translator Translator

	// PublicFeedToken gates GET /api/public/feed. Empty → the endpoint is
	// disabled (returns 404). Set → callers must send Authorization: Bearer <it>.
	PublicFeedToken string

	// Identity aggregates the operator's own per-platform profile (handle, name,
	// avatar) for the composer. nil → GET /api/identity returns empty accounts.
	Identity *identity.Service

	// Progress is the live in-flight progress registry (SSE). nil disables the
	// live modal path (handlers fall back to synchronous dispatch).
	Progress *progress.Registry

	// fetchMedia downloads already-uploaded image bytes by URL when a publish
	// carries Blossom references instead of fresh uploads (a restored/loaded
	// draft). nil → fetchSourceMedia (https-only, SSRF-guarded). Overridable in
	// tests so the reattach path can run without a live Blossom server.
	fetchMedia func(ctx context.Context, rawURL string) ([]byte, string, error)

	// Auth holds the OIDC Relying Party. nil → auth disabled (gates are
	// pass-through). Set by cmd/publisher when OIDC_ISSUER is configured.
	Auth *auth.Authenticator
	// Allowlist gates which verified identities may use the app. Non-nil iff Auth is non-nil.
	Allowlist *auth.Allowlist
	// SessionTTL is how long a new session lives. Zero → 168h default applied by handlers.
	SessionTTL time.Duration
	// EndSession toggles provider single-logout on /auth/logout.
	EndSession bool
	// AppBaseURL is the public origin (scheme://host) used to build post-logout redirects.
	AppBaseURL string

	// Unfurl builds bluesky link cards for the thread preview; may be nil
	// (no cards in previews). Satisfied by *unfurl.Service.
	Unfurl UnfurlService

	// VideoJobs runs the async probe→transcode→upload pipeline for video
	// attachments. nil when ffmpeg is not present on the host (the endpoints
	// return 503 with a clear message).
	VideoJobs *videojob.Runner

	// VideoWorkdir is the dedicated directory for video upload temp files and
	// transcode outputs. Falls back to videojob.DefaultWorkdir() when empty.
	// Must be separate from bare os.TempDir() because the runner's startup
	// sweep removes everything in the workdir.
	VideoWorkdir string
}

// New creates a new API with the given publisher and media pipeline.
func New(np *pubnostr.Publisher, mp *media.Pipeline) *API {
	return &API{np: np, media: mp}
}

// Routes returns the HTTP handler with all API routes registered, wrapped in
// the security-header and CSRF middleware.
func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", a.handleHealthz)
	mux.Handle("GET /metrics", metrics.Handler())
	mux.HandleFunc("/publish", a.handlePublish)
	mux.HandleFunc("/upload-media", a.handleUploadMedia)
	mux.HandleFunc("/api/post", a.handleAPIPost)
	mux.HandleFunc("GET /api/posts", a.handleListPosts)
	mux.HandleFunc("GET /api/posts/attention/count", a.handleAttentionCount)
	mux.HandleFunc("GET /api/posts/{id}", a.handleGetPost)
	mux.HandleFunc("GET /api/posts/{id}/progress", a.handleProgress)
	mux.HandleFunc("POST /api/posts/{id}/retry", a.handleRetry)
	mux.HandleFunc("POST /api/posts/{id}/relay-retry", a.handleRelayRetry)
	mux.HandleFunc("POST /api/posts/{id}/cancel", a.handleCancelScheduled)
	mux.HandleFunc("POST /api/posts/{id}/reschedule", a.handleReschedule)
	mux.HandleFunc("POST /api/posts/{id}/hide", a.handleHidePost)
	mux.HandleFunc("GET /api/sync/relays", a.handleSyncRelaysList)
	mux.HandleFunc("POST /api/sync/relays", a.handleSyncRelayAdd)
	mux.HandleFunc("DELETE /api/sync/relays", a.handleSyncRelayRemove)
	mux.HandleFunc("GET /api/sync/targets", a.handleSyncTargets)
	mux.HandleFunc("POST /api/sync/scan", a.handleSyncScan)
	mux.HandleFunc("POST /api/sync/apply", a.handleSyncApply)
	mux.HandleFunc("POST /api/verify", a.handleVerify)
	mux.HandleFunc("POST /api/thread-preview", a.handleThreadPreview)
	mux.HandleFunc("POST /api/resolve", a.handleResolve)
	mux.HandleFunc("POST /api/interact", a.handleInteract)
	mux.HandleFunc("GET /api/config", a.handleConfig)
	mux.HandleFunc("GET /api/identity", a.handleIdentity)
	mux.HandleFunc("GET /api/public/feed", a.handlePublicFeed)
	mux.HandleFunc("POST /api/translate", a.handleTranslate)
	mux.HandleFunc("POST /api/media/compress", a.handleCompressMedia)
	mux.HandleFunc("POST /api/media/video", a.handleVideoUpload)
	mux.HandleFunc("GET /api/media/video/{id}", a.handleVideoJob)
	mux.HandleFunc("GET /api/drafts", a.handleListDrafts)
	mux.HandleFunc("POST /api/drafts", a.handleCreateDraft)
	mux.HandleFunc("GET /api/drafts/{id}", a.handleGetDraft)
	mux.HandleFunc("PUT /api/drafts/{id}", a.handleUpdateDraft)
	mux.HandleFunc("DELETE /api/drafts/{id}", a.handleDeleteDraft)
	mux.HandleFunc("POST /api/drafts/{id}/translate", a.handleTranslateDraft)
	// Token management + identity + auth routes (only when OIDC is enabled).
	if a.authEnabled() {
		mux.HandleFunc("GET /api/tokens", a.handleListTokens)
		mux.HandleFunc("POST /api/tokens", a.handleCreateToken)
		mux.HandleFunc("DELETE /api/tokens/{id}", a.handleRevokeToken)
		mux.HandleFunc("GET /api/me", a.handleMe)
		mux.HandleFunc("GET /auth/login", a.handleAuthLogin)
		mux.HandleFunc("GET /auth/callback", a.handleAuthCallback)
		mux.HandleFunc("POST /auth/logout", a.handleAuthLogout)
	}
	mux.Handle("/", web.Handler())
	return withSecurityHeaders(a.withCSRFGuard(a.withGates(mux)))
}

// contentSecurityPolicy locks the SPA to same-origin code. Every script, style,
// and font is a same-origin asset and no markup uses inline scripts/styles, so
// the policy needs no 'unsafe-inline' — which is what makes it an effective
// stored/reflected-XSS backstop (e.g. a hostile platform remote_url). Images
// additionally allow https/blob/data for Blossom thumbnails and local previews.
const contentSecurityPolicy = "default-src 'self'; " +
	"img-src 'self' https: data: blob:; media-src 'self' https:; style-src 'self'; script-src 'self'; " +
	"font-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; " +
	"frame-ancestors 'none'; object-src 'none'"

// withSecurityHeaders adds defense-in-depth response headers to every reply.
func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// withCSRFGuard rejects a state-changing request whose Origin header is present
// but points at a different host — the signature of a cross-site attack riding
// the owner's authenticated (Authelia) session. Same-origin SPA calls and
// server-to-server callers like n8n (no Origin header) pass through, so CSRF is
// blocked without an app-level token scheme.
//
// The Origin is accepted when it matches the request Host OR the configured
// public origin (AppBaseURL). Behind a reverse proxy r.Host is whatever the
// proxy forwards upstream — often an internal host that never matches the
// browser's public Origin — so comparing against the server-configured public
// origin is what keeps the SPA's own same-origin requests (sign-out included)
// from being wrongly blocked. AppBaseURL comes from env, not the client, so it
// is not spoofable.
func (a *API) withCSRFGuard(next http.Handler) http.Handler {
	allowedHost := ""
	if a.AppBaseURL != "" {
		if u, err := url.Parse(a.AppBaseURL); err == nil {
			allowedHost = hostOnly(u.Host)
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			// safe methods don't mutate state
		default:
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				okHost := err == nil && (sameHost(u.Host, r.Host) ||
					(allowedHost != "" && hostOnly(u.Host) == allowedHost))
				if !okHost {
					httpx.WriteError(w, http.StatusForbidden, "cross-origin request blocked")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// sameHost compares two host[:port] strings by hostname, ignoring the port (the
// request Host often carries no port behind the TLS-terminating proxy while the
// Origin may, or vice-versa). Empty hosts never match.
func sameHost(a, b string) bool {
	ha, hb := hostOnly(a), hostOnly(b)
	return ha != "" && ha == hb
}

func hostOnly(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return h
}

// withGates applies session/token gates per path. When auth is disabled it is a
// pass-through, preserving the pre-OIDC behavior (and keeping the existing test
// suite green).
func (a *API) withGates(next http.Handler) http.Handler {
	if !a.authEnabled() {
		return next
	}
	session := a.requireSession(next)
	token := a.requireAPIToken(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Clean the path before gate selection: net/http does not normalize
		// r.URL.Path before the handler runs, so a raw "/auth/../api/tokens"
		// would otherwise satisfy the "/auth/" prefix and dodge the session gate.
		p := path.Clean(r.URL.Path)
		switch {
		case p == "/healthz" || p == "/metrics" ||
			strings.HasPrefix(p, "/auth/") ||
			p == "/api/public/feed":
			next.ServeHTTP(w, r) // always-open / separately-gated
		case p == "/publish" || p == "/upload-media":
			token.ServeHTTP(w, r) // machine endpoints
		default:
			session.ServeHTTP(w, r) // SPA + browser /api/*
		}
	})
}

// ─── /healthz ────────────────────────────────────────────────────────────

func (a *API) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("content-type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// ─── GET /api/config ─────────────────────────────────────────────────────
//
// Operator preferences the frontend needs on boot. Currently just the spoken-
// languages list (drives the Bluesky/Mastodon language defaults + dropdown).
// Always returns an array (possibly empty) so the JS can treat it uniformly.

func (a *API) handleConfig(w http.ResponseWriter, _ *http.Request) {
	langs := a.UserLanguages
	if langs == nil {
		langs = []string{}
	}
	// translate_targets = user_languages ∩ DeepL-supported, preserving the
	// operator's ordering. Empty when no Translator is configured (the UI
	// hides the Translate button in that case).
	targets := []string{}
	if a.Translator != nil {
		targets = translate.Intersect(langs)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"user_languages":    langs,
		"translate_targets": targets,
	})
}

// ─── GET /api/identity ───────────────────────────────────────────────────
//
// The operator's own cross-platform profile (handle, display name, avatar) so
// the composer can show real account data. Best-effort + cached; an unconfigured
// service returns {"accounts":{}} so the UI keeps its placeholders.

func (a *API) handleIdentity(w http.ResponseWriter, r *http.Request) {
	if a.Identity == nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"accounts": map[string]any{}})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, a.Identity.Get(r.Context()))
}

// ─── POST /api/translate ─────────────────────────────────────────────────
//
// Body: {"text":"...","target_lang":"fr"} where target_lang is ISO 639-1.
// Returns {"text":"...","detected_source_language":"en"}. 503 when no
// Translator is configured (i.e. DEEPL_API_KEY unset).

type translateReq struct {
	Text       string `json:"text"`
	TargetLang string `json:"target_lang"`
}

type translateResp struct {
	Text                   string `json:"text"`
	DetectedSourceLanguage string `json:"detected_source_language"`
}

func (a *API) handleTranslate(w http.ResponseWriter, r *http.Request) {
	if a.Translator == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "translation is not configured (DEEPL_API_KEY)")
		return
	}
	var req translateReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		httpx.WriteError(w, http.StatusBadRequest, "text is required")
		return
	}
	if !translate.IsSupported(req.TargetLang) {
		httpx.WriteError(w, http.StatusBadRequest, "unsupported target_lang: "+req.TargetLang)
		return
	}
	text, src, err := a.Translator.Translate(r.Context(), req.Text, req.TargetLang)
	if err != nil {
		httpx.WriteError(w, http.StatusBadGateway, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, translateResp{Text: text, DetectedSourceLanguage: src})
}

// ─── /publish ────────────────────────────────────────────────────────────

type publishRequest struct {
	Text  string    `json:"text"`
	Kind  int       `json:"kind"`
	Imeta nostr.Tag `json:"imeta,omitempty"`
	POW   *int      `json:"pow,omitempty"`
}

type publishResponse struct {
	EventID string                 `json:"event_id"`
	Kind    int                    `json:"kind"`
	POW     int                    `json:"pow_target"`
	MinedMS int64                  `json:"mined_ms"`
	Relays  []pubnostr.RelayResult `json:"relays"`
}

func (a *API) handlePublish(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpx.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPublishRequestBytes)
	var req publishRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			httpx.WriteError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("request body exceeds %d bytes", maxPublishRequestBytes))
			return
		}
		httpx.WriteError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}

	var imetas []nostr.Tag
	if len(req.Imeta) > 0 {
		imetas = []nostr.Tag{req.Imeta}
	}
	res, err := a.np.Publish(r.Context(), pubnostr.PublishInput{Text: req.Text, Kind: req.Kind, Imetas: imetas, POW: req.POW})
	if err != nil {
		if errors.Is(err, pubnostr.ErrInvalidInput) {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
		} else {
			httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	succeeded := 0
	for _, rr := range res.Relays {
		if rr.OK {
			succeeded++
		}
	}
	slog.Info("publish",
		"event_id", res.EventID,
		"kind", res.Kind,
		"pow_target", res.POW,
		"mined_ms", res.MinedMS,
		"relays_succeeded", succeeded,
		"relays_failed", len(res.Relays)-succeeded,
	)

	status := http.StatusOK
	if succeeded == 0 {
		status = http.StatusBadGateway
	}
	httpx.WriteJSON(w, status, publishResponse{
		EventID: res.EventID,
		Kind:    res.Kind,
		POW:     res.POW,
		MinedMS: res.MinedMS,
		Relays:  res.Relays,
	})
}

// ─── /upload-media ───────────────────────────────────────────────────────

type uploadResponse struct {
	URL      string    `json:"url"`
	SHA256   string    `json:"sha256"`
	Size     int64     `json:"size"`
	Mime     string    `json:"mime"`
	Dim      string    `json:"dim,omitempty"`
	Blurhash string    `json:"blurhash,omitempty"`
	Imeta    nostr.Tag `json:"imeta"`
}

func (a *API) handleUploadMedia(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpx.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// Fast-fail well-formed clients on Content-Length. MaxBytesReader alone
	// also catches this case, but its typed error doesn't always bubble up
	// cleanly through the multipart parser (which can fail mid-spill with a
	// tempfile-not-found error instead). The pre-check produces a clean 413
	// before any parsing starts.
	if r.ContentLength > maxUploadRequestBytes {
		httpx.WriteError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("upload exceeds %d bytes (%d MB) — Content-Length: %d",
				maxUploadRequestBytes, maxUploadRequestBytes>>20, r.ContentLength))
		return
	}
	// Hard-cap the full request body. ParseMultipartForm's argument is only
	// the in-memory spill threshold (not a total-bytes guard), so without
	// MaxBytesReader a chunked-encoded upload that hides its real size
	// would silently spill to /tmp and then get ReadAll'd back into RAM.
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadRequestBytes)
	// Spill anything over 8 MB to disk so a single large concurrent upload
	// doesn't pin 64 MB of RAM in the parser before we even reach FormFile.
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			httpx.WriteError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("upload exceeds %d bytes (%d MB)",
					maxUploadRequestBytes, maxUploadRequestBytes>>20))
			return
		}
		httpx.WriteError(w, http.StatusBadRequest, "parse multipart: "+err.Error())
		return
	}
	f, fh, err := r.FormFile("file")
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "missing 'file' field: "+err.Error())
		return
	}
	defer f.Close()
	body, err := io.ReadAll(f)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "read body: "+err.Error())
		return
	}

	mime := fh.Header.Get("Content-Type")
	res, err := a.media.Process(r.Context(), body, mime)
	if err != nil {
		httpx.WriteError(w, http.StatusBadGateway, err.Error())
		return
	}

	slog.Info("upload_media",
		"url", res.URL,
		"sha256", res.SHA256,
		"size", res.Size,
		"mime", res.Mime,
		"dim", res.Dim,
	)
	httpx.WriteJSON(w, http.StatusOK, uploadResponse{
		URL:      res.URL,
		SHA256:   res.SHA256,
		Size:     res.Size,
		Mime:     res.Mime,
		Dim:      res.Dim,
		Blurhash: res.Blurhash,
		Imeta:    res.Imeta,
	})
}

// ─── POST /api/media/compress ────────────────────────────────────────────
//
// On-demand composer compression: multipart {file, preset} in, re-encoded
// image bytes out. Stateless — nothing is stored; the client swaps its
// in-memory File for the response, so the compressed version is what later
// rides /api/post and becomes the canonical Blossom object. Also serves the
// HEIC attach-time auto-conversion (preset "convert").
func (a *API) handleCompressMedia(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength > maxUploadRequestBytes {
		httpx.WriteError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("upload exceeds %d bytes (%d MB) — Content-Length: %d",
				maxUploadRequestBytes, maxUploadRequestBytes>>20, r.ContentLength))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadRequestBytes)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			httpx.WriteError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("upload exceeds %d bytes (%d MB)",
					maxUploadRequestBytes, maxUploadRequestBytes>>20))
			return
		}
		httpx.WriteError(w, http.StatusBadRequest, "parse multipart: "+err.Error())
		return
	}
	params, ok := transcode.PresetParams(r.FormValue("preset"))
	if !ok {
		httpx.WriteError(w, http.StatusBadRequest, "unknown preset")
		return
	}
	f, fh, err := r.FormFile("file")
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "missing 'file' field: "+err.Error())
		return
	}
	defer f.Close()
	body, err := io.ReadAll(f)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "read body: "+err.Error())
		return
	}
	select {
	case compressSem <- struct{}{}:
		defer func() { <-compressSem }()
	case <-r.Context().Done():
		return // client gave up while queued (abort on preset switch)
	}
	if r.Context().Err() != nil {
		return
	}
	res, err := transcode.Image(body, fh.Header.Get("Content-Type"), params)
	if err != nil {
		// Unconvertible input, not a server fault: 422
		// so the composer flashes the reason and keeps the original file.
		httpx.WriteError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if !res.Changed {
		// Presets force JPEG, so decodable input always re-encodes; an
		// unchanged result means transcode's passthrough fired — either the
		// image was not decodable, or it exceeded the 100 MP pixel cap
		// (pixel presets carry no MaxBytes, so no transcode error is returned).
		// Never hand the original back as if it were compressed.
		httpx.WriteError(w, http.StatusUnprocessableEntity, "image could not be compressed (not decodable, or over the 100 MP cap)")
		return
	}
	slog.Info("compress_media", "preset", r.FormValue("preset"),
		"in_bytes", len(body), "out_bytes", len(res.Bytes), "mime", res.Mime)
	w.Header().Set("Content-Type", res.Mime)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	w.Write(res.Bytes)
}

// ─── POST /api/media/video ──────────────────────────────────────────────
//
// Async attach-time video ingest: the multipart body STREAMS to a temp file
// in the dedicated video workdir (1 GB cap, zero RAM buffering via
// MultipartReader), then a videojob runs probe → ffmpeg normalize → Blossom,
// polled by the composer at GET /api/media/video/{id}. Preset rides the query
// string so parts are never buffered while hunting for a field.
func (a *API) handleVideoUpload(w http.ResponseWriter, r *http.Request) {
	if a.VideoJobs == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "video pipeline not configured (ffmpeg missing)")
		return
	}
	// The server's global Read/WriteTimeouts (60s/120s) are sized for normal
	// API calls; a 1 GB upload needs minutes. Extend the deadlines for this
	// request only (≈4.7 Mbit/s floor — still a slow-loris bound, and the
	// endpoint is session-gated). ErrNotSupported (httptest) is ignored.
	rc := http.NewResponseController(w)
	d := time.Now().Add(30 * time.Minute)
	_ = rc.SetReadDeadline(d)
	_ = rc.SetWriteDeadline(d.Add(time.Minute))

	if a.VideoJobs.Full() {
		httpx.WriteError(w, http.StatusTooManyRequests, "transcode queue is full — wait for the current video to finish")
		return
	}
	if r.ContentLength > maxVideoUploadBytes {
		httpx.WriteError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("upload exceeds %d bytes (%d MB)", maxVideoUploadBytes, maxVideoUploadBytes>>20))
		return
	}
	preset := r.URL.Query().Get("preset")
	if preset == "" {
		preset = "1080p"
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxVideoUploadBytes)
	mr, err := r.MultipartReader()
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "multipart: "+err.Error())
		return
	}
	var tmpPath string
	for {
		part, perr := mr.NextPart()
		if perr == io.EOF {
			break
		}
		if perr != nil {
			var mbe *http.MaxBytesError
			if errors.As(perr, &mbe) {
				httpx.WriteError(w, http.StatusRequestEntityTooLarge, "upload exceeds cap")
			} else {
				httpx.WriteError(w, http.StatusBadRequest, "multipart: "+perr.Error())
			}
			return
		}
		if part.FormName() != "file" {
			io.Copy(io.Discard, part)
			continue
		}
		dir := a.videoWorkdir()
		tmp, terr := os.CreateTemp(dir, "upload-*.video")
		if terr != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "tmp: "+terr.Error())
			return
		}
		_, cerr := io.Copy(tmp, part)
		tmp.Close()
		if cerr != nil {
			os.Remove(tmp.Name())
			var pe *fs.PathError
			if errors.As(cerr, &pe) && pe.Op == "write" {
				// Server-side staging failure (disk full etc.) — never blame
				// the client, never leak the filesystem path.
				slog.Error("video upload staging failed", "err", cerr)
				httpx.WriteError(w, http.StatusInsufficientStorage, "server storage error while staging upload")
				return
			}
			var mbe *http.MaxBytesError
			if errors.As(cerr, &mbe) {
				httpx.WriteError(w, http.StatusRequestEntityTooLarge, "upload exceeds cap")
			} else {
				httpx.WriteError(w, http.StatusBadRequest, "read: "+cerr.Error())
			}
			return
		}
		tmpPath = tmp.Name()
		break
	}
	if tmpPath == "" {
		httpx.WriteError(w, http.StatusBadRequest, "missing 'file' part")
		return
	}
	id, err := a.VideoJobs.Submit(r.Context(), tmpPath, preset)
	if err != nil {
		// ErrBusy is the only Submit error; the input was already consumed.
		httpx.WriteError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	slog.Info("video_job_submitted", "job", id, "preset", preset)
	httpx.WriteJSON(w, http.StatusAccepted, map[string]string{"job_id": id})
}

// videoWorkdir is the DEDICATED dir for uploads and transcode outputs — never
// bare os.TempDir(): the runner's startup sweep deletes everything in its
// workdir, and dev /tmp is RAM-backed tmpfs. Configurable via VIDEO_WORKDIR.
func (a *API) videoWorkdir() string {
	d := a.VideoWorkdir
	if d == "" {
		d = videojob.DefaultWorkdir()
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		slog.Error("video workdir unavailable", "path", d, "err", err)
	}
	return d
}

func (a *API) handleVideoJob(w http.ResponseWriter, r *http.Request) {
	if a.VideoJobs == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "video pipeline not configured")
		return
	}
	j, ok := a.VideoJobs.Get(r.PathValue("id"))
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, "unknown job (restarted? re-attach the video)")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, j)
}

// ─── /api/post ───────────────────────────────────────────────────────────

// imageSpec is the per-image metadata carried in a "spec" field's "images"
// array. Each entry is one of two kinds, distinguished by which fields are set:
//   - a freshly-added image: Ref is set ("img_N") and its bytes arrive as the
//     next multipart "image" file part;
//   - an already-uploaded image (a restored/loaded draft): BlossomURL (+ the
//     other media fields) is set and there is NO multipart file — assembleImages
//     re-fetches the bytes from Blossom so the media isn't dropped on publish.
//
// Named (not anonymous) so it can be shared between the spec structs and the
// assembleImages helper without struct-tag identity pitfalls.
type imageSpec struct {
	Alt          string `json:"alt"`
	Ref          string `json:"ref"`
	BlossomURL   string `json:"blossom_url"`
	SHA256       string `json:"sha256"`
	Mime         string `json:"mime"`
	Dim          string `json:"dim"`
	Blurhash     string `json:"blurhash"`
	SizeBytes    int64  `json:"size_bytes"`
	Ordinal      int    `json:"ordinal"`
	DurationSecs int64  `json:"duration_secs"`
	PosterURL    string `json:"poster_url"`
}

// postSpecJSON is the JSON object expected in the "spec" multipart field.
type postSpecJSON struct {
	MasterText   string                        `json:"master_text"`
	Platforms    []string                      `json:"platforms"`
	DelaySeconds int                           `json:"delay_seconds"`
	Overrides    map[string]dispatch.Overrides `json:"overrides"`
	Images       []imageSpec                   `json:"images"`
	ScheduledAt  string                        `json:"scheduled_at"`
	Number       bool                          `json:"number"`
	DraftID      string                        `json:"draft_id,omitempty"` // NEW: consume on success
	ImgParts     []int                         `json:"img_parts"`
}

func (a *API) handleAPIPost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpx.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPostRequestBytes)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			httpx.WriteError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("request exceeds %d bytes", maxPostRequestBytes))
			return
		}
		httpx.WriteError(w, http.StatusBadRequest, "parse multipart: "+err.Error())
		return
	}
	specRaw := r.FormValue("spec")
	if specRaw == "" {
		httpx.WriteError(w, http.StatusBadRequest, "spec field is required")
		return
	}
	var sj postSpecJSON
	if err := json.Unmarshal([]byte(specRaw), &sj); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid spec json: "+err.Error())
		return
	}
	if len(sj.Platforms) == 0 {
		httpx.WriteError(w, http.StatusBadRequest, "platforms is required")
		return
	}

	imgs, mediaRecs, err := a.assembleImages(r, sj.Images)
	if err != nil {
		var me *mediaError
		if errors.As(err, &me) {
			httpx.WriteError(w, http.StatusBadGateway, me.Error())
		} else {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
		}
		return
	}

	spec := dispatch.PostSpec{
		MasterText: sj.MasterText, Platforms: sj.Platforms, DelaySeconds: sj.DelaySeconds,
		Source: "web", Overrides: sj.Overrides, Images: imgs, MediaRecords: mediaRecs,
		Number: sj.Number, ImgParts: sj.ImgParts,
	}
	if sj.ScheduledAt != "" {
		at, err := time.Parse(time.RFC3339, sj.ScheduledAt)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "scheduled_at must be RFC3339")
			return
		}
		if !at.After(time.Now()) {
			httpx.WriteError(w, http.StatusBadRequest, "scheduled_at must be in the future")
			return
		}
		srec, err := a.Dispatch.Schedule(r.Context(), spec, at)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if sj.DraftID != "" && a.Store != nil {
			if err := a.Store.DeleteDraft(sj.DraftID); err != nil {
				// log but don't fail the publish — the post was created
				slog.Warn("drafts: consume DeleteDraft failed", "draft_id", sj.DraftID, "err", err)
			}
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"post_id": srec.ID, "status": srec.Status, "scheduled_at": srec.ScheduledAt,
		})
		return
	}
	if a.Progress != nil {
		id := dispatch.NewID()
		hub := a.Progress.Create(id, spec.Platforms, "")
		draftID := sj.DraftID
		go func() {
			defer func() {
				if rec := recover(); rec != nil {
					slog.Error("post dispatch goroutine panic", "post_id", id, "panic", rec)
					a.Progress.Finish(id, "failed", 5*time.Minute)
				}
			}()
			ctx := context.WithoutCancel(r.Context())
			ctx = progress.WithSink(ctx, hub)
			rec := a.Dispatch.PostWithID(ctx, id, spec)
			a.Progress.Finish(id, rec.Status, 5*time.Minute)
			if draftID != "" && a.Store != nil {
				if err := a.Store.DeleteDraft(draftID); err != nil {
					slog.Warn("drafts: consume DeleteDraft failed", "draft_id", draftID, "err", err)
				}
			}
		}()
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"post_id": id, "status": "running"})
		return
	}
	// Fallback (no registry): synchronous as before.
	rec := a.Dispatch.Post(r.Context(), spec)

	type targetOut struct {
		Platform  string             `json:"platform"`
		Status    string             `json:"status"`
		Error     string             `json:"error,omitempty"`
		RemoteURL string             `json:"remote_url,omitempty"`
		LatencyMS int                `json:"latency_ms"`
		Relays    []store.RelayState `json:"relays,omitempty"`
		Segments  []store.Segment    `json:"segments,omitempty"`
	}
	out := struct {
		PostID  string      `json:"post_id"`
		Status  string      `json:"status"`
		Targets []targetOut `json:"targets"`
	}{PostID: rec.ID, Status: rec.Status}
	for _, tg := range rec.Targets {
		out.Targets = append(out.Targets, targetOut{
			Platform:  tg.Platform,
			Status:    tg.Status,
			Error:     errOfTarget(tg),
			RemoteURL: tg.RemoteURL,
			LatencyMS: tg.LatencyMS,
			Relays:    tg.Relays,
			Segments:  tg.Segments,
		})
	}
	if sj.DraftID != "" && a.Store != nil {
		if err := a.Store.DeleteDraft(sj.DraftID); err != nil {
			// log but don't fail the publish — the post was created
			slog.Warn("drafts: consume DeleteDraft failed", "draft_id", sj.DraftID, "err", err)
		}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// ─── /api/posts ──────────────────────────────────────────────────────────

// atoiOr parses s as an int; returns def on empty string or parse error.
func atoiOr(s string, def int) int {
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}

// ─── GET /api/public/feed ────────────────────────────────────────────────
//
// Read-only homepage feed: latest public master posts as custom JSON. Disabled
// (404) unless PUBLIC_FEED_TOKEN is set; when set, requires a matching bearer
// token. GET, so it passes the CSRF guard and is meant for a server-side
// (build-time) consumer that keeps the token secret.
func (a *API) handlePublicFeed(w http.ResponseWriter, r *http.Request) {
	if a.PublicFeedToken == "" {
		httpx.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) ||
		subtle.ConstantTimeCompare([]byte(h[len(prefix):]), []byte(a.PublicFeedToken)) != 1 {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	limit := atoiOr(r.URL.Query().Get("limit"), 20)
	posts, err := a.Store.PublicFeed(limit)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, feed.Build(posts, limit))
}

func (a *API) handleListPosts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.PostFilter{
		Status: q.Get("status"),
		Query:  q.Get("q"),
		Limit:  atoiOr(q.Get("limit"), 50),
		Offset: atoiOr(q.Get("offset"), 0),
	}
	posts, err := a.Store.ListPostsFiltered(f)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, posts)
}

func (a *API) handleGetPost(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, err := a.Store.GetPost(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "not found")
		} else {
			slog.Error("get_post", "id", id, "err", err)
			httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

// ─── GET /api/posts/{id}/progress ───────────────────────────────────────────
//
// handleProgress streams live progress for a post as Server-Sent Events. If the
// post is in-flight (a hub exists) it streams snapshots until terminal; if it
// already finished it replays a single snapshot built from the store; unknown
// ids 404.
func (a *API) handleProgress(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpx.WriteError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	send := func(s progress.Snapshot) {
		b, _ := json.Marshal(s)
		_, _ = w.Write([]byte("data: "))
		_, _ = w.Write(b)
		_, _ = w.Write([]byte("\n\n"))
		flusher.Flush()
	}

	if a.Progress != nil {
		if hub, ok := a.Progress.Get(id); ok {
			cur, ch, cancel := hub.Subscribe()
			defer cancel()
			send(cur)
			ticker := time.NewTicker(15 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-r.Context().Done():
					return
				case <-ticker.C:
					_, _ = w.Write([]byte(": ping\n\n"))
					flusher.Flush()
				case s, open := <-ch:
					if !open {
						return // hub closed: terminal snapshot already delivered
					}
					send(s)
				}
			}
		}
	}

	// Not in-flight: replay from the store if it finished, else 404.
	if a.Store != nil {
		if p, err := a.Store.GetPost(id); err == nil && p != nil {
			_, _ = w.Write([]byte("retry: -1\n\n"))
			flusher.Flush()
			send(progress.FromStorePost(p))
			return
		}
	}
	httpx.WriteError(w, http.StatusNotFound, "post not found")
}

// ─── GET /api/posts/attention/count ─────────────────────────────────────────

func (a *API) handleAttentionCount(w http.ResponseWriter, r *http.Request) {
	n, err := a.Store.AttentionCount()
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]int{"count": n})
}

// ─── POST /api/posts/{id}/retry ─────────────────────────────────────────────

func (a *API) handleRetry(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10) // optional small JSON body
	var body struct {
		Platforms []string `json:"platforms"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body) // body optional; ignore EOF/empty
	}
	post, err := a.Dispatch.Retry(r.Context(), id, body.Platforms)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "not found")
		} else {
			slog.Error("retry", "id", id, "err", err)
			httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	httpx.WriteJSON(w, http.StatusOK, post)
}

// ─── POST /api/posts/{id}/relay-retry ───────────────────────────────────────

func (a *API) handleRelayRetry(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var body struct {
		Platform string `json:"platform"`
		Relay    string `json:"relay"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if body.Platform != "nostr" {
		httpx.WriteError(w, http.StatusBadRequest, "relay retry is nostr-only")
		return
	}
	if body.Relay == "" {
		httpx.WriteError(w, http.StatusBadRequest, "relay is required")
		return
	}
	post, err := a.Dispatch.RetryRelay(r.Context(), id, body.Relay)
	if err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			httpx.WriteError(w, http.StatusNotFound, "not found")
		case errors.Is(err, dispatch.ErrBadRelayRetry):
			httpx.WriteError(w, http.StatusBadRequest, "unknown or non-retryable relay")
		default:
			slog.Error("relay_retry", "id", id, "relay", body.Relay, "err", err)
			httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	httpx.WriteJSON(w, http.StatusOK, post)
}

// ─── POST /api/posts/{id}/cancel ────────────────────────────────────────────

func (a *API) handleCancelScheduled(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := a.Store.CancelScheduled(id); err != nil {
		if errors.Is(err, store.ErrNotPending) {
			httpx.WriteError(w, http.StatusConflict, "post is not pending")
			return
		}
		slog.Error("cancel scheduled failed", "post_id", id, "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"cancelled": id})
}

// ─── POST /api/posts/{id}/hide ──────────────────────────────────────────────

func (a *API) handleHidePost(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := a.Store.HidePost(id); err != nil {
		if errors.Is(err, store.ErrNotHideable) {
			httpx.WriteError(w, http.StatusConflict, "post cannot be hidden (pending or missing)")
			return
		}
		slog.Error("hide post failed", "post_id", id, "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"hidden": id})
}

// ─── POST /api/posts/{id}/reschedule ────────────────────────────────────────

func (a *API) handleReschedule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var b struct {
		ScheduledAt string `json:"scheduled_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid json")
		return
	}
	at, err := time.Parse(time.RFC3339, b.ScheduledAt)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "scheduled_at must be RFC3339")
		return
	}
	if !at.After(time.Now()) {
		httpx.WriteError(w, http.StatusBadRequest, "scheduled_at must be in the future")
		return
	}
	if err := a.Store.RescheduleScheduled(id, at); err != nil {
		if errors.Is(err, store.ErrNotPending) {
			httpx.WriteError(w, http.StatusConflict, "post is not pending")
			return
		}
		slog.Error("reschedule failed", "post_id", id, "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	post, err := a.Store.GetPost(id)
	if err != nil {
		slog.Error("reschedule: reload failed", "post_id", id, "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, post)
}

// errOfTarget returns the error string from the last attempt on a target, or
// empty string if there are no attempts.
func errOfTarget(tg store.Target) string {
	if len(tg.Attempts) > 0 {
		return tg.Attempts[len(tg.Attempts)-1].Error
	}
	return ""
}

// dedupPlatforms returns a deduplicated copy of platforms preserving order.
func dedupPlatforms(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// ─── /api/sync ───────────────────────────────────────────────────────────

func (a *API) syncRelayLists() (nip65, secondary []string) {
	if a.np != nil {
		if w, err := a.np.ResolveWriteRelays(context.Background()); err == nil {
			nip65 = w
		}
	}
	secondary, _ = a.Store.SyncRelays()
	return
}

func (a *API) resolveSyncTargets() []relaysync.Target {
	nip65, secondary := a.syncRelayLists()
	return relaysync.ResolveTargets(nip65, secondary, a.HomeRelay)
}

// filteredSyncTargets returns the resolved targets, optionally narrowed to the
// given relay URLs (trailing-slash-normalized). Empty relays = all targets.
func (a *API) filteredSyncTargets(relays []string) []relaysync.Target {
	targets := a.resolveSyncTargets()
	if len(relays) == 0 {
		return targets
	}
	want := map[string]bool{}
	for _, u := range relays {
		want[strings.TrimRight(u, "/")] = true
	}
	out := targets[:0:0]
	for _, t := range targets {
		if want[strings.TrimRight(t.URL, "/")] {
			out = append(out, t)
		}
	}
	return out
}

// handleSyncTargets returns the resolved target set so the SPA can render the
// relay rows up front and scan/apply them one at a time (live progress).
func (a *API) handleSyncTargets(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"targets": a.resolveSyncTargets()})
}

func (a *API) handleSyncRelaysList(w http.ResponseWriter, r *http.Request) {
	nip65, secondary := a.syncRelayLists()
	httpx.WriteJSON(w, http.StatusOK, map[string][]string{"nip65": nip65, "secondary": secondary})
}

// validSecondaryRelay rejects non-ws(s), overlay, and the home relay.
func (a *API) validSecondaryRelay(u string) bool {
	u = strings.TrimSpace(u)
	if !strings.HasPrefix(u, "ws://") && !strings.HasPrefix(u, "wss://") {
		return false
	}
	if pubnostr.IsOverlayRelay(u) {
		return false
	}
	return strings.TrimRight(u, "/") != strings.TrimRight(a.HomeRelay, "/")
}

func (a *API) handleSyncRelayAdd(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var b struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if !a.validSecondaryRelay(b.URL) {
		httpx.WriteError(w, http.StatusBadRequest, "relay must be ws(s)://, not overlay, not the home relay")
		return
	}
	// Store the normalized URL (no trailing slash) so it matches ResolveTargets'
	// dedup/exclusion — otherwise a trailing-slash relay lists but never syncs.
	if err := a.Store.AddSyncRelay(strings.TrimRight(strings.TrimSpace(b.URL), "/")); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.handleSyncRelaysList(w, r)
}

func (a *API) handleSyncRelayRemove(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var b struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if err := a.Store.RemoveSyncRelay(strings.TrimRight(strings.TrimSpace(b.URL), "/")); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.handleSyncRelaysList(w, r)
}

func (a *API) handleSyncScan(w http.ResponseWriter, r *http.Request) {
	if a.Sync == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "sync not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var b struct {
		Relays []string `json:"relays"`
	}
	_ = json.NewDecoder(r.Body).Decode(&b) // body optional (per-relay live scan sends one)
	out := a.Sync.Scan(r.Context(), a.filteredSyncTargets(b.Relays))
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"relays": out})
}

func (a *API) handleSyncApply(w http.ResponseWriter, r *http.Request) {
	if a.Sync == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "sync not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var b struct {
		Direction string   `json:"direction"`
		Relays    []string `json:"relays"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if b.Direction != "pull" && b.Direction != "push" {
		httpx.WriteError(w, http.StatusBadRequest, "direction must be pull or push")
		return
	}
	out := a.Sync.Apply(r.Context(), a.filteredSyncTargets(b.Relays), b.Direction)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"results": out})
}

// ─── POST /api/verify ────────────────────────────────────────────────────

func (a *API) handleVerify(w http.ResponseWriter, r *http.Request) {
	if a.Verify == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "verification not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxVerifyRequestBytes)
	var req struct {
		Input    string `json:"input"`
		Platform string `json:"platform"`
		Expected string `json:"expected"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			httpx.WriteError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("request body exceeds %d bytes", maxVerifyRequestBytes))
			return
		}
		httpx.WriteError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Input) == "" {
		httpx.WriteError(w, http.StatusBadRequest, "input is required")
		return
	}
	v := a.Verify.Verify(r.Context(), verify.Input{
		Raw: req.Input, Platform: req.Platform, Expected: req.Expected,
	})
	status := http.StatusOK
	if v.Status == verify.StatusError {
		status = http.StatusBadGateway
	}
	httpx.WriteJSON(w, status, v)
}

// ─── POST /api/thread-preview ──────────────────────────────────────────────

func (a *API) handleThreadPreview(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxThreadPreviewBytes)
	var req struct {
		Text        string   `json:"text"`
		Platforms   []string `json:"platforms"`
		Number      bool     `json:"number"`
		Images      int      `json:"images"`
		ImgParts    []int    `json:"img_parts"`
		Interaction bool     `json:"interaction"`
		// Media carries per-image metadata so previews can badge planned
		// platform-fit conversions. Optional: an older cached bundle sends
		// only the count — previews then skip fit notes.
		Media []struct {
			SizeBytes    int64  `json:"size_bytes"`
			Mime         string `json:"mime"`
			Dim          string `json:"dim"`
			DurationSecs int64  `json:"duration_secs"`
		} `json:"media"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			httpx.WriteError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("request body exceeds %d bytes", maxThreadPreviewBytes))
			return
		}
		httpx.WriteError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		httpx.WriteError(w, http.StatusBadRequest, "text is required")
		return
	}
	if req.Images < 0 {
		req.Images = 0
	}
	if req.Images > maxImagesPerRequest {
		req.Images = maxImagesPerRequest
	}
	var metas []transcode.Meta
	for _, m := range req.Media {
		iw, ih := transcode.ParseDim(m.Dim)
		metas = append(metas, transcode.Meta{SizeBytes: m.SizeBytes, Mime: m.Mime, W: iw, H: ih})
	}
	if len(metas) > maxImagesPerRequest {
		metas = metas[:maxImagesPerRequest]
	}
	if len(metas) > 0 && len(metas) > req.Images {
		// media[] is authoritative when it covers more images than the count;
		// when a buggy client sends fewer media entries than images, keep the
		// larger count so the SPLIT PLAN stays right and only badges go missing.
		req.Images = len(metas)
	}
	// Normalize img_parts to exactly req.Images entries (pad missing with 0 =
	// front-load, drop negatives). req.Images is capped at maxImagesPerRequest above,
	// but the metas bump re-assigns it from user input — so build into a
	// constant-capacity slice with an explicitly bounded loop, keeping the
	// allocation size independent of any user-provided value.
	n := req.Images
	if n > maxImagesPerRequest {
		n = maxImagesPerRequest
	}
	imgParts := make([]int, 0, maxImagesPerRequest)
	for i := 0; i < n; i++ {
		v := 0
		if i < len(req.ImgParts) && req.ImgParts[i] > 0 {
			v = req.ImgParts[i]
		}
		imgParts = append(imgParts, v)
	}
	// videoNotes maps video metas through the same VideoGate dispatch uses —
	// ✗ marks a hard per-target failure, ⚠ an advisory. Image metas are
	// handled by PlanMediaFit (which skips videos); ordinals stay aligned
	// because both iterate req.Media by index.
	videoNotes := func(plat string) []dispatch.FitNote {
		var out []dispatch.FitNote
		for i, m := range req.Media {
			if !strings.HasPrefix(m.Mime, "video/") {
				continue
			}
			fail, warns := transcode.VideoGate(plat, transcode.VideoInfo{
				SizeBytes: m.SizeBytes, DurationSecs: m.DurationSecs,
			})
			if fail != "" {
				out = append(out, dispatch.FitNote{Ordinal: i, Note: "✗ " + fail + " — this target will fail"})
			}
			for _, wmsg := range warns {
				out = append(out, dispatch.FitNote{Ordinal: i, Note: "⚠ " + wmsg})
			}
		}
		return out
	}
	type cardJSON struct {
		Segment     int    `json:"segment"`
		URI         string `json:"uri"`
		Title       string `json:"title"`
		Description string `json:"description"`
		ThumbURL    string `json:"thumb_url,omitempty"`
	}
	type preview struct {
		Platform string             `json:"platform"`
		Count    int                `json:"count"`
		Segments []string           `json:"segments"`
		Imgs     [][]int            `json:"imgs"`
		Card     *cardJSON          `json:"card,omitempty"`
		Warnings []string           `json:"warnings,omitempty"`
		FitNotes []dispatch.FitNote `json:"fit_notes,omitempty"`
	}
	out := struct {
		Previews []preview `json:"previews"`
	}{Previews: []preview{}}
	for _, p := range req.Platforms {
		if p == "bluesky" {
			// Same planner as dispatch (PlanBlueskyCard), so the previewed
			// text, counts and card placement are exactly what posting does.
			// The unfurl gets a tight budget: a slow page degrades the
			// preview to "no card" instead of hanging the composer.
			var card *unfurl.Card
			// Interactions never carry cards in v1 (InteractWithID scrubs
			// them), so their previews must not plan one either.
			if a.Unfurl != nil && !req.Interaction {
				if u, _, ok := unfurl.CardURL(req.Text); ok {
					uctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
					if c, err := a.Unfurl.Unfurl(uctx, u); err == nil {
						card = c
					}
					cancel()
				}
			}
			cp := dispatch.PlanBlueskyCard(req.Text, card, imgParts, req.Number)
			pv := preview{Platform: p, Count: len(cp.Segs), Segments: cp.Segs, Imgs: cp.Plan, Warnings: cp.Warnings, FitNotes: append(dispatch.PlanMediaFit(p, metas), videoNotes(p)...)}
			if cp.Card != nil {
				pv.Card = &cardJSON{
					Segment: cp.Card.Segment, URI: cp.Card.URI, Title: cp.Card.Title,
					Description: cp.Card.Description, ThumbURL: cp.Card.ThumbURL,
				}
			}
			out.Previews = append(out.Previews, pv)
			continue
		}
		segs, plan, warns := thread.SplitPlace(req.Text, thread.LimitFor(p), imgParts, thread.MaxImagesFor(p), thread.Opts{Number: req.Number})
		out.Previews = append(out.Previews, preview{
			Platform: p, Count: len(segs), Segments: segs, Imgs: plan, Warnings: warns, FitNotes: append(dispatch.PlanMediaFit(p, metas), videoNotes(p)...),
		})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// ─── POST /api/resolve ───────────────────────────────────────────────────────

func (a *API) handleResolve(w http.ResponseWriter, r *http.Request) {
	if a.Resolve == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "resolve not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxVerifyRequestBytes)
	var req struct {
		Input string `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.Input) == "" {
		httpx.WriteError(w, http.StatusBadRequest, "input is required")
		return
	}
	ref, err := a.Resolve.Resolve(r.Context(), req.Input)
	if err != nil {
		// Resolution problems (unsupported platform, not found, bad input) are
		// client-facing 400s with the reason; never a 500.
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, ref)
}

// ─── POST /api/interact ──────────────────────────────────────────────────────

// compressSem bounds concurrent transcodes: each can transiently allocate
// hundreds of MB (decode + flatten + orientation copies of a ~100MP image),
// and this host has no swap. Two slots keep the composer snappy while making
// pile-ups impossible.
var compressSem = make(chan struct{}, 2)

// sourceMediaClient is SSRF-guarded: source media URLs come from a pasted
// (untrusted) post, so a fetch must never reach internal/loopback addresses.
var sourceMediaClient = verify.NewSafeClient(20 * time.Second)

func (a *API) handleInteract(w http.ResponseWriter, r *http.Request) {
	if a.Dispatch == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "dispatch not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPostRequestBytes)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			httpx.WriteError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("request exceeds %d bytes", maxPostRequestBytes))
			return
		}
		httpx.WriteError(w, http.StatusBadRequest, "parse multipart: "+err.Error())
		return
	}
	var sj struct {
		Action        string               `json:"action"`
		Platform      string               `json:"platform"`
		Ref           dispatch.InteractRef `json:"ref"`
		SourceURL     string               `json:"source_url"`
		SourceAuthor  string               `json:"source_author"`
		SourcePreview struct {
			Author string `json:"author"`
			Text   string `json:"text"`
			Media  []struct {
				URL string `json:"url"`
				Alt string `json:"alt"`
			} `json:"media"`
		} `json:"source_preview"`
		Text      string                        `json:"text"`
		Overrides map[string]dispatch.Overrides `json:"overrides"`
		Fanout    []string                      `json:"fanout"`
		Number    bool                          `json:"number"`
		Force     bool                          `json:"force"`
		Images    []imageSpec                   `json:"images"`
	}
	if err := json.Unmarshal([]byte(r.FormValue("spec")), &sj); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid spec json: "+err.Error())
		return
	}
	switch sj.Action {
	case "reply", "repost", "quote":
	default:
		httpx.WriteError(w, http.StatusBadRequest, "action must be reply, repost, or quote")
		return
	}
	if sj.Platform == "" {
		httpx.WriteError(w, http.StatusBadRequest, "platform is required")
		return
	}

	userImgs, userRecs, err := a.assembleImages(r, sj.Images)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Re-host the original's media so fan-out targets show the embedded media
	// rather than a hotlink. Best-effort: a source URL that won't fetch (or
	// is SSRF-blocked) is skipped, never failing the whole interaction.
	var srcImgs []dispatch.Img
	var srcRecs []store.Media
	for i, m := range sj.SourcePreview.Media {
		body, mime, ferr := a.fetchSourceMedia(r.Context(), m.URL)
		if ferr != nil {
			continue
		}
		if strings.HasPrefix(mime, "video/") {
			continue // skip source video: re-hosting is best-effort, quote proceeds without it
		}
		res, perr := a.media.Process(r.Context(), body, mime)
		if perr != nil {
			continue
		}
		srcImgs = append(srcImgs, dispatch.Img{Bytes: res.Bytes, Mime: res.Mime, Alt: m.Alt, BlossomURL: res.URL,
			SizeBytes: res.Size})
		srcRecs = append(srcRecs, store.Media{
			Ordinal: i, BlossomURL: res.URL, SHA256: res.SHA256, Mime: res.Mime,
			Dim: res.Dim, Blurhash: res.Blurhash, SizeBytes: res.Size, Alt: m.Alt,
		})
	}

	ispec := dispatch.InteractSpec{
		Action: sj.Action, SourcePlatform: sj.Platform, Ref: sj.Ref,
		SourceURL: sj.SourceURL, SourceAuthor: sj.SourceAuthor, Text: sj.Text,
		Overrides: sj.Overrides, Fanout: sj.Fanout, Force: sj.Force, Number: sj.Number,
		Images: userImgs, MediaRecords: userRecs,
		SourcePreview:      dispatch.SourcePreview{Author: sj.SourcePreview.Author, Text: sj.SourcePreview.Text},
		SourceImages:       srcImgs,
		SourceMediaRecords: srcRecs,
	}
	if a.Progress != nil {
		id := dispatch.NewID()
		// native = the reply/quote target platform; fan-out platforms are the rest.
		platforms := append([]string{ispec.SourcePlatform}, ispec.Fanout...)
		hub := a.Progress.Create(id, dedupPlatforms(platforms), ispec.SourcePlatform)
		go func() {
			defer func() {
				if rec := recover(); rec != nil {
					slog.Error("interact dispatch goroutine panic", "post_id", id, "panic", rec)
					a.Progress.Finish(id, "failed", 5*time.Minute)
				}
			}()
			ctx := context.WithoutCancel(r.Context())
			ctx = progress.WithSink(ctx, hub)
			rec := a.Dispatch.InteractWithID(ctx, id, ispec)
			a.Progress.Finish(id, rec.Status, 5*time.Minute)
		}()
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"post_id": id, "status": "running"})
		return
	}
	post := a.Dispatch.Interact(r.Context(), ispec)
	httpx.WriteJSON(w, http.StatusOK, post)
}

// assembleImages reconstructs the ordered image set for a publish/interact
// request from its spec.images, in spec order. Each entry is handled by kind:
//   - fresh upload (no BlossomURL): consume the next multipart "image" file and
//     run its bytes through the media pipeline;
//   - already-uploaded reference (BlossomURL set, no fresh file): re-fetch the
//     bytes from Blossom and re-process them, so platforms that need raw bytes
//     (Bluesky/Mastodon/Threads) still get the media and Nostr gets a fresh
//     imeta. Without this, a restored/loaded draft would publish with no media.
//
// A reference that can't be fetched is skipped (best-effort, matching source-
// media re-hosting) rather than failing the whole post. The returned slices are
// renumbered densely so a skipped reference doesn't leave an ordinal gap.
func (a *API) assembleImages(r *http.Request, specs []imageSpec) ([]dispatch.Img, []store.Media, error) {
	var files []*multipart.FileHeader
	if r.MultipartForm != nil {
		files = r.MultipartForm.File["image"]
	}
	// Every multipart file contributes at most one image; every Blossom-ref
	// spec entry contributes at most one more without consuming a file (the
	// trailing defensive loop processes leftover files). Bound the combined
	// total — but NOT len(specs)+len(files): a fresh-upload spec entry and the
	// file it consumes are the same image.
	blossomRefs := 0
	for _, s := range specs {
		if s.BlossomURL != "" {
			blossomRefs++
		}
	}
	if len(files)+blossomRefs > maxImagesPerRequest || len(specs) > maxImagesPerRequest {
		return nil, nil, fmt.Errorf("max %d images", maxImagesPerRequest)
	}
	fetch := a.fetchMedia
	if fetch == nil {
		fetch = a.fetchSourceMedia
	}

	// processUpload reads one multipart file and runs it through the media
	// pipeline. A pipeline failure is wrapped as *mediaError so callers can map
	// it to 502 (upstream) rather than 400 (client) — open/read failures stay
	// plain (client/server-side body issues).
	processUpload := func(fh *multipart.FileHeader) (media.Result, error) {
		f, err := fh.Open()
		if err != nil {
			return media.Result{}, fmt.Errorf("open image: %w", err)
		}
		body, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			return media.Result{}, fmt.Errorf("read image: %w", err)
		}
		mime := fh.Header.Get("Content-Type")
		sniff := body
		if len(sniff) > 512 {
			sniff = sniff[:512]
		}
		if strings.HasPrefix(mime, "video/") || strings.HasPrefix(http.DetectContentType(sniff), "video/") {
			return media.Result{}, fmt.Errorf("video files must go through POST /api/media/video (async transcode pipeline)")
		}
		res, err := a.media.Process(r.Context(), body, mime)
		if err != nil {
			return media.Result{}, &mediaError{err: fmt.Errorf("media: %w", err)}
		}
		return res, nil
	}

	var imgs []dispatch.Img
	var recs []store.Media
	add := func(res media.Result, alt string) {
		imgs = append(imgs, dispatch.Img{Bytes: res.Bytes, Mime: res.Mime, Alt: alt, BlossomURL: res.URL,
			Dim: res.Dim, DurationSecs: res.DurationSecs, SizeBytes: res.Size})
		recs = append(recs, store.Media{
			Ordinal: len(recs), BlossomURL: res.URL, SHA256: res.SHA256, Mime: res.Mime,
			Dim: res.Dim, Blurhash: res.Blurhash, SizeBytes: res.Size, Alt: alt,
			DurationSecs: res.DurationSecs, PosterURL: res.PosterURL,
		})
	}

	fi := 0 // cursor into the uploaded files (fresh images, in spec order)
	for _, s := range specs {
		if s.BlossomURL != "" {
			if strings.HasPrefix(s.Mime, "video/") {
				// Video references are NEVER re-Processed (no canonical
				// mutation) and bytes are fetched only when a byte-upload
				// platform could actually use them (gates cap at ≤100 MB;
				// FetchCap is 110 MB). Larger canonicals ride as metadata-only:
				// URL platforms (threads, nostr) need nothing else and byte
				// platforms gate out first. A failed best-effort fetch leaves
				// Bytes nil — the adapter gate then fails that target loudly.
				img := dispatch.Img{Mime: s.Mime, Alt: s.Alt, BlossomURL: s.BlossomURL,
					Dim: s.Dim, DurationSecs: s.DurationSecs, SizeBytes: s.SizeBytes}
				if s.SizeBytes > 0 && s.SizeBytes <= media.FetchCap {
					if body, _, ferr := fetch(r.Context(), s.BlossomURL); ferr == nil {
						img.Bytes = body
					}
				}
				imgs = append(imgs, img)
				recs = append(recs, store.Media{
					Ordinal: len(recs), BlossomURL: s.BlossomURL, SHA256: s.SHA256, Mime: s.Mime,
					Dim: s.Dim, Blurhash: s.Blurhash, SizeBytes: s.SizeBytes, Alt: s.Alt,
					DurationSecs: s.DurationSecs, PosterURL: s.PosterURL,
				})
				continue
			}
			// Already-uploaded reference: re-fetch the bytes from Blossom.
			body, mime, ferr := fetch(r.Context(), s.BlossomURL)
			if ferr != nil {
				continue // unreachable reference → skip, never fail the post
			}
			res, perr := a.media.Process(r.Context(), body, mime)
			if perr != nil {
				continue
			}
			add(res, s.Alt)
			continue
		}
		// Fresh upload: consume the next multipart file part.
		if fi >= len(files) {
			continue // spec entry with neither file nor reference → skip
		}
		res, err := processUpload(files[fi])
		fi++
		if err != nil {
			return nil, nil, err
		}
		add(res, s.Alt)
	}
	// Defensive: process any uploaded files with no matching spec entry (a client
	// that posts files without an images[] array) as fresh images with no alt, so
	// media is never silently dropped.
	for ; fi < len(files); fi++ {
		res, err := processUpload(files[fi])
		if err != nil {
			return nil, nil, err
		}
		add(res, "")
	}
	return imgs, recs, nil
}

// mediaError marks a media-pipeline (upstream Blossom) failure so handlers can
// respond 502 Bad Gateway, distinct from 400 client errors.
type mediaError struct{ err error }

func (e *mediaError) Error() string { return e.err.Error() }

// fetchSourceMedia downloads a source media URL for re-hosting (https-only,
// size-limited, SSRF-guarded). Best-effort: callers skip on error.
func (a *API) fetchSourceMedia(ctx context.Context, rawURL string) ([]byte, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" {
		return nil, "", fmt.Errorf("bad media url")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := sourceMediaClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("media fetch %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUploadRequestBytes))
	if err != nil {
		return nil, "", err
	}
	return body, resp.Header.Get("Content-Type"), nil
}
