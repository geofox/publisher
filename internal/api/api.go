package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"fiatjaf.com/nostr"

	"github.com/geofox/publisher/internal/dispatch"
	"github.com/geofox/publisher/internal/httpx"
	"github.com/geofox/publisher/internal/media"
	pubnostr "github.com/geofox/publisher/internal/nostr"
	"github.com/geofox/publisher/internal/relaysync"
	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/thread"
	"github.com/geofox/publisher/internal/verify"
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
	maxPostRequestBytes    int64 = 64 << 20  // 64 MB (spec + up to 4 images)
	maxVerifyRequestBytes  int64 = 512 << 10 // 512 KB (pasted event JSON or a URL)
	maxThreadPreviewBytes  int64 = 256 << 10 // 256 KB (draft text for split preview)
)

// Dispatcher is implemented by dispatch.Dispatcher; extracted as an interface
// so the api package has no concrete dependency on the dispatcher and tests
// can substitute a fake.
type Dispatcher interface {
	Post(ctx context.Context, spec dispatch.PostSpec) *store.Post
	Retry(ctx context.Context, id string, platforms []string) (*store.Post, error)
	RetryRelay(ctx context.Context, id, relay string) (*store.Post, error)
	Schedule(ctx context.Context, spec dispatch.PostSpec, at time.Time) (*store.Post, error)
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

// API holds the dependencies for the HTTP handlers.
type API struct {
	np        *pubnostr.Publisher
	media     *media.Pipeline
	Dispatch  Dispatcher   // set by cmd/publisher (exported so tests can inject fakes)
	Store     *store.Store // set by cmd/publisher; used by history endpoints
	Sync      Syncer       // set by cmd/publisher; used by relay-sync endpoints
	HomeRelay string       // set by cmd/publisher; the home relay URL
	Verify    Verifier     // set by cmd/publisher; verifies pasted events / post URLs
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
	mux.HandleFunc("/publish", a.handlePublish)
	mux.HandleFunc("/upload-media", a.handleUploadMedia)
	mux.HandleFunc("/api/post", a.handleAPIPost)
	mux.HandleFunc("GET /api/posts", a.handleListPosts)
	mux.HandleFunc("GET /api/posts/{id}", a.handleGetPost)
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
	mux.Handle("/", web.Handler())
	return withSecurityHeaders(withCSRFGuard(mux))
}

// contentSecurityPolicy locks the SPA to same-origin code. Every script, style,
// and font is a same-origin asset and no markup uses inline scripts/styles, so
// the policy needs no 'unsafe-inline' — which is what makes it an effective
// stored/reflected-XSS backstop (e.g. a hostile platform remote_url). Images
// additionally allow https/blob/data for Blossom thumbnails and local previews.
const contentSecurityPolicy = "default-src 'self'; " +
	"img-src 'self' https: data: blob:; style-src 'self'; script-src 'self'; " +
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
// the owner's authenticated (Authelia) session. Same-origin SPA calls (Origin
// matches Host) and server-to-server callers like n8n (no Origin header) pass
// through, so CSRF is blocked without an app-level token scheme.
func withCSRFGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			// safe methods don't mutate state
		default:
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || !sameHost(u.Host, r.Host) {
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

// ─── /healthz ────────────────────────────────────────────────────────────

func (a *API) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("content-type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
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

// ─── /api/post ───────────────────────────────────────────────────────────

// postSpecJSON is the JSON object expected in the "spec" multipart field.
type postSpecJSON struct {
	MasterText   string                        `json:"master_text"`
	Platforms    []string                      `json:"platforms"`
	DelaySeconds int                           `json:"delay_seconds"`
	Overrides    map[string]dispatch.Overrides `json:"overrides"`
	Images       []struct {
		Alt string `json:"alt"`
	} `json:"images"`
	ScheduledAt string `json:"scheduled_at"`
	Number      bool   `json:"number"`
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

	var imgs []dispatch.Img
	var mediaRecs []store.Media
	files := r.MultipartForm.File["image"]
	if len(files) > 4 {
		httpx.WriteError(w, http.StatusBadRequest, "max 4 images")
		return
	}
	for i, fh := range files {
		f, err := fh.Open()
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "open image: "+err.Error())
			return
		}
		body, err := io.ReadAll(f)
		if err != nil {
			f.Close()
			httpx.WriteError(w, http.StatusInternalServerError, "read image: "+err.Error())
			return
		}
		f.Close()
		res, err := a.media.Process(r.Context(), body, fh.Header.Get("Content-Type"))
		if err != nil {
			httpx.WriteError(w, http.StatusBadGateway, "media: "+err.Error())
			return
		}
		alt := ""
		if i < len(sj.Images) {
			alt = sj.Images[i].Alt
		}
		imgs = append(imgs, dispatch.Img{Bytes: res.Bytes, Mime: res.Mime, Alt: alt, BlossomURL: res.URL})
		mediaRecs = append(mediaRecs, store.Media{
			Ordinal: i, BlossomURL: res.URL, SHA256: res.SHA256, Mime: res.Mime,
			Dim: res.Dim, Blurhash: res.Blurhash, SizeBytes: res.Size, Alt: alt,
		})
	}

	spec := dispatch.PostSpec{
		MasterText: sj.MasterText, Platforms: sj.Platforms, DelaySeconds: sj.DelaySeconds,
		Source: "web", Overrides: sj.Overrides, Images: imgs, MediaRecords: mediaRecs,
		Number: sj.Number,
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
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"post_id": srec.ID, "status": srec.Status, "scheduled_at": srec.ScheduledAt,
		})
		return
	}
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
		Text      string   `json:"text"`
		Platforms []string `json:"platforms"`
		Number    bool     `json:"number"`
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
	type preview struct {
		Platform string   `json:"platform"`
		Count    int      `json:"count"`
		Segments []string `json:"segments"`
		Warnings []string `json:"warnings,omitempty"`
	}
	out := struct {
		Previews []preview `json:"previews"`
	}{Previews: []preview{}}
	for _, p := range req.Platforms {
		segs, warns := thread.Split(req.Text, thread.LimitFor(p), thread.Opts{Number: req.Number})
		out.Previews = append(out.Previews, preview{
			Platform: p, Count: len(segs), Segments: segs, Warnings: warns,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
