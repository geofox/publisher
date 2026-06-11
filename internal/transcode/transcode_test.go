package transcode

import (
	"bytes"
	"crypto/sha256"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand"
	"testing"
)

// noiseImg builds a w×h image of deterministic per-pixel noise — incompressible,
// so encoded sizes stay large enough to exercise byte ceilings.
func noiseImg(t *testing.T, w, h int) *image.RGBA {
	t.Helper()
	rng := rand.New(rand.NewSource(42))
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = uint8(rng.Intn(256))
	}
	return img
}

func encPNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func encJPEG(t *testing.T, img image.Image, q int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestImagePassthroughWhenFits(t *testing.T) {
	src := encJPEG(t, noiseImg(t, 100, 80), 85)
	r, err := Image(src, "image/jpeg", ImageParams{MaxBytes: 1 << 20, Format: KeepIfAllowed, Quality: 85})
	if err != nil {
		t.Fatal(err)
	}
	if r.Changed {
		t.Fatal("small image should pass through unchanged")
	}
	if !bytes.Equal(r.Bytes, src) || r.Mime != "image/jpeg" {
		t.Fatal("passthrough must return identical bytes and mime")
	}
	if r.W != 100 || r.H != 80 {
		t.Fatalf("dims = %dx%d, want 100x80", r.W, r.H)
	}
}

func TestImageFitsUnderByteCeiling(t *testing.T) {
	src := encPNG(t, noiseImg(t, 1400, 1400)) // multi-MB PNG
	ceiling := int64(300 * 1024)
	r, err := Image(src, "image/png", ImageParams{MaxBytes: ceiling, Format: KeepIfAllowed, Quality: 85})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Changed || r.Mime != "image/jpeg" {
		t.Fatalf("oversized PNG must re-encode to JPEG (changed=%v mime=%s)", r.Changed, r.Mime)
	}
	if int64(len(r.Bytes)) > ceiling {
		t.Fatalf("output %d bytes exceeds ceiling %d", len(r.Bytes), ceiling)
	}
	if r.W <= 0 || r.H <= 0 {
		t.Fatal("re-encode must report output dimensions")
	}
}

func TestImageMaxLongEdgeDownscales(t *testing.T) {
	src := encJPEG(t, noiseImg(t, 3000, 1500), 85)
	r, err := Image(src, "image/jpeg", ImageParams{MaxLongEdge: 1500, Format: JPEG, Quality: 82})
	if err != nil {
		t.Fatal(err)
	}
	if r.W != 1500 || r.H != 750 {
		t.Fatalf("dims = %dx%d, want 1500x750 (aspect preserved)", r.W, r.H)
	}
}

func TestImageFormatJPEGAlwaysReencodes(t *testing.T) {
	src := encJPEG(t, noiseImg(t, 100, 100), 85)
	r, err := Image(src, "image/jpeg", ImageParams{Format: JPEG, Quality: 90})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Changed {
		t.Fatal("Format JPEG must re-encode even when input already fits")
	}
}

func TestImageDeterministic(t *testing.T) {
	src := encPNG(t, noiseImg(t, 1200, 900))
	p := ImageParams{MaxBytes: 200 * 1024, Format: JPEG, Quality: 82}
	a, err := Image(src, "image/png", p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Image(src, "image/png", p)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(a.Bytes) != sha256.Sum256(b.Bytes) {
		t.Fatal("same input + params must produce identical bytes")
	}
}

func TestImageFlattensAlphaToWhite(t *testing.T) {
	// Fully transparent 50x50 PNG → JPEG re-encode must come out white, not black.
	img := image.NewNRGBA(image.Rect(0, 0, 50, 50))
	src := encPNG(t, img)
	r, err := Image(src, "image/png", ImageParams{Format: JPEG, Quality: 90})
	if err != nil {
		t.Fatal(err)
	}
	out, err := jpeg.Decode(bytes.NewReader(r.Bytes))
	if err != nil {
		t.Fatal(err)
	}
	c := color.GrayModel.Convert(out.At(25, 25)).(color.Gray)
	if c.Y < 240 {
		t.Fatalf("transparent pixel re-encoded to %d, want near-white (>=240)", c.Y)
	}
}

func TestImagePixelBomb(t *testing.T) {
	// A small PNG whose header declares gigapixel dimensions must error when it
	// needs work, and pass through when it already fits (old fitBlob contract).
	bomb := pngBomb(t, 60000, 60000)
	if _, err := Image(bomb, "image/png", ImageParams{MaxBytes: 1, Format: KeepIfAllowed, Quality: 85}); err == nil {
		t.Fatal("oversized pixel bomb must be rejected, not decoded")
	}
	r, err := Image(bomb, "image/png", ImageParams{MaxBytes: 1 << 20, Format: KeepIfAllowed, Quality: 85})
	if err != nil || r.Changed {
		t.Fatalf("small-enough bomb should pass through: changed=%v err=%v", r.Changed, err)
	}
}

func TestImageUndecodableButSmallPassesThrough(t *testing.T) {
	r, err := Image([]byte("not an image"), "application/octet-stream",
		ImageParams{MaxBytes: 1 << 20, Format: KeepIfAllowed, Quality: 85})
	if err != nil || r.Changed {
		t.Fatalf("undecodable-but-small must pass through: changed=%v err=%v", r.Changed, err)
	}
}

func TestImageQualityZeroDefaults(t *testing.T) {
	src := encPNG(t, noiseImg(t, 200, 200))
	zero, err := Image(src, "image/png", ImageParams{Format: JPEG})
	if err != nil {
		t.Fatal(err)
	}
	q85, err := Image(src, "image/png", ImageParams{Format: JPEG, Quality: 85})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(zero.Bytes, q85.Bytes) {
		t.Fatal("Quality 0 must default to 85, not the encoder's quality-1 clamp")
	}
}

func TestImageKeepIfAllowedDeniedByLongEdge(t *testing.T) {
	src := encJPEG(t, noiseImg(t, 1000, 500), 85)
	r, err := Image(src, "image/jpeg", ImageParams{MaxBytes: 10 << 20, MaxLongEdge: 800, Format: KeepIfAllowed, Quality: 85})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Changed || r.W != 800 || r.H != 400 {
		t.Fatalf("under-cap but over-edge must re-encode downscaled: changed=%v %dx%d", r.Changed, r.W, r.H)
	}
}

func TestImageLadderExhaustionErrors(t *testing.T) {
	src := encPNG(t, noiseImg(t, 400, 400))
	if _, err := Image(src, "image/png", ImageParams{MaxBytes: 1, Format: JPEG, Quality: 85}); err == nil {
		t.Fatal("impossible byte ceiling must error after ladder exhaustion")
	}
}

// jpegBomb hand-crafts a JPEG declaring w×h in the SOF0 segment. Go's JPEG
// DecodeConfig succeeds (reads SOF0 dims when it finds the SOS marker), but
// the image is undecodable (no valid Huffman tables, no real scan data), so
// Image() passes it through unchanged. Useful to test Fit's re-check path
// where dims come from the header but the payload can't be transcoded.
func jpegBomb(t *testing.T, w, h int) []byte {
	t.Helper()
	// Go's JPEG DecodeConfig (configOnly=true) returns early at the SOS
	// marker without decoding the scan data — so we include a minimal SOS.
	// The scan data is one zero byte; Decode will fail on it, which is fine.
	buf := &bytes.Buffer{}
	// SOI
	buf.Write([]byte{0xff, 0xd8})
	// SOF0: FF C0 | len(2be) | precision(1) | height(2be) | width(2be) | ncomp(1) | comp[3]
	// len = 2(len field) + 1(prec) + 2(h) + 2(w) + 1(ncomp) + 3*1(comp) = 11
	buf.Write([]byte{0xff, 0xc0, 0x00, 0x0b})
	buf.Write([]byte{8}) // 8-bit precision
	buf.Write([]byte{byte(h >> 8), byte(h & 0xff)})
	buf.Write([]byte{byte(w >> 8), byte(w & 0xff)})
	buf.Write([]byte{1})          // 1 component (grayscale)
	buf.Write([]byte{1, 0x11, 0}) // comp id, sampling, qtable
	// SOS: FF DA | len(2be) | ncomp(1) | comp[2*1] | 3 spec bytes | data
	// len = 2 + 1 + 2*1 + 3 = 8
	buf.Write([]byte{0xff, 0xda, 0x00, 0x08})
	buf.Write([]byte{1})       // 1 component
	buf.Write([]byte{1, 0x00}) // comp selector + Huffman table ids
	buf.Write([]byte{0, 63, 0}) // Ss, Se, Ah/Al
	buf.Write([]byte{0x00})     // minimal (invalid) scan data
	// EOI
	buf.Write([]byte{0xff, 0xd9})
	return buf.Bytes()
}

// pngBomb hand-crafts a PNG IHDR declaring w×h without allocating the bitmap
// (mirrors internal/bluesky/bomb_test.go).
func pngBomb(t *testing.T, w, h int) []byte {
	t.Helper()
	small := encPNG(t, image.NewRGBA(image.Rect(0, 0, 1, 1)))
	// IHDR starts at offset 16: 4-byte width, 4-byte height (big-endian).
	b := append([]byte(nil), small...)
	put := func(off, v int) {
		b[off] = byte(v >> 24)
		b[off+1] = byte(v >> 16)
		b[off+2] = byte(v >> 8)
		b[off+3] = byte(v)
	}
	put(16, w)
	put(20, h)
	// CRC now wrong, but DecodeConfig reads dims before checking it.
	return b
}
