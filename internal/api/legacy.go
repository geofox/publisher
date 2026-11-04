package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"fiatjaf.com/nostr"

	"github.com/geofox/publisher/internal/httpx"
	pubnostr "github.com/geofox/publisher/internal/nostr"
)

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
