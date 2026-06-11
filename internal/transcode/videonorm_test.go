package transcode

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestNormalizeArgs(t *testing.T) {
	// Pure: assert the arg builder pins the spec'd encoder contract.
	args := normalizeArgs("in.mov", "out.mp4", NormParams{W: 1280, H: 720, FPS: 60, HasAudio: true})
	s := strings.Join(args, " ")
	for _, want := range []string{
		"-c:v libx264", "-preset medium", "-crf 23", "-maxrate 8M",
		"-vf scale=1280:720", "-r 60", "-pix_fmt yuv420p",
		"-c:a aac", "-b:a 128k", "-ar 48000", "-ac 2",
		"-map_metadata -1", "-movflags +faststart", "-progress pipe:1",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("args missing %q in: %s", want, s)
		}
	}
	if strings.Contains(s, "-noautorotate") {
		t.Fatal("NEVER disable autorotation — probe dims are display dims")
	}
	noAudio := strings.Join(normalizeArgs("a", "b", NormParams{W: 2, H: 2, FPS: 30, HasAudio: false}), " ")
	if strings.Contains(noAudio, "-c:a") {
		t.Fatal("audio args must be omitted for silent input")
	}
	if !strings.Contains(noAudio, "-an") {
		t.Fatal("silent input must explicitly drop audio (-an)")
	}
}

func TestNormalizeRealFile(t *testing.T) {
	ffmpeg, ffprobe := requireFFmpeg(t)
	in := makeFixture(t, ffmpeg)
	out := in + ".norm.mp4"
	var last float64
	err := Normalize(context.Background(), ffmpeg, in, out,
		NormParams{W: 320, H: 240, FPS: 30, HasAudio: true, DurationSecs: 2},
		func(f float64) { last = f })
	if err != nil {
		t.Fatal(err)
	}
	m, err := Probe(context.Background(), ffprobe, out)
	if err != nil {
		t.Fatal(err)
	}
	if m.VCodec != "h264" || m.ACodec != "aac" || m.W != 320 || m.H != 240 {
		t.Fatalf("normalized meta = %+v", m)
	}
	if last <= 0 || last > 1.01 {
		t.Fatalf("progress callback last=%f, want (0,1]", last)
	}
}

func TestNormalizeFPSCap(t *testing.T) {
	ffmpeg, ffprobe := requireFFmpeg(t)
	in := makeFixture(t, ffmpeg, "-r", "90") // 90fps source
	out := in + ".norm.mp4"
	if err := Normalize(context.Background(), ffmpeg, in, out,
		NormParams{W: 320, H: 240, FPS: 60, HasAudio: true, DurationSecs: 2}, nil); err != nil {
		t.Fatal(err)
	}
	m, _ := Probe(context.Background(), ffprobe, out)
	if m.FPS > 60.5 {
		t.Fatalf("fps not capped: %f", m.FPS)
	}
}

func TestNormalizeRotatedPortrait(t *testing.T) {
	// End-to-end rotation contract: a -90 display-rotated 320x240 clip probes
	// as 240x320 (display dims); normalizing to those display-fitted dims
	// must produce a REAL 240x320 output with square pixels (no anamorphic
	// compensation) and no leftover rotation metadata.
	ffmpeg, ffprobe := requireFFmpeg(t)
	plain := makeFixture(t, ffmpeg)
	rotated := plain + ".rot.mp4"
	if b, err := execCommand(ffmpeg, "-display_rotation", "-90", "-i", plain, "-c", "copy", "-y", rotated); err != nil {
		t.Fatalf("remux: %v\n%s", err, b)
	}
	m, err := Probe(context.Background(), ffprobe, rotated)
	if err != nil {
		t.Fatal(err)
	}
	if m.W != 240 || m.H != 320 {
		t.Fatalf("probe dims %dx%d, want 240x320", m.W, m.H)
	}
	out := rotated + ".norm.mp4"
	if err := Normalize(context.Background(), ffmpeg, rotated, out,
		NormParams{W: m.W, H: m.H, FPS: 30, HasAudio: m.HasAudio, DurationSecs: m.DurationSecs}, nil); err != nil {
		t.Fatal(err)
	}
	o, err := Probe(context.Background(), ffprobe, out)
	if err != nil {
		t.Fatal(err)
	}
	if o.W != 240 || o.H != 320 {
		t.Fatalf("output dims %dx%d, want 240x320 (autorotation + display-fitted scale)", o.W, o.H)
	}
}

// execCommand runs a binary capturing combined output (fixture remuxes).
func execCommand(bin string, args ...string) ([]byte, error) {
	return exec.Command(bin, args...).CombinedOutput()
}
