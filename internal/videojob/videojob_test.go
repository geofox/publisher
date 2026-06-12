package videojob

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/media"
	"github.com/geofox/publisher/internal/transcode"
)

type fakeTC struct {
	meta     transcode.VideoMeta
	probeErr error
	normErr  error
}

func (f fakeTC) Probe(ctx context.Context, path string) (transcode.VideoMeta, error) {
	return f.meta, f.probeErr
}
func (f fakeTC) Normalize(ctx context.Context, in, out string, p transcode.NormParams, progress func(float64)) error {
	if f.normErr != nil {
		return f.normErr
	}
	if progress != nil {
		progress(0.5)
		progress(1)
	}
	return os.WriteFile(out, []byte("normalized-bytes"), 0o644)
}

type fakeStore struct {
	res media.Result
	err error
}

func (f fakeStore) ProcessFile(ctx context.Context, path, mime, dim string, dur int64, progress func(float64)) (media.Result, error) {
	if progress != nil {
		progress(1)
	}
	return f.res, f.err
}

func newRunner(t *testing.T, tc Transcoder, st Storer) *Runner {
	t.Helper()
	return NewRunner(tc, st, t.TempDir())
}

func waitState(t *testing.T, r *Runner, id, want string) Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		j, ok := r.Get(id)
		if !ok {
			t.Fatalf("job %s vanished", id)
		}
		if j.State == want {
			return j
		}
		if j.State == StateError && want != StateError {
			t.Fatalf("job errored: %s", j.Err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job never reached %s", want)
	return Job{}
}

func submit(t *testing.T, r *Runner, preset string) string {
	t.Helper()
	in := filepath.Join(t.TempDir(), "in.mov")
	if err := os.WriteFile(in, []byte("raw"), 0o644); err != nil {
		t.Fatal(err)
	}
	return r.Submit(context.Background(), in, preset)
}

func TestJobHappyPath(t *testing.T) {
	want := media.Result{URL: "https://b/v", SHA256: "ss", Mime: "video/mp4", Dim: "1280x720", DurationSecs: 3}
	r := newRunner(t,
		fakeTC{meta: transcode.VideoMeta{W: 1920, H: 1080, DurationSecs: 2.5, FPS: 30, VCodec: "hevc", HasAudio: true}},
		fakeStore{res: want})
	id := submit(t, r, "720p")
	j := waitState(t, r, id, StateDone)
	if j.Media == nil || j.Media.URL != "https://b/v" {
		t.Fatalf("media = %+v", j.Media)
	}
	if j.Pct < 0.99 {
		t.Fatalf("pct = %f", j.Pct)
	}
}

func TestJobProbeError(t *testing.T) {
	r := newRunner(t, fakeTC{probeErr: errors.New("no video stream")}, fakeStore{})
	id := submit(t, r, "1080p")
	j := waitState(t, r, id, StateError)
	if j.Err == "" {
		t.Fatal("error message must surface")
	}
}

type slowTC struct{ d time.Duration }

func (s slowTC) Probe(ctx context.Context, path string) (transcode.VideoMeta, error) {
	return transcode.VideoMeta{W: 100, H: 100, DurationSecs: 1, FPS: 30}, nil
}
func (s slowTC) Normalize(ctx context.Context, in, out string, p transcode.NormParams, progress func(float64)) error {
	time.Sleep(s.d)
	return os.WriteFile(out, []byte("n"), 0o644)
}

func TestJobsSerialize(t *testing.T) {
	r := newRunner(t, slowTC{d: 300 * time.Millisecond}, fakeStore{res: media.Result{URL: "u"}})
	id1 := submit(t, r, "1080p")
	id2 := submit(t, r, "1080p")
	time.Sleep(50 * time.Millisecond)
	j2, _ := r.Get(id2)
	if j2.State != StateQueued {
		t.Fatalf("second job state = %s, want queued", j2.State)
	}
	waitState(t, r, id1, StateDone)
	waitState(t, r, id2, StateDone)
}

func TestJobTempCleanup(t *testing.T) {
	work := t.TempDir()
	r := NewRunner(fakeTC{meta: transcode.VideoMeta{W: 10, H: 10, DurationSecs: 1, FPS: 30}},
		fakeStore{res: media.Result{URL: "u"}}, work)
	id := submit(t, r, "1080p")
	waitState(t, r, id, StateDone)
	ents, err := os.ReadDir(work)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Fatalf("workdir not cleaned: %v", ents)
	}
}

func TestJobInputRemovedOnError(t *testing.T) {
	r := newRunner(t, fakeTC{probeErr: errors.New("bad")}, fakeStore{})
	in := filepath.Join(t.TempDir(), "in.mov")
	if err := os.WriteFile(in, []byte("raw"), 0o644); err != nil {
		t.Fatal(err)
	}
	id := r.Submit(context.Background(), in, "1080p")
	waitState(t, r, id, StateError)
	if _, err := os.Stat(in); !os.IsNotExist(err) {
		t.Fatal("input temp must be removed on error too")
	}
}

func TestSweepWorkdir(t *testing.T) {
	work := t.TempDir()
	os.WriteFile(filepath.Join(work, "leftover.mp4"), []byte("x"), 0o644)
	r := NewRunner(fakeTC{}, fakeStore{}, work)
	r.SweepWorkdir()
	ents, _ := os.ReadDir(work)
	if len(ents) != 0 {
		t.Fatalf("sweep left %v", ents)
	}
}
