package api

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/geofox/publisher/internal/dispatch"
	"github.com/geofox/publisher/internal/feed"
	"github.com/geofox/publisher/internal/httpx"
	"github.com/geofox/publisher/internal/progress"
	"github.com/geofox/publisher/internal/store"
)

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
