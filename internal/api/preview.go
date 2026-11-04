package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/geofox/publisher/internal/dispatch"
	"github.com/geofox/publisher/internal/httpx"
	"github.com/geofox/publisher/internal/thread"
	"github.com/geofox/publisher/internal/transcode"
	"github.com/geofox/publisher/internal/unfurl"
)

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
