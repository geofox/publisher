package api

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/transcode"
	"github.com/geofox/publisher/internal/videojob"
)

// /v1/media: files uploaded once, then named by id in posts. An image is
// stored at once (201, ready); a video is normalized in the background
// (202, processing) and polled with GET /v1/media/{id}.

const (
	prefixMedia = "med_"
	maxAltRunes = 10000 // storage bound; destinations have their own limits
)

type v1MediaObjectBody struct {
	ID           string            `json:"id"`
	Status       string            `json:"status"`
	Mime         string            `json:"mime"`
	Alt          string            `json:"alt"`
	URL          *string           `json:"url"`
	SHA256       string            `json:"sha256,omitempty"`
	SizeBytes    int64             `json:"size_bytes,omitempty"`
	Dim          string            `json:"dim,omitempty"`
	Blurhash     string            `json:"blurhash,omitempty"`
	DurationSecs int64             `json:"duration_secs,omitempty"`
	PosterURL    string            `json:"poster_url,omitempty"`
	Error        string            `json:"error,omitempty"`
	Processing   *v1ProcessingBody `json:"processing,omitempty"`
	CreatedAt    string            `json:"created_at"`
	UpdatedAt    string            `json:"updated_at"`
}

type v1ProcessingBody struct {
	Stage    string  `json:"stage"`
	Progress float64 `json:"progress"`
}

func (a *API) v1MediaBody(m *store.MediaObject) v1MediaObjectBody {
	b := v1MediaObjectBody{
		ID: m.ID, Status: m.Status, Mime: m.Mime, Alt: m.Alt, SHA256: m.SHA256, SizeBytes: m.SizeBytes,
		Dim: m.Dim, Blurhash: m.Blurhash, DurationSecs: m.DurationSecs, PosterURL: m.PosterURL, Error: m.Error,
		CreatedAt: rfc3339(m.CreatedAt), UpdatedAt: rfc3339(m.UpdatedAt),
	}
	if m.URL != "" {
		u := m.URL
		b.URL = &u
	}
	if m.Status == store.MediaProcessing && a.VideoJobs != nil {
		if j, ok := a.VideoJobs.Get(m.JobID); ok {
			b.Processing = &v1ProcessingBody{Stage: j.State, Progress: j.Pct}
		}
	}
	return b
}

// refreshMedia brings a processing video up to date with its job: ready
// with the file, failed with why, or failed when the job is gone (the
// server restarted mid-way).
func (a *API) refreshMedia(ctx context.Context, m *store.MediaObject) error {
	if m.Status != store.MediaProcessing {
		return nil
	}
	var j videojob.Job
	ok := false
	if a.VideoJobs != nil {
		j, ok = a.VideoJobs.Get(m.JobID)
	}
	switch {
	case !ok:
		m.Status, m.Error = store.MediaFailed, "processing was interrupted (the server restarted); upload the video again"
	case j.State == videojob.StateError:
		m.Status, m.Error = store.MediaFailed, j.Err
	case j.State == videojob.StateDone && j.Media != nil:
		r := j.Media
		m.Status, m.URL, m.SHA256, m.Mime, m.Dim, m.Blurhash = store.MediaReady, r.URL, r.SHA256, r.Mime, r.Dim, r.Blurhash
		m.SizeBytes, m.DurationSecs, m.PosterURL = r.Size, r.DurationSecs, r.PosterURL
	default:
		return nil // still going
	}
	if _, err := a.Store.FinishMediaObject(ctx, m); err != nil {
		return err
	}
	// Another request may have finished it first; read what was stored.
	cur, err := a.Store.MediaObject(ctx, m.OwnerID, m.ID)
	if err != nil {
		return err
	}
	*m = *cur
	return nil
}

// watchVideo finishes a video's media object when its job ends, so it
// doesn't depend on someone polling before the job is forgotten.
func (a *API) watchVideo(m store.MediaObject) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	ctx := context.Background()
	for {
		select {
		case <-a.Stopping:
			return
		case <-t.C:
		}
		if err := a.refreshMedia(ctx, &m); err != nil {
			slog.Warn("v1: finish video media", "media_id", m.ID, "err", err)
			return
		}
		if m.Status != store.MediaProcessing {
			return
		}
	}
}

// ownMedia loads the media object named by the path.
func (a *API) ownMedia(w http.ResponseWriter, r *http.Request) (*store.MediaObject, bool) {
	if a.Store == nil {
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "no database")
		return nil, false
	}
	id := r.PathValue("id")
	pr, _ := principalFrom(r.Context())
	m, err := a.Store.MediaObject(r.Context(), pr.OwnerID, id)
	if errors.Is(err, store.ErrNoMedia) || !strings.HasPrefix(id, prefixMedia) {
		problem(w, r, http.StatusNotFound, codeNotFound, "no media "+id)
		return nil, false
	}
	if err == nil {
		err = a.refreshMedia(r.Context(), m)
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "v1: get media", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "reading the media failed")
		return nil, false
	}
	return m, true
}

func (a *API) v1GetMedia(w http.ResponseWriter, r *http.Request) {
	if m, ok := a.ownMedia(w, r); ok {
		writeV1(w, http.StatusOK, a.v1MediaBody(m))
	}
}

func (a *API) v1PatchMedia(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Alt *string `json:"alt"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Alt == nil {
		invalid(w, r, []fieldError{{Pointer: "/alt", Code: "required", Detail: "the alt text to set (\"\" clears it)"}})
		return
	}
	if msg := altProblem(*req.Alt); msg != "" {
		invalid(w, r, []fieldError{{Pointer: "/alt", Code: "invalid", Detail: msg}})
		return
	}
	m, ok := a.ownMedia(w, r)
	if !ok {
		return
	}
	if err := a.Store.SetMediaAlt(r.Context(), m.OwnerID, m.ID, *req.Alt); err != nil {
		slog.ErrorContext(r.Context(), "v1: set alt", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "saving the alt text failed")
		return
	}
	m, ok = a.ownMedia(w, r)
	if ok {
		writeV1(w, http.StatusOK, a.v1MediaBody(m))
	}
}

func altProblem(alt string) string {
	switch {
	case !utf8.ValidString(alt):
		return "not valid UTF-8"
	case utf8.RuneCountInString(alt) > maxAltRunes:
		return "at most 10000 characters"
	}
	return ""
}

// upload is a file being received: its first bytes, to tell its type, and
// the rest.
type upload struct {
	mime string
	body *bufio.Reader
}

func sniffUpload(declared string, r io.Reader) upload {
	br := bufio.NewReaderSize(r, 512)
	head, _ := br.Peek(512)
	mt, _, _ := mime.ParseMediaType(declared)
	if mt == "" || mt == "application/octet-stream" {
		mt = http.DetectContentType(head)
		if i := strings.IndexByte(mt, ';'); i >= 0 {
			mt = mt[:i]
		}
	}
	if !strings.HasPrefix(mt, "video/") && strings.HasPrefix(http.DetectContentType(head), "video/") {
		mt = strings.SplitN(http.DetectContentType(head), ";", 2)[0]
	}
	return upload{mime: mt, body: br}
}

// v1UploadMedia stores an image or starts processing a video. The file is
// a multipart "file" part (with optional "alt" and, for a video, "preset"
// parts) or the whole body, with its type as Content-Type and alt and
// preset in the query.
func (a *API) v1UploadMedia(w http.ResponseWriter, r *http.Request) {
	if a.Store == nil {
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "no database")
		return
	}
	// The server's timeouts are for ordinary requests; an upload of a
	// large video needs minutes.
	rc := http.NewResponseController(w)
	deadline := time.Now().Add(30 * time.Minute)
	_ = rc.SetReadDeadline(deadline)
	_ = rc.SetWriteDeadline(deadline.Add(time.Minute))
	if r.ContentLength > maxVideoUploadBytes {
		problem(w, r, http.StatusRequestEntityTooLarge, codeTooLarge, "uploads are at most 1 GiB")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxVideoUploadBytes)
	q := r.URL.Query()
	alt, preset := q.Get("alt"), q.Get("preset")
	ct, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	var file *upload
	var mr *multipart.Reader
	switch {
	case ct == "multipart/form-data":
		mr = multipart.NewReader(r.Body, params["boundary"])
		for file == nil {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				a.uploadReadProblem(w, r, err)
				return
			}
			switch part.FormName() {
			case "file":
				up := sniffUpload(part.Header.Get("Content-Type"), part)
				file = &up
			case "alt", "preset":
				b, err := io.ReadAll(io.LimitReader(part, 64<<10))
				if err != nil {
					a.uploadReadProblem(w, r, err)
					return
				}
				if part.FormName() == "alt" {
					alt = string(b)
				} else {
					preset = string(b)
				}
			default:
				_, _ = io.Copy(io.Discard, part)
			}
		}
		if file == nil {
			invalid(w, r, []fieldError{{Pointer: "/file", Code: "required", Detail: "send the file as a multipart part named file"}})
			return
		}
	case strings.HasPrefix(ct, "image/"), strings.HasPrefix(ct, "video/"), ct == "application/octet-stream":
		up := sniffUpload(ct, r.Body)
		file = &up
	default:
		problem(w, r, http.StatusUnsupportedMediaType, codeUnsupportedMediaType,
			"send multipart/form-data with a file part, or the file itself with its image/* or video/* type")
		return
	}
	if msg := altProblem(alt); msg != "" {
		invalid(w, r, []fieldError{{Pointer: "/alt", Code: "invalid", Detail: msg}})
		return
	}
	pr, _ := principalFrom(r.Context())
	if strings.HasPrefix(file.mime, "video/") {
		a.uploadVideo(w, r, pr.OwnerID, file, alt, preset)
		return
	}
	if !strings.HasPrefix(file.mime, "image/") {
		invalid(w, r, []fieldError{{Pointer: "/file", Code: "unsupported_type", Detail: "an image or a video, not " + file.mime}})
		return
	}
	body, err := io.ReadAll(io.LimitReader(file.body, maxUploadRequestBytes+1))
	if err != nil {
		a.uploadReadProblem(w, r, err)
		return
	}
	if int64(len(body)) > maxUploadRequestBytes {
		problem(w, r, http.StatusRequestEntityTooLarge, codeTooLarge, "images are at most 64 MiB")
		return
	}
	if mr != nil {
		_, _ = io.Copy(io.Discard, r.Body) // the rest of the form
	}
	if a.media == nil {
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "media storage is not configured")
		return
	}
	res, err := a.media.Process(r.Context(), body, file.mime)
	if err != nil {
		slog.WarnContext(r.Context(), "v1: store image", "err", err)
		if strings.Contains(err.Error(), "heic convert") {
			invalid(w, r, []fieldError{{Pointer: "/file", Code: "unreadable", Detail: "the HEIC image could not be converted"}})
			return
		}
		problem(w, r, http.StatusBadGateway, codeUpstream, "media storage refused the file: "+err.Error())
		return
	}
	m := &store.MediaObject{OwnerID: pr.OwnerID, Status: store.MediaReady, URL: res.URL, SHA256: res.SHA256, Mime: res.Mime,
		Dim: res.Dim, Blurhash: res.Blurhash, SizeBytes: res.Size, Alt: alt}
	if err := a.Store.CreateMediaObject(r.Context(), m); err != nil {
		slog.ErrorContext(r.Context(), "v1: save media", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "saving the media failed")
		return
	}
	w.Header().Set("Location", "/v1/media/"+m.ID)
	writeV1(w, http.StatusCreated, a.v1MediaBody(m))
}

func (a *API) uploadVideo(w http.ResponseWriter, r *http.Request, owner string, file *upload, alt, preset string) {
	if a.VideoJobs == nil {
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "video processing is not available (ffmpeg is missing)")
		return
	}
	if preset == "" {
		preset = "1080p"
	}
	if !transcode.IsVideoPreset(preset) {
		invalid(w, r, []fieldError{{Pointer: "/preset", Code: "invalid", Detail: "one of " + strings.Join(transcode.VideoPresetNames, ", ")}})
		return
	}
	if a.VideoJobs.Full() {
		w.Header().Set("Retry-After", "60")
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "the video queue is full; try again when a video has finished")
		return
	}
	tmp, err := os.CreateTemp(a.videoWorkdir(), "upload-*.video")
	if err != nil {
		slog.ErrorContext(r.Context(), "v1: stage video", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "staging the upload failed")
		return
	}
	_, err = io.Copy(tmp, file.body)
	tmp.Close()
	if err != nil {
		os.Remove(tmp.Name())
		var pe *fs.PathError
		if errors.As(err, &pe) && pe.Op == "write" {
			slog.ErrorContext(r.Context(), "v1: stage video", "err", err)
			problem(w, r, http.StatusInsufficientStorage, codeInternal, "the server ran out of room for the upload")
			return
		}
		a.uploadReadProblem(w, r, err)
		return
	}
	_, _ = io.Copy(io.Discard, r.Body)
	jobID, err := a.VideoJobs.Submit(context.WithoutCancel(r.Context()), tmp.Name(), preset)
	if err != nil {
		w.Header().Set("Retry-After", "60")
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "the video queue is full; try again when a video has finished")
		return
	}
	m := &store.MediaObject{OwnerID: owner, Status: store.MediaProcessing, Mime: file.mime, Alt: alt, JobID: jobID}
	if err := a.Store.CreateMediaObject(r.Context(), m); err != nil {
		slog.ErrorContext(r.Context(), "v1: save media", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "saving the media failed")
		return
	}
	go a.watchVideo(*m)
	w.Header().Set("Location", "/v1/media/"+m.ID)
	writeV1(w, http.StatusAccepted, a.v1MediaBody(m))
}

func (a *API) uploadReadProblem(w http.ResponseWriter, r *http.Request, err error) {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		problem(w, r, http.StatusRequestEntityTooLarge, codeTooLarge, "uploads are at most 1 GiB")
		return
	}
	problem(w, r, http.StatusBadRequest, codeUnreadableUpload, "the upload could not be read: "+err.Error())
}

// v1CompressMedia re-encodes an image with a preset and answers the bytes;
// nothing is stored. "convert" turns HEIC into JPEG.
func (a *API) v1CompressMedia(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadRequestBytes+1<<20)
	ct, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	preset := r.URL.Query().Get("preset")
	var file *upload
	var body []byte
	switch {
	case ct == "multipart/form-data":
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				a.uploadReadProblem(w, r, err)
				return
			}
			switch part.FormName() {
			case "file":
				up := sniffUpload(part.Header.Get("Content-Type"), part)
				b, err := io.ReadAll(up.body)
				if err != nil {
					a.uploadReadProblem(w, r, err)
					return
				}
				file, body = &up, b
			case "preset":
				b, _ := io.ReadAll(io.LimitReader(part, 256))
				preset = string(b)
			default:
				_, _ = io.Copy(io.Discard, part)
			}
		}
	case strings.HasPrefix(ct, "image/"), ct == "application/octet-stream":
		up := sniffUpload(ct, r.Body)
		b, err := io.ReadAll(up.body)
		if err != nil {
			a.uploadReadProblem(w, r, err)
			return
		}
		file, body = &up, b
	default:
		problem(w, r, http.StatusUnsupportedMediaType, codeUnsupportedMediaType,
			"send multipart/form-data with a file part, or the image itself with its image/* type")
		return
	}
	var errs []fieldError
	if file == nil {
		errs = append(errs, fieldError{Pointer: "/file", Code: "required", Detail: "send the image as a multipart part named file"})
	}
	params2, ok := transcode.PresetParams(preset)
	if !ok {
		errs = append(errs, fieldError{Pointer: "/preset", Code: "invalid", Detail: "one of " + strings.Join(transcode.ImagePresetNames, ", ")})
	}
	if len(errs) > 0 {
		invalid(w, r, errs)
		return
	}
	select {
	case compressSem <- struct{}{}:
		defer func() { <-compressSem }()
	case <-r.Context().Done():
		return
	}
	res, err := transcode.Image(body, file.mime, params2)
	if err != nil || !res.Changed {
		detail := "the image could not be re-encoded (not decodable, or over 100 megapixels)"
		if err != nil {
			detail = err.Error()
		}
		invalid(w, r, []fieldError{{Pointer: "/file", Code: "unreadable", Detail: detail}})
		return
	}
	w.Header().Set("Content-Type", res.Mime)
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, bytes.NewReader(res.Bytes))
}

// mediaResult is a stored object as the post planner takes it.
func mediaRecord(m *store.MediaObject, ordinal int, alt string) store.Media {
	return store.Media{Ordinal: ordinal, BlossomURL: m.URL, SHA256: m.SHA256, Mime: m.Mime, Dim: m.Dim,
		Blurhash: m.Blurhash, SizeBytes: m.SizeBytes, Alt: alt, DurationSecs: m.DurationSecs, PosterURL: m.PosterURL}
}
