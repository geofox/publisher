package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/geofox/publisher/internal/dispatch"
	"github.com/geofox/publisher/internal/httpx"
	"github.com/geofox/publisher/internal/progress"
	"github.com/geofox/publisher/internal/store"
)

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
