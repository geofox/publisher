package transcode

import (
	"image"
	"os"
	"strings"
	"testing"
)

func TestPresetParams(t *testing.T) {
	cases := []struct {
		name     string
		edge     int
		maxBytes int64
	}{
		{"convert", 0, 64 << 20},
		{"large", 2048, 0},
		{"medium", 1600, 0},
		{"small", 1080, 0},
	}
	for _, c := range cases {
		p, ok := PresetParams(c.name)
		if !ok {
			t.Fatalf("preset %q missing", c.name)
		}
		if p.MaxLongEdge != c.edge || p.Format != JPEG {
			t.Fatalf("preset %q = %+v, want JPEG with edge %d", c.name, p, c.edge)
		}
		if p.MaxBytes != c.maxBytes {
			t.Fatalf("preset %q MaxBytes = %d, want %d", c.name, p.MaxBytes, c.maxBytes)
		}
	}
	if _, ok := PresetParams("original"); ok {
		t.Fatal(`"original" must not be a server preset (client-side revert)`)
	}
}

func TestProfileNeeds(t *testing.T) {
	cases := []struct {
		plat   string
		m      Meta
		need   bool
		reason string
	}{
		{"bluesky", Meta{SizeBytes: 1952 * 1024, Mime: "image/jpeg"}, false, ""},
		{"bluesky", Meta{SizeBytes: 1952*1024 + 1, Mime: "image/jpeg"}, true, "over"},
		{"mastodon", Meta{SizeBytes: 8<<20 + 1, Mime: "image/jpeg"}, true, "over"},
		{"mastodon", Meta{SizeBytes: 1000, Mime: "image/jpeg", W: 6000, H: 3000}, true, "16 MP"},
		{"threads", Meta{SizeBytes: 1000, Mime: "image/webp"}, true, "webp"},
		{"threads", Meta{SizeBytes: 1000, Mime: "image/png"}, false, ""},
		// Unknown metadata plans optimistically; dispatch re-checks real bytes.
		{"threads", Meta{Mime: ""}, false, ""},
	}
	for _, c := range cases {
		prof, ok := ProfileFor(c.plat)
		if !ok {
			t.Fatalf("no profile for %s", c.plat)
		}
		need, reason := prof.Needs(c.m)
		if need != c.need || (c.reason != "" && !strings.Contains(reason, c.reason)) {
			t.Fatalf("%s %+v → need=%v reason=%q, want need=%v reason~%q", c.plat, c.m, need, reason, c.need, c.reason)
		}
	}
	if _, ok := ProfileFor("nostr"); ok {
		t.Fatal("nostr must have no profile (passthrough)")
	}
}

func TestProfileFitThreadsFormatHandling(t *testing.T) {
	// Sniff correction: PNG bytes lying as webp are identified as PNG, which
	// Threads accepts — passthrough, no pointless re-encode.
	src := encPNG(t, noiseImg(t, 50, 50))
	r, err := Threads.Fit(src, "image/webp")
	if err != nil {
		t.Fatal(err)
	}
	if r.Changed {
		t.Fatal("PNG mislabeled as webp must sniff-correct and pass through")
	}
	// A genuinely disallowed format (HEIC fixture) is re-encoded to JPEG.
	heicBytes, err := os.ReadFile("testdata/sample.heic")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := Threads.Fit(heicBytes, "image/heic")
	if err != nil {
		t.Fatal(err)
	}
	if !r2.Changed || r2.Mime != "image/jpeg" {
		t.Fatalf("HEIC for threads must re-encode to JPEG: changed=%v mime=%s", r2.Changed, r2.Mime)
	}
	// Undecodable bytes violating the profile fail loudly instead of shipping.
	if _, err := Threads.Fit([]byte("not an image"), "image/gif"); err == nil {
		t.Fatal("undecodable disallowed-format input must error, not pass through")
	}
}

func flatImg(t *testing.T, w, h int) *image.RGBA {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = 30, 90, 200, 255
	}
	return img
}

// TestNeedsFitAgreement pins the package invariant: for decodable input,
// Needs(meta)==false ⇒ Fit passthrough (byte-identical), and Needs==true ⇒
// Fit changes the bytes (or errors). Built after a review found the old
// square-edge pixel translation silently re-encoding every 4032x3024 phone
// photo for Mastodon without a badge.
func TestNeedsFitAgreement(t *testing.T) {
	cases := []struct {
		name string
		prof Profile
		img  *image.RGBA
	}{
		{"iphone 12MP", Mastodon, flatImg(t, 4032, 3024)},   // 12.2 MP, edge > 4000: must pass through
		{"panorama 12MP", Mastodon, flatImg(t, 8000, 1500)}, // long edge, under area cap: must pass through
		{"over area 20MP", Mastodon, flatImg(t, 5000, 4000)},
		{"small square", Mastodon, flatImg(t, 500, 500)},
		{"small bluesky", Bluesky, flatImg(t, 800, 600)},
	}
	for _, c := range cases {
		src := encJPEG(t, c.img, 85)
		b := c.img.Bounds()
		meta := Meta{SizeBytes: int64(len(src)), Mime: "image/jpeg", W: b.Dx(), H: b.Dy()}
		need, reason := c.prof.Needs(meta)
		r, err := c.prof.Fit(src, "image/jpeg")
		if err != nil {
			t.Fatalf("%s: fit error: %v", c.name, err)
		}
		if need != r.Changed {
			t.Fatalf("%s: Needs=%v (%s) but Fit.Changed=%v — predicate disagreement", c.name, need, reason, r.Changed)
		}
		if r.Changed && c.prof.MaxPixels > 0 {
			if int64(r.W)*int64(r.H) > c.prof.MaxPixels {
				t.Fatalf("%s: fitted to %dx%d, still over %d px", c.name, r.W, r.H, c.prof.MaxPixels)
			}
			// Aspect ratio preserved within rounding.
			in, out := float64(b.Dx())/float64(b.Dy()), float64(r.W)/float64(r.H)
			if in/out > 1.01 || out/in > 1.01 {
				t.Fatalf("%s: aspect drifted %f → %f", c.name, in, out)
			}
		}
	}
}

func TestProfileFitBlueskyCeiling(t *testing.T) {
	src := encPNG(t, noiseImg(t, 1800, 1800)) // > 2 MB as PNG
	if int64(len(src)) <= Bluesky.MaxBytes {
		t.Skip("fixture unexpectedly small")
	}
	r, err := Bluesky.Fit(src, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(r.Bytes)) > Bluesky.MaxBytes {
		t.Fatalf("fitted %d > ceiling %d", len(r.Bytes), Bluesky.MaxBytes)
	}
	if r.Mime != "image/jpeg" {
		t.Fatalf("oversize re-encode mime = %s, want image/jpeg", r.Mime)
	}
}

func TestParseDim(t *testing.T) {
	w, h := ParseDim("1200x800")
	if w != 1200 || h != 800 {
		t.Fatalf("ParseDim = %d,%d", w, h)
	}
	if w, h := ParseDim("garbage"); w != 0 || h != 0 {
		t.Fatalf("bad dim must yield 0,0, got %d,%d", w, h)
	}
}
