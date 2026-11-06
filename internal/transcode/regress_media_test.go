package transcode

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// qaGPSJPEG is a small JPEG with an EXIF APP1 that carries orientation 6 and a
// recognisable GPS marker string, standing in for a phone photo.
func qaGPSJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 0x80
	}
	src := buildExifJPEG(img, 6)
	// Append a fake GPS payload inside a second APP1 so the marker is
	// unambiguous in the bytes.
	gps := append([]byte("Exif\x00\x00"), []byte("GPSLatitude=48.8584N GPSLongitude=2.2945E")...)
	seg := []byte{0xFF, 0xE1, byte((len(gps) + 2) >> 8), byte(len(gps) + 2)}
	seg = append(seg, gps...)
	out := append([]byte{}, src[:2]...)
	out = append(out, seg...)
	return append(out, src[2:]...)
}

// Finding: a fitting JPEG is shipped to Bluesky/Mastodon/Threads byte for
// byte, EXIF (GPS) included.
func TestFitPassesEXIFGPSThrough(t *testing.T) {
	src := qaGPSJPEG(t, 64, 32)
	for _, p := range []Profile{Bluesky, Mastodon, Threads} {
		r, err := p.Fit(src, "image/jpeg")
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(r.Bytes, []byte("GPSLatitude")) {
			t.Errorf("%s: Fit output still carries the GPS EXIF payload (Changed=%v)", p.Name, r.Changed)
		}
	}
}

// Finding: Fit reports stored (pre-rotation) dims for an EXIF-rotated JPEG,
// which Bluesky uses as aspectRatio.
func TestFitDimsIgnoreOrientation(t *testing.T) {
	src := qaGPSJPEG(t, 64, 32) // orientation 6 => displays 32x64
	r, err := Bluesky.Fit(src, "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if r.W != 32 || r.H != 64 {
		t.Errorf("Bluesky.Fit dims = %dx%d, want display dims 32x64 (orientation 6)", r.W, r.H)
	}
}

// Finding: Threads rejects images beyond 10:1; Fit passes them.
func TestThreadsAspectRatio(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 3300, 300)) // 11:1
	src := encJPEG(t, img, 80)
	r, err := Threads.Fit(src, "image/jpeg")
	if err == nil && r.W > 10*r.H {
		t.Errorf("Threads.Fit accepted a %dx%d (%.1f:1) image; Threads' limit is 10:1", r.W, r.H, float64(r.W)/float64(r.H))
	}
}

func qaNeedFF(t *testing.T) (string, string) {
	ff, err1 := exec.LookPath("ffmpeg")
	fp, err2 := exec.LookPath("ffprobe")
	if err1 != nil || err2 != nil {
		t.Skip("ffmpeg/ffprobe not installed")
	}
	return ff, fp
}

// Finding: an uploaded "video" that is really an HLS playlist makes
// ffprobe/ffmpeg fetch URLs it names (SSRF from the server).
func TestProbeFollowsHLSPlaylistURLs(t *testing.T) {
	ff, fp := qaNeedFF(t)
	var hits atomic.Int32
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		t.Logf("internal server hit: %s %s", r.Method, r.URL)
		http.NotFound(w, r)
	})}
	go srv.Serve(ln)
	defer srv.Close()
	dir := t.TempDir()
	in := filepath.Join(dir, "upload-123.video") // the name receiveUpload gives it
	pl := fmt.Sprintf("#EXTM3U\n#EXT-X-TARGETDURATION:10\n#EXTINF:10.0,\nhttp://%s/internal-admin/seg.ts\n#EXT-X-ENDLIST\n", ln.Addr())
	if err := os.WriteFile(in, []byte(pl), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, perr := Probe(ctx, fp, in)
	t.Logf("probe err: %v", perr)
	_ = Normalize(ctx, ff, in, filepath.Join(dir, "out.mp4"), NormParams{W: 2, H: 2, FPS: 30}, nil)
	if n := hits.Load(); n > 0 {
		t.Errorf("ffprobe/ffmpeg made %d request(s) to a URL named inside the uploaded file", n)
	}
}

// Finding: the canonical MP4 has edit lists; Threads requires "no edit lists".
func TestNormalizeWritesEditList(t *testing.T) {
	ff, fp := qaNeedFF(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp4")
	gen := exec.Command(ff, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=320x240:rate=30:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2", "-shortest", "-c:v", "libx264", "-c:a", "aac", "-y", src)
	if b, err := gen.CombinedOutput(); err != nil {
		t.Skipf("cannot generate source: %v %s", err, b)
	}
	ctx := context.Background()
	m, err := Probe(ctx, fp, src)
	if err != nil {
		t.Fatal(err)
	}
	w, h := FitVideoDims(m.W, m.H, "1080p")
	out := filepath.Join(dir, "out.mp4")
	if err := Normalize(ctx, ff, src, out, NormParams{W: w, H: h, FPS: m.FPS, HasAudio: m.HasAudio, DurationSecs: m.DurationSecs}, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if bytes.Contains(b, []byte("elst")) {
		t.Errorf("normalized canonical contains an edts/elst edit list; Threads video spec says \"no edit lists\"")
	}
}
