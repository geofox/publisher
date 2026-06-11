package transcode

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// probeJSON is a canned ffprobe -print_format json output (1280x720, 2.5s,
// 30fps h264 + aac) so the parser is tested without any binary.
const probeJSON = `{
  "streams": [
    {"codec_type": "video", "codec_name": "h264", "width": 1280, "height": 720,
     "r_frame_rate": "30/1"},
    {"codec_type": "audio", "codec_name": "aac"}
  ],
  "format": {"duration": "2.500000"}
}`

func TestParseProbe(t *testing.T) {
	m, err := parseProbe([]byte(probeJSON))
	if err != nil {
		t.Fatal(err)
	}
	if m.W != 1280 || m.H != 720 || m.VCodec != "h264" || !m.HasAudio {
		t.Fatalf("meta = %+v", m)
	}
	if m.DurationSecs < 2.4 || m.DurationSecs > 2.6 {
		t.Fatalf("duration = %f", m.DurationSecs)
	}
	if m.FPS < 29.9 || m.FPS > 30.1 {
		t.Fatalf("fps = %f", m.FPS)
	}
}

func TestParseProbeRejectsNoVideoStream(t *testing.T) {
	audioOnly := `{"streams":[{"codec_type":"audio","codec_name":"aac"}],"format":{"duration":"1.0"}}`
	if _, err := parseProbe([]byte(audioOnly)); err == nil {
		t.Fatal("audio-only input must be rejected")
	}
}

// requireFFmpeg skips unless both binaries are present (dev hosts may lack
// them; the controller extracted static ones to ~/.local/bin).
func requireFFmpeg(t *testing.T) (ffmpeg, ffprobe string) {
	t.Helper()
	var err error
	if ffmpeg, err = exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	if ffprobe, err = exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not on PATH")
	}
	return
}

// makeFixture renders a tiny test clip with ffmpeg's lavfi testsrc (no
// committed binaries). Returns the file path.
func makeFixture(t *testing.T, ffmpeg string, args ...string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "fixture.mp4")
	base := []string{"-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=30",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-c:v", "libx264", "-c:a", "aac", "-shortest"}
	cmd := exec.Command(ffmpeg, append(append(base, args...), "-y", out)...)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v\n%s", err, b)
	}
	return out
}

func TestProbeRealFile(t *testing.T) {
	ffmpeg, ffprobe := requireFFmpeg(t)
	f := makeFixture(t, ffmpeg)
	m, err := Probe(context.Background(), ffprobe, f)
	if err != nil {
		t.Fatal(err)
	}
	if m.W != 320 || m.H != 240 || m.VCodec != "h264" || !m.HasAudio {
		t.Fatalf("meta = %+v", m)
	}
	if m.DurationSecs < 1.5 || m.DurationSecs > 2.5 {
		t.Fatalf("duration = %f", m.DurationSecs)
	}
}

func TestProbeRejectsJunk(t *testing.T) {
	_, ffprobe := requireFFmpeg(t)
	junk := filepath.Join(t.TempDir(), "junk.mp4")
	os.WriteFile(junk, []byte("not a video at all"), 0o644)
	if _, err := Probe(context.Background(), ffprobe, junk); err == nil {
		t.Fatal("junk must be rejected")
	}
}
