package transcode

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// probeJSON is a canned ffprobe -print_format json output (1280x720, 2.5s,
// 30fps h264 + aac) so the parser is tested without any binary.
const probeJSON = `{
  "streams": [
    {"codec_type": "video", "codec_name": "h264", "width": 1280, "height": 720,
     "avg_frame_rate": "30/1", "r_frame_rate": "30/1"},
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

func TestParseProbeRotationSwapsDims(t *testing.T) {
	rotated := `{"streams":[{"codec_type":"video","codec_name":"h264","width":1920,"height":1080,
	  "avg_frame_rate":"30/1","r_frame_rate":"30/1",
	  "side_data_list":[{"side_data_type":"Display Matrix","rotation":-90}]}],
	  "format":{"duration":"2.0"}}`
	m, err := parseProbe([]byte(rotated))
	if err != nil {
		t.Fatal(err)
	}
	if m.W != 1080 || m.H != 1920 {
		t.Fatalf("portrait dims = %dx%d, want 1080x1920 (display orientation)", m.W, m.H)
	}
	flipped := `{"streams":[{"codec_type":"video","codec_name":"h264","width":1920,"height":1080,
	  "avg_frame_rate":"30/1","r_frame_rate":"30/1",
	  "side_data_list":[{"side_data_type":"Display Matrix","rotation":-180}]}],
	  "format":{"duration":"2.0"}}`
	m2, _ := parseProbe([]byte(flipped))
	if m2.W != 1920 || m2.H != 1080 {
		t.Fatalf("180° dims = %dx%d, want unchanged 1920x1080", m2.W, m2.H)
	}
}

func TestParseProbeSkipsCoverArt(t *testing.T) {
	mp3 := `{"streams":[
	  {"codec_type":"video","codec_name":"png","width":300,"height":300,
	   "disposition":{"attached_pic":1}},
	  {"codec_type":"audio","codec_name":"mp3"}],
	  "format":{"duration":"180.0"}}`
	if _, err := parseProbe([]byte(mp3)); err == nil {
		t.Fatal("cover-art-only 'video' must be rejected (it's an audio file)")
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
	_, err := Probe(context.Background(), ffprobe, junk)
	if err == nil {
		t.Fatal("junk must be rejected")
	}
	if !strings.Contains(err.Error(), "Invalid data") && !strings.Contains(err.Error(), "moov") {
		t.Fatalf("error message too vague (no ffprobe stderr): %v", err)
	}
}

func TestProbeRealRotatedFile(t *testing.T) {
	ffmpeg, ffprobe := requireFFmpeg(t)
	plain := makeFixture(t, ffmpeg)
	rotated := filepath.Join(t.TempDir(), "rot.mp4")
	cmd := exec.Command(ffmpeg, "-display_rotation", "-90", "-i", plain, "-c", "copy", "-y", rotated)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("remux: %v\n%s", err, b)
	}
	m, err := Probe(context.Background(), ffprobe, rotated)
	if err != nil {
		t.Fatal(err)
	}
	if m.W != 240 || m.H != 320 {
		t.Fatalf("rotated dims = %dx%d, want 240x320 (display orientation)", m.W, m.H)
	}
}
