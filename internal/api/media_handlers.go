package api

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/geofox/publisher/internal/httpx"
	"github.com/geofox/publisher/internal/transcode"
	"github.com/geofox/publisher/internal/videojob"
)

// ─── POST /api/media/compress ────────────────────────────────────────────
//
// On-demand composer compression: multipart {file, preset} in, re-encoded
// image bytes out. Stateless — nothing is stored; the client swaps its
// in-memory File for the response, so the compressed version is what later
// rides /api/post and becomes the canonical Blossom object. Also serves the
// HEIC attach-time auto-conversion (preset "convert").
func (a *API) handleCompressMedia(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength > maxUploadRequestBytes {
		httpx.WriteError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("upload exceeds %d bytes (%d MB) — Content-Length: %d",
				maxUploadRequestBytes, maxUploadRequestBytes>>20, r.ContentLength))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadRequestBytes)
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
	params, ok := transcode.PresetParams(r.FormValue("preset"))
	if !ok {
		httpx.WriteError(w, http.StatusBadRequest, "unknown preset")
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
	select {
	case compressSem <- struct{}{}:
		defer func() { <-compressSem }()
	case <-r.Context().Done():
		return // client gave up while queued (abort on preset switch)
	}
	if r.Context().Err() != nil {
		return
	}
	res, err := transcode.Image(body, fh.Header.Get("Content-Type"), params)
	if err != nil {
		// Unconvertible input, not a server fault: 422
		// so the composer flashes the reason and keeps the original file.
		httpx.WriteError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if !res.Changed {
		// Presets force JPEG, so decodable input always re-encodes; an
		// unchanged result means transcode's passthrough fired — either the
		// image was not decodable, or it exceeded the 100 MP pixel cap
		// (pixel presets carry no MaxBytes, so no transcode error is returned).
		// Never hand the original back as if it were compressed.
		httpx.WriteError(w, http.StatusUnprocessableEntity, "image could not be compressed (not decodable, or over the 100 MP cap)")
		return
	}
	slog.Info("compress_media", "preset", r.FormValue("preset"),
		"in_bytes", len(body), "out_bytes", len(res.Bytes), "mime", res.Mime)
	w.Header().Set("Content-Type", res.Mime)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	w.Write(res.Bytes)
}

// ─── POST /api/media/video ──────────────────────────────────────────────
//
// Async attach-time video ingest: the multipart body STREAMS to a temp file
// in the dedicated video workdir (1 GB cap, zero RAM buffering via
// MultipartReader), then a videojob runs probe → ffmpeg normalize → Blossom,
// polled by the composer at GET /api/media/video/{id}. Preset rides the query
// string so parts are never buffered while hunting for a field.
func (a *API) handleVideoUpload(w http.ResponseWriter, r *http.Request) {
	if a.VideoJobs == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "video pipeline not configured (ffmpeg missing)")
		return
	}
	// The server's global Read/WriteTimeouts (60s/120s) are sized for normal
	// API calls; a 1 GB upload needs minutes. Extend the deadlines for this
	// request only (≈4.7 Mbit/s floor — still a slow-loris bound, and the
	// endpoint is session-gated). ErrNotSupported (httptest) is ignored.
	rc := http.NewResponseController(w)
	d := time.Now().Add(30 * time.Minute)
	_ = rc.SetReadDeadline(d)
	_ = rc.SetWriteDeadline(d.Add(time.Minute))

	if a.VideoJobs.Full() {
		httpx.WriteError(w, http.StatusTooManyRequests, "transcode queue is full — wait for the current video to finish")
		return
	}
	if r.ContentLength > maxVideoUploadBytes {
		httpx.WriteError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("upload exceeds %d bytes (%d MB)", maxVideoUploadBytes, maxVideoUploadBytes>>20))
		return
	}
	preset := r.URL.Query().Get("preset")
	if preset == "" {
		preset = "1080p"
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxVideoUploadBytes)
	mr, err := r.MultipartReader()
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "multipart: "+err.Error())
		return
	}
	var tmpPath string
	for {
		part, perr := mr.NextPart()
		if perr == io.EOF {
			break
		}
		if perr != nil {
			var mbe *http.MaxBytesError
			if errors.As(perr, &mbe) {
				httpx.WriteError(w, http.StatusRequestEntityTooLarge, "upload exceeds cap")
			} else {
				httpx.WriteError(w, http.StatusBadRequest, "multipart: "+perr.Error())
			}
			return
		}
		if part.FormName() != "file" {
			io.Copy(io.Discard, part)
			continue
		}
		dir := a.videoWorkdir()
		tmp, terr := os.CreateTemp(dir, "upload-*.video")
		if terr != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "tmp: "+terr.Error())
			return
		}
		_, cerr := io.Copy(tmp, part)
		tmp.Close()
		if cerr != nil {
			os.Remove(tmp.Name())
			var pe *fs.PathError
			if errors.As(cerr, &pe) && pe.Op == "write" {
				// Server-side staging failure (disk full etc.) — never blame
				// the client, never leak the filesystem path.
				slog.Error("video upload staging failed", "err", cerr)
				httpx.WriteError(w, http.StatusInsufficientStorage, "server storage error while staging upload")
				return
			}
			var mbe *http.MaxBytesError
			if errors.As(cerr, &mbe) {
				httpx.WriteError(w, http.StatusRequestEntityTooLarge, "upload exceeds cap")
			} else {
				httpx.WriteError(w, http.StatusBadRequest, "read: "+cerr.Error())
			}
			return
		}
		tmpPath = tmp.Name()
		break
	}
	if tmpPath == "" {
		httpx.WriteError(w, http.StatusBadRequest, "missing 'file' part")
		return
	}
	id, err := a.VideoJobs.Submit(r.Context(), tmpPath, preset)
	if err != nil {
		// ErrBusy is the only Submit error; the input was already consumed.
		httpx.WriteError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	slog.Info("video_job_submitted", "job", id, "preset", preset)
	httpx.WriteJSON(w, http.StatusAccepted, map[string]string{"job_id": id})
}

// videoWorkdir is the DEDICATED dir for uploads and transcode outputs — never
// bare os.TempDir(): the runner's startup sweep deletes everything in its
// workdir, and dev /tmp is RAM-backed tmpfs. Configurable via VIDEO_WORKDIR.
func (a *API) videoWorkdir() string {
	d := a.VideoWorkdir
	if d == "" {
		d = videojob.DefaultWorkdir()
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		slog.Error("video workdir unavailable", "path", d, "err", err)
	}
	return d
}

func (a *API) handleVideoJob(w http.ResponseWriter, r *http.Request) {
	if a.VideoJobs == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "video pipeline not configured")
		return
	}
	j, ok := a.VideoJobs.Get(r.PathValue("id"))
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, "unknown job (restarted? re-attach the video)")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, j)
}

// compressSem bounds concurrent transcodes: each can transiently allocate
// hundreds of MB (decode + flatten + orientation copies of a ~100MP image),
// and this host has no swap. Two slots keep the composer snappy while making
// pile-ups impossible.
var compressSem = make(chan struct{}, 2)
