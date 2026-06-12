// Package videojob runs the asynchronous attach-time video pipeline:
// probe → normalize (ffmpeg) → stream to Blossom. One transcode at a time
// (this host is small); an in-memory registry serves the composer's 1 s poll.
// Jobs do not survive restarts (spec §3.1): the poll 404s and the composer
// asks the user to re-attach. The workdir must absorb ~2x the output size
// transiently (ffmpeg's +faststart second pass writes an adjacent temp file).
package videojob

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/geofox/publisher/internal/media"
	"github.com/geofox/publisher/internal/transcode"
)

const (
	StateQueued      = "queued"
	StateProbing     = "probing"
	StateTranscoding = "transcoding"
	StateUploading   = "uploading"
	StateDone        = "done"
	StateError       = "error"
)

// storeTimeout bounds the Blossom streaming upload: the streaming HTTP client
// deliberately has no overall timeout (a 1 GB body fits no fixed budget), so
// the job provides the ceiling instead.
const storeTimeout = 15 * time.Minute

// Transcoder is the seam between the job engine and ffmpeg, fake-able in
// tests (and in the API tests one layer up).
type Transcoder interface {
	Probe(ctx context.Context, path string) (transcode.VideoMeta, error)
	Normalize(ctx context.Context, in, out string, p transcode.NormParams, progress func(float64)) error
}

// Storer is the seam to media.Pipeline.ProcessFile.
type Storer interface {
	ProcessFile(ctx context.Context, path, mime, dim string, durationSecs int64, progress func(float64)) (media.Result, error)
}

type Job struct {
	ID    string        `json:"job_id"`
	State string        `json:"state"`
	Pct   float64       `json:"pct"`
	Err   string        `json:"error,omitempty"`
	Media *media.Result `json:"media,omitempty"`
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
// and starts it asynchronously. The caller hands over ownership of inPath —
// the job deletes it when finished either way.
func (r *Runner) Submit(ctx context.Context, inPath, preset string) string {
	id := newID()
	r.mu.Lock()
	r.jobs[id] = &Job{ID: id, State: StateQueued}
	r.mu.Unlock()
	go r.run(context.WithoutCancel(ctx), id, inPath, preset)
	return id
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

	fail := func(err error) { r.set(id, func(j *Job) { j.State, j.Err = StateError, err.Error() }) }

	r.set(id, func(j *Job) { j.State = StateProbing })
	meta, err := r.tc.Probe(ctx, inPath)
	if err != nil {
		fail(fmt.Errorf("probe: %w", err))
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
	r.set(id, func(j *Job) { j.State, j.Pct, j.Media = StateDone, 1, &res })
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "vj_" + hex.EncodeToString(b)
}

// SweepWorkdir removes leftovers from jobs killed by a restart (spec §7) —
// inputs, outputs, and ffmpeg faststart temp files alike. Called once at
// startup; the workdir is DEDICATED to this runner (never a shared /tmp).
func (r *Runner) SweepWorkdir() {
	ents, err := os.ReadDir(r.workdir)
	if err != nil {
		return
	}
	for _, e := range ents {
		os.Remove(filepath.Join(r.workdir, e.Name()))
	}
}
