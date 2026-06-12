// Package videojob runs the asynchronous attach-time video pipeline:
// probe → normalize (ffmpeg) → stream to Blossom. One transcode at a time
// (this host is small); an in-memory registry serves the composer's 1 s poll.
// Jobs do not survive restarts (spec §3.1): the poll 404s and the composer
// asks the user to re-attach. The workdir must absorb ~2x the output size
// transiently (ffmpeg's +faststart second pass writes an adjacent temp file).
// Shutdown abort is intentionally absent: the daemon runs in a container whose
// teardown kills ffmpeg with the process, and SweepWorkdir reclaims disk at
// next start.
package videojob

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/geofox/publisher/internal/media"
	"github.com/geofox/publisher/internal/transcode"
)

// DefaultWorkdir is the fallback when VIDEO_WORKDIR is unset. Dedicated dir —
// never bare os.TempDir(): SweepWorkdir deletes everything in it, and dev
// /tmp is often RAM-backed tmpfs.
func DefaultWorkdir() string { return filepath.Join(os.TempDir(), "publisher-video") }

const (
	StateQueued      = "queued"
	StateProbing     = "probing"
	StateTranscoding = "transcoding"
	StateUploading   = "uploading"
	StateDone        = "done"
	StateError       = "error"
)

const maxInputDuration = 20 * time.Minute

const (
	// storeTimeout bounds the Blossom streaming upload: the streaming HTTP client
	// deliberately has no overall timeout (a 1 GB body fits no fixed budget), so
	// the job provides the ceiling instead.
	storeTimeout = 15 * time.Minute

	// maxPending bounds simultaneous on-disk input files (each up to 1 GB) —
	// queued jobs hold their uploads on disk while parked on the semaphore.
	maxPending = 3

	// evictAfter prunes terminal jobs from the registry (the composer stops
	// polling within seconds; an hour is ample for debugging).
	evictAfter = time.Hour
)

// ErrBusy rejects a Submit when maxPending jobs are queued/running; the
// endpoint maps it to 429.
var ErrBusy = errors.New("videojob: transcode queue is full")

// Transcoder is the seam between the job engine and ffmpeg, fake-able in
// tests (and in the API tests one layer up).
type Transcoder interface {
	Probe(ctx context.Context, path string) (transcode.VideoMeta, error)
	Normalize(ctx context.Context, in, out string, p transcode.NormParams, progress func(float64)) error
	ExtractPoster(ctx context.Context, in, out string, atSecs float64) error
}

// Storer is the seam to media.Pipeline.ProcessFile and Process.
type Storer interface {
	ProcessFile(ctx context.Context, path, mime, dim string, durationSecs int64, progress func(float64)) (media.Result, error)
	Process(ctx context.Context, body []byte, mime string) (media.Result, error)
}

type Job struct {
	ID     string        `json:"job_id"`
	State  string        `json:"state"`
	Pct    float64       `json:"pct"`
	Err    string        `json:"error,omitempty"`
	Media  *media.Result `json:"media,omitempty"`
	doneAt time.Time
}

type Runner struct {
	tc      Transcoder
	store   Storer
	workdir string

	mu   sync.Mutex
	jobs map[string]*Job

	sem chan struct{} // 1 slot: one ffmpeg at a time
}

func NewRunner(tc Transcoder, store Storer, workdir string) *Runner {
	return &Runner{tc: tc, store: store, workdir: workdir,
		jobs: make(map[string]*Job), sem: make(chan struct{}, 1)}
}

// Submit registers a job for the (already fully received) upload at inPath
// and starts it asynchronously. Submit CONSUMES inPath unconditionally — on
// rejection it is deleted, so callers never branch on cleanup. Terminal jobs
// older than evictAfter are pruned here (the only place the registry grows).
func (r *Runner) Submit(ctx context.Context, inPath, preset string) (string, error) {
	r.mu.Lock()
	pending := 0
	for jid, j := range r.jobs {
		switch j.State {
		case StateDone, StateError:
			if time.Since(j.doneAt) > evictAfter {
				delete(r.jobs, jid)
			}
		default:
			pending++
		}
	}
	if pending >= maxPending {
		r.mu.Unlock()
		os.Remove(inPath)
		return "", ErrBusy
	}
	id := newID()
	r.jobs[id] = &Job{ID: id, State: StateQueued}
	r.mu.Unlock()
	go r.run(context.WithoutCancel(ctx), id, inPath, preset)
	return id, nil
}

func (r *Runner) Get(id string) (Job, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[id]
	if !ok {
		return Job{}, false
	}
	return *j, true
}

// Full reports whether Submit would currently reject with ErrBusy — a cheap
// pre-check so the endpoint can 429 BEFORE receiving a 1 GB body. Best-effort
// (TOCTOU vs. Submit is harmless: Submit re-checks authoritatively).
func (r *Runner) Full() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	pending := 0
	for _, j := range r.jobs {
		if j.State != StateDone && j.State != StateError {
			pending++
		}
	}
	return pending >= maxPending
}

func (r *Runner) set(id string, f func(*Job)) {
	r.mu.Lock()
	if j, ok := r.jobs[id]; ok {
		f(j)
	}
	r.mu.Unlock()
}

func (r *Runner) run(ctx context.Context, id, inPath, preset string) {
	defer os.Remove(inPath)
	r.sem <- struct{}{}
	defer func() { <-r.sem }()

	fail := func(err error) {
		r.set(id, func(j *Job) { j.State, j.Err, j.doneAt = StateError, err.Error(), time.Now() })
	}

	r.set(id, func(j *Job) { j.State = StateProbing })
	pctx, pcancel := context.WithTimeout(ctx, time.Minute)
	meta, err := r.tc.Probe(pctx, inPath)
	pcancel()
	if err != nil {
		fail(fmt.Errorf("probe: %w", err))
		return
	}
	if meta.DurationSecs > maxInputDuration.Seconds() {
		fail(fmt.Errorf("probe: video is %.0f min — longer than the %d-minute limit", meta.DurationSecs/60, int(maxInputDuration.Minutes())))
		return
	}

	w, h := transcode.FitVideoDims(meta.W, meta.H, preset)
	fps := math.Min(meta.FPS, 60)
	out := filepath.Join(r.workdir, id+".mp4")
	defer os.Remove(out)

	// ffmpeg gets a duration-scaled hard timeout: max(10 min, 4× realtime).
	// The faststart second pass happens after progress ~1.0 (UI: "finalizing"
	// plateau) and is included in this budget.
	budget := time.Duration(math.Max(600, meta.DurationSecs*4)) * time.Second
	tctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	r.set(id, func(j *Job) { j.State = StateTranscoding; j.Pct = 0 })
	err = r.tc.Normalize(tctx, inPath, out,
		transcode.NormParams{W: w, H: h, FPS: fps, HasAudio: meta.HasAudio, DurationSecs: meta.DurationSecs},
		func(f float64) { r.set(id, func(j *Job) { j.Pct = f }) })
	if err != nil {
		fail(fmt.Errorf("transcode: %w", err))
		return
	}

	// Poster: best-effort single-frame JPEG — a video without a poster is
	// strictly better than a failed upload, so every error here only warns.
	// One fresh 1-minute ctx bounds extract AND upload: tctx's duration-scaled
	// budget may be nearly spent after a long transcode, and the upload rides
	// the no-overall-timeout streaming client (storeTimeout philosophy: the
	// job provides the ceiling) — unbounded, a stall here would wedge the
	// single transcode slot forever.
	posterPath := out + ".poster.jpg"
	defer os.Remove(posterPath)
	var posterURL string
	pjctx, pjcancel := context.WithTimeout(ctx, time.Minute)
	if perr := r.tc.ExtractPoster(pjctx, out, posterPath, transcode.PosterAt(meta.DurationSecs)); perr != nil {
		slog.Warn("video poster extract failed", "job", id, "err", perr)
	} else if pb, rerr := os.ReadFile(posterPath); rerr != nil {
		slog.Warn("video poster read failed", "job", id, "err", rerr)
	} else if pres, uerr := r.store.Process(pjctx, pb, "image/jpeg"); uerr != nil {
		slog.Warn("video poster upload failed", "job", id, "err", uerr)
	} else {
		posterURL = pres.URL
	}
	pjcancel()

	r.set(id, func(j *Job) { j.State = StateUploading; j.Pct = 0 })
	sctx, scancel := context.WithTimeout(ctx, storeTimeout)
	defer scancel()
	dim := fmt.Sprintf("%dx%d", w, h)
	dur := int64(math.Ceil(meta.DurationSecs))
	res, err := r.store.ProcessFile(sctx, out, "video/mp4", dim, dur,
		func(f float64) { r.set(id, func(j *Job) { j.Pct = f }) })
	if err != nil {
		fail(fmt.Errorf("store: %w", err))
		return
	}
	if posterURL != "" {
		res.PosterURL = posterURL
		// Rebuild the payload imeta with the poster (ProcessFile cannot know
		// it); dispatch composes identically from the store row at post time.
		res.Imeta = media.ImetaTag(res.URL, res.Mime, res.SHA256, res.Dim, "", posterURL)
	}
	r.set(id, func(j *Job) { j.State, j.Pct, j.Media, j.doneAt = StateDone, 1, &res, time.Now() })
}

// newID generates a random job identifier. _, _ = rand.Read(b) never errors
// since Go 1.24; a failure indicates an OS entropy crisis and would crash.
func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "vj_" + hex.EncodeToString(b)
}

// SweepWorkdir removes leftovers from jobs killed by a restart (spec §7) —
// inputs, outputs, and ffmpeg faststart temp files alike. Called once at
// startup; the workdir is DEDICATED to this runner (never a shared /tmp).
// flat dir assumed; subdirectories are skipped (os.Remove fails non-empty, ignored).
func (r *Runner) SweepWorkdir() {
	ents, err := os.ReadDir(r.workdir)
	if err != nil {
		return
	}
	for _, e := range ents {
		os.Remove(filepath.Join(r.workdir, e.Name()))
	}
}
