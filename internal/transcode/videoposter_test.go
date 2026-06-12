package transcode

import (
	"context"
	"image"
	_ "image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPosterArgs(t *testing.T) {
	s := strings.Join(posterArgs("in.mp4", "out.jpg", 0.8), " ")
	for _, want := range []string{"-ss 0.8", "-i in.mp4", "-frames:v 1", "-q:v 4", "-y out.jpg"} {
		if !strings.Contains(s, want) {
			t.Fatalf("args missing %q in: %s", want, s)
		}
	}
	if strings.Contains(s, "-noautorotate") {
		t.Fatal("autorotation must stay on for posters too (portrait frames)")
	}
}

func TestPosterAt(t *testing.T) {
	cases := []struct{ dur, want float64 }{{30, 1}, {5, 0.5}, {0.4, 0.04}}
	for _, c := range cases {
		if got := PosterAt(c.dur); got != c.want {
			t.Fatalf("PosterAt(%v) = %v, want %v", c.dur, got, c.want)
		}
	}
}

func TestExtractPosterRealFile(t *testing.T) {
	ffmpeg, ffprobe := requireFFmpeg(t)
	_ = ffprobe
	in := makeFixture(t, ffmpeg)
	out := filepath.Join(t.TempDir(), "poster.jpg")
	if err := ExtractPoster(context.Background(), ffmpeg, in, out, PosterAt(2)); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, format, err := image.DecodeConfig(f)
	if err != nil || format != "jpeg" {
		t.Fatalf("poster decode: format=%s err=%v", format, err)
	}
	if cfg.Width != 320 || cfg.Height != 240 {
		t.Fatalf("poster dims %dx%d, want 320x240", cfg.Width, cfg.Height)
	}
}
