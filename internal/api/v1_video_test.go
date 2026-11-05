package api

import (
	"bytes"
	"context"
	"errors"
	"mime/multipart"
	"os"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/media"
	"github.com/geofox/publisher/internal/transcode"
	"github.com/geofox/publisher/internal/videojob"
)

// Fakes for the video runner: a transcoder that writes placeholder files
// (or fails), and media storage that answers fixed URLs.
type vtFake struct{ fail bool }

func (vtFake) Probe(context.Context, string) (transcode.VideoMeta, error) {
	return transcode.VideoMeta{W: 320, H: 240, DurationSecs: 2, FPS: 30, VCodec: "hevc", HasAudio: true}, nil
}
func (f vtFake) Normalize(_ context.Context, _, out string, _ transcode.NormParams, _ func(float64)) error {
	if f.fail {
		return errors.New("ffmpeg: broken input")
	}
	return os.WriteFile(out, []byte("norm"), 0o644)
}
func (vtFake) ExtractPoster(_ context.Context, _, out string, _ float64) error {
	return os.WriteFile(out, []byte("\xFF\xD8poster"), 0o644)
}

type vsFake struct{}

func (vsFake) ProcessFile(_ context.Context, _, _, dim string, dur int64, _ func(float64)) (media.Result, error) {
	return media.Result{URL: "https://b/vid", SHA256: "ff", Mime: "video/mp4", Dim: dim, DurationSecs: dur, Size: 4}, nil
}
func (vsFake) Process(context.Context, []byte, string) (media.Result, error) {
	return media.Result{URL: "https://b/poster"}, nil
}

// videoForm builds a multipart body with a file part and other fields.
func videoForm(file string, fields ...[2]string) (string, string) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "clip.mov")
	_, _ = fw.Write([]byte(file))
	for _, f := range fields {
		_ = mw.WriteField(f[0], f[1])
	}
	_ = mw.Close()
	return buf.String(), mw.FormDataContentType()
}

const testMov = "\x00\x00\x00\x14ftypqt  \x00\x00\x00\x00qt  pretend-video-bytes"

// waitMedia polls GET /v1/media/{id} until it leaves processing.
func (e *v1Env) waitMedia(id string) v1MediaObjectBody {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r := e.do(req{method: "GET", path: "/v1/media/" + id})
		wantStatus(e.t, r, 200)
		var m v1MediaObjectBody
		r.json(&m)
		if m.Status != "processing" || time.Now().After(deadline) {
			return m
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestV1VideoLifecycle pins POST /v1/media for a video: 202 processing,
// then ready with the normalized file, or failed when the transcode fails.
// (It replaces the old /api/media/video tests.)
func TestV1VideoLifecycle(t *testing.T) {
	for _, c := range []struct {
		name string
		fail bool
		want string
	}{{"ready", false, "ready"}, {"failed", true, "failed"}} {
		t.Run(c.name, func(t *testing.T) {
			e := newV1Env(t, true)
			e.withEngine()
			e.api.VideoJobs = videojob.NewRunner(vtFake{fail: c.fail}, vsFake{}, t.TempDir())
			e.api.VideoWorkdir = t.TempDir()
			body, ct := videoForm(testMov, [2]string{"preset", "720p"}, [2]string{"alt", "a clip"})
			r := e.do(req{method: "POST", path: "/v1/media", body: body, header: map[string]string{"Content-Type": ct}})
			wantStatus(t, r, 202)
			var m v1MediaObjectBody
			r.json(&m)
			if m.Status != "processing" {
				t.Fatalf("media = %+v", m)
			}
			got := e.waitMedia(m.ID)
			if got.Status != c.want {
				t.Fatalf("status = %q, want %q (%+v)", got.Status, c.want, got)
			}
			if c.want == "ready" && (got.URL == nil || *got.URL != "https://b/vid" || got.Mime != "video/mp4" || got.Alt != "a clip") {
				t.Fatalf("ready media = %+v", got)
			}
		})
	}
}
