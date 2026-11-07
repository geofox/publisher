package api

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/geofox/publisher/internal/accounts"
	"github.com/geofox/publisher/internal/auth"
	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/health"
	"github.com/geofox/publisher/internal/httpx"
	"github.com/geofox/publisher/internal/localauth"
	"github.com/geofox/publisher/internal/media"
	"github.com/geofox/publisher/internal/metrics"
	"github.com/geofox/publisher/internal/netguard"
	"github.com/geofox/publisher/internal/relaysync"
	"github.com/geofox/publisher/internal/settings"
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
	// Saving a post is Enqueue (enqueuer) or EnqueueInteraction
	// (interactionEnqueuer); the engine has both.
	RetryTargetRelay(ctx context.Context, postID string, targetID int64, relay string) (*store.Post, error)
}

// Syncer is implemented by *relaysync.Sync: it scans or syncs the events of
// the account named on each call.
type Syncer interface {
	Scan(ctx context.Context, who relaysync.Owner, targets []relaysync.Target) []relaysync.RelayDiff
	Apply(ctx context.Context, who relaysync.Owner, targets []relaysync.Target, direction string) []relaysync.ApplyResult
}

// Verifier is implemented by *verify.Service; extracted so the api package can
// be tested with a stub and has no hard dependency on the concrete verify service.
type Verifier interface {
	Verify(ctx context.Context, in verify.Input) verify.Verdict
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

// ItemLocator finds an account's own posts by URL; implemented by
// dispatch.Dispatcher.
type ItemLocator interface {
	CanLocate(accountID string) bool
	Locate(ctx context.Context, accountID, url string) (destination.ItemRef, error)
}

// AccountRegistry is the account registry as the API uses it: whether an
// account can be used now, its profile, and a lease on its open account
// (looking posts up, relay sync). Implemented by accounts.Registry.
type AccountRegistry interface {
	Get(id string) (accounts.Entry, bool)
	// List is the owner's accounts in registry order.
	List(owner string) []accounts.Entry
	// Acquire leases a connected account; release must be called.
	Acquire(id string) (destination.Account, func(), error)
	// Profile is the account's last known profile, at once.
	Profile(id string) (accounts.Profile, bool)
	// Update takes the owner's settings from a row the store just saved.
	Update(id string, row store.Account) error
}

// API holds the dependencies for the HTTP handlers.
type API struct {
	media    *media.Pipeline
	Dispatch Dispatcher   // set by cmd/publisher (exported so tests can inject fakes)
	Store    *store.Store // set by cmd/publisher
	// Sync scans and syncs a Nostr account's relays (relaysync.Sync); the
	// account comes from Accounts. nil: relay sync is off.
	Sync Syncer
	// MasterKey is the master key's status at startup (accounts.PrepareKeys):
	// whether credentials can be saved. The zero value: no key.
	MasterKey accounts.KeyState
	// IdemMAC is a keyed MAC under the master key (secrets.Keyring.MAC):
	// the idempotency fingerprint of a request that holds credentials.
	// nil, or nil answers: a key of this process.
	IdemMAC     func([]byte) []byte
	idemKey     []byte
	idemKeyOnce sync.Once
	// acctLimits rate-limit account writes and checks.
	acctLimits accountLimits
	// AccountWritesPerMinute overrides the owner's limit on account
	// creates and credential changes (0: 10 a minute). For tests that add
	// many accounts in a row.
	AccountWritesPerMinute int
	// Guard is who owner-set addresses may reach (PRIVATE_DESTINATIONS
	// and the server's own addresses): relay sync's targets go through
	// it. The zero value allows public addresses only.
	Guard  netguard.Policy
	Verify Verifier // set by cmd/publisher; verifies pasted events / post URLs
	// relaySyncMu lets one /v1 relay scan or sync run at a time.
	relaySyncMu sync.Mutex
	// Locator looks up an account's own posts when the owner resolves an
	// unknown outcome by hand. nil: URLs are kept as given.
	Locator ItemLocator

	// UserLanguages is the operator's spoken-languages list (ISO 639-1 codes);
	// in GET /v1/instance so the web app can default the language fields and
	// offer a dropdown when there's more than one.
	UserLanguages []string

	// Translator powers translation (POST /v1/translate, drafts' :translate).
	// nil → disabled: 503, and /v1/instance lists no translate_targets.
	Translator Translator

	// PublicFeedToken gates GET /v1/public/feed. Empty → the endpoint is
	// disabled (404). Set → callers must send Authorization: Bearer <it>.
	PublicFeedToken string

	// Settings, when set, holds the owner's and the instance's settings;
	// the languages, the translation key and the feed token are read from
	// it at each request (in place of the three fields above).
	Settings *settings.Service
	// TestSettings sends a settings group's test (POST
	// /v1/settings/{group}:test); nil: no tests.
	TestSettings SettingsTester
	// NewTranslator makes a translator for a key (nil: DeepL).
	NewTranslator func(key string) Translator
	deepl         atomic.Pointer[deeplFor]

	// Caps returns an account's effective capabilities (the dispatcher's
	// Caps). nil → the registered drivers' defaults.
	Caps func(ctx context.Context, accountID, typ string) destination.Capabilities

	// Accounts is the account registry: which accounts can send now, their
	// profiles, and the accounts that look posts up and sync relays. nil →
	// every enabled account of a registered type can send, and nothing
	// that needs an open account works.
	Accounts AccountRegistry

	// fetchMedia downloads a quoted post's images to copy them. nil → the
	// SSRF-guarded client. Overridable in tests.
	fetchMedia func(ctx context.Context, rawURL string) ([]byte, string, error)

	// Mode is how callers sign in: ModeOIDC, ModeLocal or ModeProxy (the
	// reverse proxy in front is the gate). "" (tests): OIDC when Auth is
	// set, else proxy.
	Mode string
	// Local is local sign-in (passkeys, password, setup); required in
	// ModeLocal.
	Local *localauth.Service
	// Clients finds a request's client address (TRUSTED_PROXIES). nil:
	// the connection's peer.
	Clients *httpx.ClientAddrs
	// PublicURL is the origin the owner's browser uses (PUBLIC_URL), "" when
	// unset. Local sign-in requires requests from exactly this origin.
	PublicURL string
	// SetupTokenFile (SETUP_TOKEN_FILE) is removed once setup is done.
	SetupTokenFile string

	// Auth holds the OIDC Relying Party, in ModeOIDC. Set by cmd/publisher
	// when OIDC_ISSUER is configured.
	Auth *auth.Authenticator
	// Allowlist gates which verified identities may use the app. Non-nil iff Auth is non-nil.
	Allowlist *auth.Allowlist
	// SessionTTL is how long a new session lives. Zero → 168h default applied by handlers.
	SessionTTL time.Duration
	// EndSession toggles provider single-logout on /auth/logout.
	EndSession bool
	// AppBaseURL is the public origin (scheme://host) used to build post-logout redirects.
	AppBaseURL string
	// AllowedHosts, when set, are the only Host names served (plus
	// loopback names and AppBaseURL's host): a DNS-rebinding page whose name
	// now points at this server sends its own name as Host and is refused.
	AllowedHosts []string

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

	streams atomic.Int64 // open event streams

	// oidcReauth holds OIDC re-authentication logins in flight.
	oidcReauth oidcReauths
}

// New creates a new API with the given media pipeline.
func New(mp *media.Pipeline) *API {
	return &API{media: mp}
}

// Routes returns the HTTP handler with all API routes registered, wrapped in
// the security-header and CSRF middleware.
func (a *API) Routes() http.Handler {
	a.checkMode()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", a.handleHealthz)
	mux.HandleFunc("GET /readyz", a.handleReadyz)
	mux.Handle("GET /metrics", metrics.Handler())
	// The pre-1.0 API is gone; its paths say where it went.
	for _, p := range oldAPIPaths {
		mux.HandleFunc(p, handleGone)
	}
	// Sign-in routes, by mode (none behind a proxy).
	switch a.mode() {
	case ModeOIDC:
		mux.HandleFunc("GET /auth/login", a.handleAuthLogin)
		mux.HandleFunc("GET /auth/callback", a.handleAuthCallback)
		mux.HandleFunc("POST /auth/logout", a.handleAuthLogout)
		mux.HandleFunc("GET /auth/reauth/oidc", a.handleOIDCReauthStart)
		mux.HandleFunc("POST /auth/reauth/claim", a.handleOIDCReauthClaim)
	case ModeLocal:
		a.mountLocal(mux)
	}
	a.mountV1(mux)
	mux.Handle("/", web.Handler())
	return withSecurityHeaders(a.withHostGuard(a.withCSRFGuard(a.withGates(mux))))
}

// caps returns an account's effective capabilities: the open account's,
// else its type's defaults, else none (no limits).
func (a *API) caps(ctx context.Context, accountID, typ string) destination.Capabilities {
	if a.Caps != nil {
		return a.Caps(ctx, accountID, typ)
	}
	if d, ok := destination.Lookup(typ); ok {
		return d.Manifest().Defaults
	}
	return destination.Capabilities{}
}

// accountUnusable says why an account can't send now, or "" when it can.
func (a *API) accountUnusable(ac store.Account) string {
	if _, ok := destination.Lookup(ac.Type); !ok {
		return "this server has no driver for " + ac.Type
	}
	if a.Accounts == nil {
		if !ac.Enabled {
			return "this account is turned off"
		}
		return ""
	}
	e, ok := a.Accounts.Get(ac.ID)
	switch {
	case !ok:
		return "this account isn't loaded yet; try again after a restart"
	case e.Status != accounts.Connected:
		return (&accounts.Unavailable{ID: ac.ID, Status: e.Status, Reason: e.Reason}).Error()
	}
	return ""
}

// displayName is a destination type's human name ("Bluesky").
func displayName(name string) string {
	if d, ok := destination.Lookup(name); ok && d.Manifest().Name != "" {
		return d.Manifest().Name
	}
	return name
}

// oldAPIPaths are the pre-1.0 API's routes (patterns), answered 410.
var oldAPIPaths = []string{"/publish", "/upload-media", "/api/"}

// isOldAPIPath reports whether a cleaned path is one of the old routes.
func isOldAPIPath(p string) bool {
	return p == "/publish" || p == "/upload-media" || p == "/api" || strings.HasPrefix(p, "/api/")
}

// handleGone answers a call to the pre-1.0 API: removed in 1.0, replaced
// by /v1. Open (no session or token needed), so a script or n8n flow sees
// why instead of a sign-in page.
func handleGone(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteError(w, http.StatusGone, "the pre-1.0 API was removed; use /v1 (see "+
		"https://github.com/geofox/publisher#migrating-from-the-pre-10-api)")
}
