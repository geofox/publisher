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
		// The cause (ffprobe's words, paths, storage addresses) goes to the
		// log, not to the caller.
		slog.WarnContext(ctx, "v1: video processing failed", "media_id", m.ID, "job", m.JobID, "err", j.Err)
		m.Status, m.Error = store.MediaFailed, "the video could not be processed: it may be unreadable, or media storage failed (the server log has details)"
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
			// A busy database is no reason to stop: the job's result is
			// only kept for a while.
			slog.Warn("v1: finish video media", "media_id", m.ID, "err", err)
			continue
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

// Upload bounds. Images are read into memory (the pipeline hashes and
// measures them), so few are read at once; videos stream to disk, so few
// are staged at once (each can be 1 GiB).
var (
	imageReadSem   = make(chan struct{}, 4)
	videoUploadSem = make(chan struct{}, 2)
)

const (
	imageUploadDeadline = 5 * time.Minute
	videoUploadDeadline = 30 * time.Minute
)

// uploadImageTypes are the image types the pipeline and the destinations
// handle (HEIC is converted to JPEG on the way in).
var uploadImageTypes = map[string]bool{
	"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true,
	"image/heic": true, "image/heif": true,
}

// uploadVideoTypes are the video types taken on the word of the client
// when the bytes don't say (ffprobe checks them anyway).
var uploadVideoTypes = map[string]bool{
	"video/mp4": true, "video/quicktime": true, "video/webm": true, "video/x-matroska": true,
}

// sniffType names an upload's type from its first bytes. The content
// decides, not the client: a declared type is used only for a video
// container the sniffer can't name.
func sniffType(declared string, head []byte) string {
	if t := ftypType(head); t != "" {
		return t
	}
	dt, _, _ := mime.ParseMediaType(http.DetectContentType(head))
	if uploadImageTypes[dt] || strings.HasPrefix(dt, "video/") {
		return dt
	}
	if mt, _, _ := mime.ParseMediaType(declared); uploadVideoTypes[mt] {
		return mt
	}
	return dt
}

// ftypType names an ISO base media file (MP4, QuickTime, HEIF) by its
// ftyp brand; http.DetectContentType knows only MP4 brands.
func ftypType(head []byte) string {
	if len(head) < 12 || string(head[4:8]) != "ftyp" {
		return ""
	}
	switch string(head[8:12]) {
	case "heic", "heix", "heim", "heis", "hevc", "hevx":
		return "image/heic"
	case "mif1", "msf1":
		return "image/heif"
	case "avif", "avis":
		return "image/avif" // not taken: the pipeline can't decode it
	case "qt  ":
		return "video/quicktime"
	}
	return "video/mp4"
}

// staged is the file of an upload, received: an image's bytes, or a
// video in a temporary file.
type staged struct {
	mime    string
	data    []byte
	path    string
	release func() // frees the slot the upload holds
}

func (s *staged) cleanup() {
	if s.path != "" {
		os.Remove(s.path)
	}
	if s.release != nil {
		s.release()
	}
}

// v1UploadMedia stores an image or starts processing a video. The file is
// a multipart "file" part (with optional "alt" and, for a video, "preset"
// parts, in any order) or the whole body, with alt and preset in the
// query.
func (a *API) v1UploadMedia(w http.ResponseWriter, r *http.Request) {
	if a.Store == nil {
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "no database")
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(imageUploadDeadline))
	if r.ContentLength > maxVideoUploadBytes {
		problem(w, r, http.StatusRequestEntityTooLarge, codeTooLarge, "uploads are at most 1 GiB")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxVideoUploadBytes)
	q := r.URL.Query()
	alt, preset := q.Get("alt"), q.Get("preset")
	if preset != "" && !transcode.IsVideoPreset(preset) {
		invalid(w, r, []fieldError{{Parameter: "preset", Code: "invalid", Detail: "one of " + strings.Join(transcode.VideoPresetNames, ", ")}})
		return
	}
	var file *staged
	defer func() {
		if file != nil {
			file.cleanup()
		}
	}()
	ct, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch {
	case ct == "multipart/form-data":
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				a.uploadReadProblem(w, r, err, "uploads are at most 1 GiB")
				return
			}
			switch part.FormName() {
			case "file":
				if file != nil {
					invalid(w, r, []fieldError{{Pointer: "/file", Code: "too_many", Detail: "one file per upload"}})
					return
				}
				if file = a.receiveUpload(w, r, part.Header.Get("Content-Type"), part); file == nil {
					return
				}
			case "alt", "preset":
				b, err := io.ReadAll(io.LimitReader(part, 64<<10))
				if err != nil {
					a.uploadReadProblem(w, r, err, "uploads are at most 1 GiB")
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
		if file = a.receiveUpload(w, r, ct, r.Body); file == nil {
			return
		}
	default:
		problem(w, r, http.StatusUnsupportedMediaType, codeUnsupportedMediaType,
			"send multipart/form-data with a file part, or the file itself with its image/* or video/* type")
		return
	}
	var errs []fieldError
	if msg := altProblem(alt); msg != "" {
		errs = append(errs, fieldError{Pointer: "/alt", Code: "invalid", Detail: msg})
	}
	if preset != "" && !transcode.IsVideoPreset(preset) {
		errs = append(errs, fieldError{Pointer: "/preset", Code: "invalid", Detail: "one of " + strings.Join(transcode.VideoPresetNames, ", ")})
	}
	if len(errs) > 0 {
		invalid(w, r, errs)
		return
	}
	pr, _ := principalFrom(r.Context())
	if file.path != "" {
		a.startVideo(w, r, pr.OwnerID, file, alt, preset)
		return
	}
	res, err := a.media.Process(r.Context(), file.data, file.mime)
	if err != nil {
		slog.WarnContext(r.Context(), "v1: store image", "err", err)
		if strings.Contains(err.Error(), "heic convert") {
			invalid(w, r, []fieldError{{Pointer: "/file", Code: "unreadable", Detail: "the HEIC image could not be converted"}})
			return
		}
		// The cause (an internal address, storage's own words) goes to
		// the log, not to the caller.
		problem(w, r, http.StatusBadGateway, codeUpstream, "media storage refused the file or could not be reached; the server log has details")
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

// receiveUpload reads the file: an image into memory, a video to a
// temporary file. On a problem it answers it and returns nil.
func (a *API) receiveUpload(w http.ResponseWriter, r *http.Request, declared string, body io.Reader) *staged {
	br := bufio.NewReaderSize(body, 512)
	head, _ := br.Peek(512)
	mt := sniffType(declared, head)
	switch {
	case uploadImageTypes[mt]:
		if a.media == nil {
			problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "media storage is not configured")
			return nil
		}
		select {
		case imageReadSem <- struct{}{}:
		case <-r.Context().Done():
			problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "the request ended")
			return nil
		}
		f := &staged{mime: mt, release: func() { <-imageReadSem }}
		data, err := io.ReadAll(io.LimitReader(br, maxUploadRequestBytes+1))
		if err != nil {
			f.cleanup()
			a.uploadReadProblem(w, r, err, "uploads are at most 1 GiB")
			return nil
		}
		if int64(len(data)) > maxUploadRequestBytes {
			f.cleanup()
			problem(w, r, http.StatusRequestEntityTooLarge, codeTooLarge, "images are at most 64 MiB")
			return nil
		}
		f.data = data
		return f
	case strings.HasPrefix(mt, "video/"):
		if a.VideoJobs == nil {
			problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "video processing is not available (ffmpeg is missing)")
			return nil
		}
		busy := func() *staged {
			w.Header().Set("Retry-After", "60")
			problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "the video queue is full; try again when a video has finished")
			return nil
		}
		if a.VideoJobs.Full() {
			return busy()
		}
		// A slot is held while the file is received, so parallel uploads
		// can't fill the disk before the queue turns them away.
		select {
		case videoUploadSem <- struct{}{}:
		default:
			return busy()
		}
		f := &staged{mime: mt, release: func() { <-videoUploadSem }}
		// The server's timeouts are for ordinary requests; a large video
		// needs minutes.
		rc := http.NewResponseController(w)
		deadline := time.Now().Add(videoUploadDeadline)
		_ = rc.SetReadDeadline(deadline)
		_ = rc.SetWriteDeadline(deadline.Add(time.Minute))
		tmp, err := os.CreateTemp(a.videoWorkdir(), "upload-*.video")
		if err != nil {
			f.cleanup()
			slog.ErrorContext(r.Context(), "v1: stage video", "err", err)
			problem(w, r, http.StatusInternalServerError, codeInternal, "staging the upload failed")
			return nil
		}
		f.path = tmp.Name()
		_, err = io.Copy(tmp, br)
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			f.cleanup()
			var pe *fs.PathError
			if errors.As(err, &pe) && (pe.Op == "write" || pe.Op == "close") {
				slog.ErrorContext(r.Context(), "v1: stage video", "err", err)
				problem(w, r, http.StatusInsufficientStorage, codeNoSpace, "the server has no room for the upload")
				return nil
			}
			a.uploadReadProblem(w, r, err, "uploads are at most 1 GiB")
			return nil
		}
		return f
	}
	invalid(w, r, []fieldError{{Pointer: "/file", Code: "unsupported_type",
		Detail: "a JPEG, PNG, GIF, WebP or HEIC image, or a video; not " + mt}})
	return nil
}

// startVideo hands a received video to the video runner and records it.
func (a *API) startVideo(w http.ResponseWriter, r *http.Request, owner string, file *staged, alt, preset string) {
	if preset == "" {
		preset = "1080p"
	}
	path := file.path
	file.path = "" // the runner owns the file now, and removes it
	jobID, err := a.VideoJobs.Submit(context.WithoutCancel(r.Context()), path, preset)
	if err != nil {
		w.Header().Set("Retry-After", "60")
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "the video queue is full; try again when a video has finished")
		return
	}
	m := &store.MediaObject{OwnerID: owner, Status: store.MediaProcessing, Mime: file.mime, Alt: alt, JobID: jobID}
	if err := a.Store.CreateMediaObject(r.Context(), m); err != nil {
		// The job runs on with nothing pointing at it; its file in media
		// storage is unreferenced, as an abandoned upload's would be.
		slog.ErrorContext(r.Context(), "v1: save media", "err", err, "job", jobID)
		problem(w, r, http.StatusInternalServerError, codeInternal, "saving the media failed")
		return
	}
	go a.watchVideo(*m)
	w.Header().Set("Location", "/v1/media/"+m.ID)
	writeV1(w, http.StatusAccepted, a.v1MediaBody(m))
}

func (a *API) uploadReadProblem(w http.ResponseWriter, r *http.Request, err error, limit string) {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		problem(w, r, http.StatusRequestEntityTooLarge, codeTooLarge, limit)
		return
	}
	problem(w, r, http.StatusBadRequest, codeUnreadableUpload, "the upload could not be read: "+err.Error())
}

// v1CompressMedia re-encodes an image with a preset and answers the bytes;
// nothing is stored. "convert" turns HEIC into JPEG.
func (a *API) v1CompressMedia(w http.ResponseWriter, r *http.Request) {
	const limit = "images are at most 64 MiB"
	if r.ContentLength > maxUploadRequestBytes+1<<20 {
		problem(w, r, http.StatusRequestEntityTooLarge, codeTooLarge, limit)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadRequestBytes+1<<20)
	ct, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	preset := r.URL.Query().Get("preset")
	if ct != "multipart/form-data" && !strings.HasPrefix(ct, "image/") && ct != "application/octet-stream" {
		problem(w, r, http.StatusUnsupportedMediaType, codeUnsupportedMediaType,
			"send multipart/form-data with a file part, or the image itself with its image/* type")
		return
	}
	// The slot bounds the bodies held in memory as well as the work.
	select {
	case compressSem <- struct{}{}:
		defer func() { <-compressSem }()
	case <-r.Context().Done():
		return
	}
	var mt string
	var body []byte
	read := func(declared string, rd io.Reader) bool {
		br := bufio.NewReaderSize(rd, 512)
		head, _ := br.Peek(512)
		mt = sniffType(declared, head)
		b, err := io.ReadAll(br)
		if err != nil {
			a.uploadReadProblem(w, r, err, limit)
			return false
		}
		body = b
		return true
	}
	if ct == "multipart/form-data" {
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				a.uploadReadProblem(w, r, err, limit)
				return
			}
			switch part.FormName() {
			case "file":
				if !read(part.Header.Get("Content-Type"), part) {
					return
				}
			case "preset":
				b, _ := io.ReadAll(io.LimitReader(part, 256))
				preset = string(b)
			default:
				_, _ = io.Copy(io.Discard, part)
			}
		}
	} else if !read(ct, r.Body) {
		return
	}
	var errs []fieldError
	switch {
	case body == nil:
		errs = append(errs, fieldError{Pointer: "/file", Code: "required", Detail: "send the image as a multipart part named file"})
	case !uploadImageTypes[mt]:
		errs = append(errs, fieldError{Pointer: "/file", Code: "unsupported_type", Detail: "a JPEG, PNG, GIF, WebP or HEIC image; not " + mt})
	}
	params2, ok := transcode.PresetParams(preset)
	if !ok {
		errs = append(errs, fieldError{Pointer: "/preset", Code: "invalid", Detail: "one of " + strings.Join(transcode.ImagePresetNames, ", ")})
	}
	if len(errs) > 0 {
		invalid(w, r, errs)
		return
	}
	res, err := transcode.Image(body, mt, params2)
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
