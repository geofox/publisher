package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/geofox/publisher/internal/httpx"
	"github.com/geofox/publisher/internal/store"
)

const (
	maxDraftRequestBytes = int64(64 << 20) // mirrors maxPostRequestBytes
	draftMultipartMemory = int64(8 << 20)
)

type draftImageEntry struct {
	ID           string `json:"id,omitempty"`
	Ordinal      int    `json:"ordinal"`
	Ref          string `json:"ref,omitempty"`         // "img_0", "img_1", … — present iff newly uploaded
	BlossomURL   string `json:"blossom_url,omitempty"` // present iff already uploaded
	SHA256       string `json:"sha256,omitempty"`
	Mime         string `json:"mime,omitempty"`
	Dim          string `json:"dim,omitempty"`
	Blurhash     string `json:"blurhash,omitempty"`
	SizeBytes    int64  `json:"size_bytes,omitempty"`
	Alt          string `json:"alt,omitempty"`
	DurationSecs int64  `json:"duration_secs,omitempty"`
	PosterURL    string `json:"poster_url,omitempty"`
}

type draftSpecJSON struct {
	MasterText  string            `json:"master_text"`
	Platforms   []string          `json:"platforms"`
	Overrides   json.RawMessage   `json:"overrides"`
	Interaction json.RawMessage   `json:"interaction,omitempty"`
	Anchors     json.RawMessage   `json:"anchors,omitempty"`
	Tags        []string          `json:"tags"`
	Images      []draftImageEntry `json:"images"`
}

func newDraftID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (a *API) handleListDrafts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.DraftFilter{
		Query:  q.Get("q"),
		Tags:   q["tag"],
		Limit:  atoiOr(q.Get("limit"), 50),
		Offset: atoiOr(q.Get("offset"), 0),
	}
	items, err := a.Store.ListDraftsFiltered(f)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, items)
}

func (a *API) handleCreateDraft(w http.ResponseWriter, r *http.Request) {
	d, _, status, msg := a.buildDraftFromRequest(r, "")
	if status != 0 {
		httpx.WriteError(w, status, msg)
		return
	}
	if err := a.Store.CreateDraft(d); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, d)
}

// buildDraftFromRequest parses a multipart request, processes any newly
// uploaded images via the media pipeline, and returns a fully-populated
// *store.Draft. If id is empty, a new one is generated. On error returns a
// non-zero HTTP status and message; on success returns (draft, normalizedSpecJSON, 0, "").
func (a *API) buildDraftFromRequest(r *http.Request, id string) (*store.Draft, string, int, string) {
	r.Body = http.MaxBytesReader(nil, r.Body, maxDraftRequestBytes)
	if err := r.ParseMultipartForm(draftMultipartMemory); err != nil {
		return nil, "", http.StatusBadRequest, "parse multipart: " + err.Error()
	}
	specRaw := r.FormValue("spec")
	var sj draftSpecJSON
	if err := json.Unmarshal([]byte(specRaw), &sj); err != nil {
		return nil, "", http.StatusBadRequest, "invalid spec json: " + err.Error()
	}

	mediaRecs := make([]store.Media, 0, len(sj.Images))
	for i, img := range sj.Images {
		if img.Ref != "" {
			// new upload via media pipeline
			fhs := r.MultipartForm.File[img.Ref]
			if len(fhs) == 0 {
				return nil, "", http.StatusBadRequest, fmt.Sprintf("image %d: missing multipart field %q", i, img.Ref)
			}
			f, err := fhs[0].Open()
			if err != nil {
				return nil, "", http.StatusBadRequest, fmt.Sprintf("image %d: open: %v", i, err)
			}
			body, err := io.ReadAll(f)
			_ = f.Close()
			if err != nil {
				return nil, "", http.StatusBadRequest, fmt.Sprintf("image %d: read: %v", i, err)
			}
			mime := fhs[0].Header.Get("Content-Type")
			sniff := body
			if len(sniff) > 512 {
				sniff = sniff[:512]
			}
			if strings.HasPrefix(mime, "video/") || strings.HasPrefix(http.DetectContentType(sniff), "video/") {
				return nil, "", http.StatusBadRequest, fmt.Sprintf("image %d: video files must go through POST /api/media/video (async transcode pipeline)", i)
			}
			res, err := a.media.Process(r.Context(), body, mime)
			if err != nil {
				return nil, "", http.StatusBadGateway, fmt.Sprintf("image %d: upload: %v", i, err)
			}
			mediaRecs = append(mediaRecs, store.Media{
				Ordinal: img.Ordinal, BlossomURL: res.URL, SHA256: res.SHA256,
				Mime: res.Mime, Dim: res.Dim, Blurhash: res.Blurhash, SizeBytes: res.Size,
				Alt: img.Alt, DurationSecs: res.DurationSecs, PosterURL: res.PosterURL,
				ClientID: img.ID,
			})
		} else if img.BlossomURL != "" {
			mediaRecs = append(mediaRecs, store.Media{
				Ordinal: img.Ordinal, BlossomURL: img.BlossomURL, SHA256: img.SHA256,
				Mime: img.Mime, Dim: img.Dim, Blurhash: img.Blurhash, SizeBytes: img.SizeBytes,
				Alt: img.Alt, DurationSecs: img.DurationSecs, PosterURL: img.PosterURL,
				ClientID: img.ID,
			})
		} else {
			return nil, "", http.StatusBadRequest, fmt.Sprintf("image %d: neither ref nor blossom_url present", i)
		}
	}

	// We re-marshal the spec without the images array (server is the source of
	// truth for media) and store it as spec_json. The frontend reconstructs the
	// images entries from draft.Media on load.
	sj.Images = nil
	sj.Tags = store.NormalizeTags(sj.Tags)
	specJSON, _ := json.Marshal(sj)

	if id == "" {
		id = newDraftID()
	}
	now := time.Now().UTC()
	d := &store.Draft{
		ID:         id,
		CreatedAt:  now,
		UpdatedAt:  now,
		Title:      store.DeriveTitle(sj.MasterText),
		MasterText: sj.MasterText,
		Tags:       sj.Tags,
		Spec:       string(specJSON),
		Media:      mediaRecs,
	}
	return d, string(specJSON), 0, ""
}

func (a *API) handleGetDraft(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	d, err := a.Store.GetDraft(id)
	if err != nil {
		if errorsIsNotFound(err) {
			httpx.WriteError(w, http.StatusNotFound, "draft not found")
			return
		}
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, d)
}

func (a *API) handleUpdateDraft(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		httpx.WriteError(w, http.StatusBadRequest, "missing draft id")
		return
	}
	d, _, status, msg := a.buildDraftFromRequest(r, id)
	if status != 0 {
		httpx.WriteError(w, status, msg)
		return
	}
	// preserve original CreatedAt (not editable)
	if existing, err := a.Store.GetDraft(id); err == nil {
		d.CreatedAt = existing.CreatedAt
	}
	if err := a.Store.UpdateDraft(d); err != nil {
		if errorsIsNotFound(err) {
			httpx.WriteError(w, http.StatusNotFound, "draft not found")
			return
		}
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, d)
}

func (a *API) handleDeleteDraft(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := a.Store.DeleteDraft(id); err != nil {
		if errorsIsNotFound(err) {
			httpx.WriteError(w, http.StatusNotFound, "draft not found")
			return
		}
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// errorsIsNotFound centralizes the wrapped-error check for ErrDraftNotFound.
func errorsIsNotFound(err error) bool { return errors.Is(err, store.ErrDraftNotFound) }

type translateDraftReq struct {
	Target string `json:"target"`
}

func (a *API) handleTranslateDraft(w http.ResponseWriter, r *http.Request) {
	if a.Translator == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "translation is not configured")
		return
	}
	id := r.PathValue("id")
	src, err := a.Store.GetDraft(id)
	if err != nil {
		if errorsIsNotFound(err) {
			httpx.WriteError(w, http.StatusNotFound, "draft not found")
			return
		}
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var req translateDraftReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Target) == "" {
		httpx.WriteError(w, http.StatusBadRequest, "missing target")
		return
	}
	translated, _, err := a.Translator.Translate(r.Context(), src.MasterText, req.Target)
	if err != nil {
		httpx.WriteError(w, http.StatusBadGateway, "translate: "+err.Error())
		return
	}

	// Build a new spec by copying the original spec's platforms/interaction,
	// dropping overrides (they'll be re-derived in the editor), and replacing
	// master_text with the translation.
	var origSpec draftSpecJSON
	_ = json.Unmarshal([]byte(src.Spec), &origSpec)
	newSpec := draftSpecJSON{
		MasterText:  translated,
		Platforms:   origSpec.Platforms,
		Interaction: origSpec.Interaction,
		Anchors:     origSpec.Anchors,
		Tags:        src.Tags,
		Overrides:   json.RawMessage("{}"),
	}
	specJSON, _ := json.Marshal(newSpec)

	now := time.Now().UTC()
	d := &store.Draft{
		ID:         newDraftID(),
		CreatedAt:  now,
		UpdatedAt:  now,
		Title:      store.DeriveTitle(translated),
		MasterText: translated,
		Tags:       src.Tags,
		Spec:       string(specJSON),
		// Copy media references — content-addressed, no re-upload.
		Media: append([]store.Media(nil), src.Media...),
	}
	if err := a.Store.CreateDraft(d); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, d)
}
