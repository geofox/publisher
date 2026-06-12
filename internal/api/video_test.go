package api

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/media"
	"github.com/geofox/publisher/internal/transcode"
	"github.com/geofox/publisher/internal/videojob"
)

type vtFake struct{}

func (vtFake) Probe(ctx context.Context, path string) (transcode.VideoMeta, error) {
	return transcode.VideoMeta{W: 320, H: 240, DurationSecs: 2, FPS: 30, VCodec: "hevc", HasAudio: true}, nil
}
func (vtFake) Normalize(ctx context.Context, in, out string, p transcode.NormParams, progress func(float64)) error {
	return os.WriteFile(out, []byte("norm"), 0o644)
}

type vsFake struct{}

func (vsFake) ProcessFile(ctx context.Context, path, mime, dim string, dur int64, progress func(float64)) (media.Result, error) {
	return media.Result{URL: "https://b/vid", SHA256: "ff", Mime: "video/mp4", Dim: dim, DurationSecs: dur, Size: 4}, nil
}

func newVideoAPI(t *testing.T) *API {
	t.Helper()
	a := &API{}
	a.VideoJobs = videojob.NewRunner(vtFake{}, vsFake{}, t.TempDir())
	a.VideoWorkdir = t.TempDir()
	return a
}

func postVideo(t *testing.T, a *API, body []byte, preset string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "clip.mov")
	if err != nil {
		t.Fatal(err)
	}
	fw.Write(body)
	mw.Close()
	req := httptest.NewRequest("POST", "/api/media/video?preset="+preset, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)
	return rec
}

func TestVideoUploadJobLifecycle(t *testing.T) {
	a := newVideoAPI(t)
	rec := postVideo(t, a, []byte("pretend-video-bytes"), "720p")
	if rec.Code != 202 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.JobID == "" {
		t.Fatalf("bad job response: %s", rec.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec2 := httptest.NewRecorder()
		a.Routes().ServeHTTP(rec2, httptest.NewRequest("GET", "/api/media/video/"+out.JobID, nil))
		if rec2.Code != 200 {
			t.Fatalf("poll status %d", rec2.Code)
		}
		var j struct {
			State string          `json:"state"`
			Media json.RawMessage `json:"media"`
		}
		json.Unmarshal(rec2.Body.Bytes(), &j)
		if j.State == "done" {
			if !bytes.Contains(j.Media, []byte("https://b/vid")) {
				t.Fatalf("media = %s", j.Media)
			}
			return
		}
		if j.State == "error" {
			t.Fatalf("job error: %s", rec2.Body.String())
		}
		if time.Now().After(deadline) {
			t.Fatal("job never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestVideoUploadUnknownJob404(t *testing.T) {
	a := newVideoAPI(t)
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/media/video/vj_nope", nil))
	if rec.Code != 404 {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestVideoUploadRejectsOversize(t *testing.T) {
	a := newVideoAPI(t)
	req := httptest.NewRequest("POST", "/api/media/video?preset=1080p", bytes.NewReader(nil))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	req.ContentLength = 1<<30 + 2<<20 // over maxVideoUploadBytes (cap + 1 MiB slack)
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)
	if rec.Code != 413 {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestVideoUploadRejectsWhenDisabled(t *testing.T) {
	a := &API{} // VideoJobs nil = ffmpeg not configured
	rec := postVideo(t, a, []byte("x"), "1080p")
	if rec.Code != 503 {
		t.Fatalf("status %d, want 503 when video pipeline disabled", rec.Code)
	}
}

// gateVT blocks Normalize until released — lets tests hold jobs in flight.
type gateVT struct{ release chan struct{} }

func (gateVT) Probe(ctx context.Context, path string) (transcode.VideoMeta, error) {
	return transcode.VideoMeta{W: 320, H: 240, DurationSecs: 1, FPS: 30}, nil
}
func (g gateVT) Normalize(ctx context.Context, in, out string, p transcode.NormParams, _ func(float64)) error {
	<-g.release
	return os.WriteFile(out, []byte("n"), 0o644)
}

func TestVideoUploadQueueFull429(t *testing.T) {
	g := gateVT{release: make(chan struct{})}
	a := &API{}
	a.VideoJobs = videojob.NewRunner(g, vsFake{}, t.TempDir())
	a.VideoWorkdir = t.TempDir()
	for i := 0; i < 3; i++ {
		if rec := postVideo(t, a, []byte("v"), "1080p"); rec.Code != 202 {
			t.Fatalf("submit %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	rec := postVideo(t, a, []byte("v"), "1080p")
	if rec.Code != 429 {
		t.Fatalf("status %d, want 429", rec.Code)
	}
	close(g.release)
}

func TestVideoUploadMissingFilePart400(t *testing.T) {
	a := newVideoAPI(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("preset", "720p") // a field part but no file part
	mw.Close()
	req := httptest.NewRequest("POST", "/api/media/video", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("status %d, want 400", rec.Code)
	}
}
