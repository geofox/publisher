package api

import (
	"context"
	"github.com/geofox/publisher/internal/destination"
	"net/http"
	"time"

	"github.com/geofox/publisher/internal/auth"
	"github.com/geofox/publisher/internal/dispatch"
	"github.com/geofox/publisher/internal/health"
	"github.com/geofox/publisher/internal/identity"
	"github.com/geofox/publisher/internal/media"
	"github.com/geofox/publisher/internal/metrics"
	pubnostr "github.com/geofox/publisher/internal/nostr"
	"github.com/geofox/publisher/internal/progress"
	"github.com/geofox/publisher/internal/relaysync"
	"github.com/geofox/publisher/internal/resolve"
	"github.com/geofox/publisher/internal/store"
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

	// Caps returns a target's effective capabilities (the dispatcher's
	// Caps). nil → the registered drivers' defaults.
	Caps func(ctx context.Context, name string) (destination.Capabilities, bool)

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

	// Ready runs the /readyz checks. nil → /readyz always reports ok (tests).
	Ready *health.Checker

	// Version is the build's version, reported by GET /v1/instance.
	Version string

	// Stopping is closed when the server shuts down, which ends the event
	// streams (they would otherwise hold the shutdown until its deadline).
	// nil: streams end only when their client leaves.
	Stopping <-chan struct{}
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
	mux.HandleFunc("GET /readyz", a.handleReadyz)
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
	a.mountV1(mux)
	mux.Handle("/", web.Handler())
	return withSecurityHeaders(a.withCSRFGuard(a.withGates(mux)))
}

// caps returns a target's effective capabilities: the connected account's,
// else the registered driver's defaults, else none (no limits).
func (a *API) caps(ctx context.Context, name string) destination.Capabilities {
	if a.Caps != nil {
		if c, ok := a.Caps(ctx, name); ok {
			return c
		}
	}
	if d, ok := destination.Lookup(name); ok {
		return d.Manifest().Defaults
	}
	return destination.Capabilities{}
}

// displayName is a destination type's human name ("Bluesky").
func displayName(name string) string {
	if d, ok := destination.Lookup(name); ok && d.Manifest().Name != "" {
		return d.Manifest().Name
	}
	return name
}
