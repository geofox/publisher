package bluesky

import (
	"bytes"
	"image"
	"image/jpeg"
	"image/png"
	"math/rand"
	"testing"

	"github.com/geofox/publisher/internal/transcode"
)

func TestFitBlobShrinksLargeImage(t *testing.T) {
	// 1500x1500 random RGBA → PNG is incompressible and well over 2 MB.
	img := image.NewRGBA(image.Rect(0, 0, 1500, 1500))
	rng := rand.New(rand.NewSource(1))
	_, _ = rng.Read(img.Pix)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if buf.Len() <= int(transcode.Bluesky.MaxBytes) {
		t.Fatalf("precondition: test image only %d bytes", buf.Len())
	}
	out, mime, w, h, err := fitBlob(buf.Bytes(), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > int(transcode.Bluesky.MaxBytes) {
		t.Errorf("output %d bytes exceeds cap %d", len(out), transcode.Bluesky.MaxBytes)
	}
	if mime != "image/jpeg" {
		t.Errorf("expected re-encode to jpeg, got %s", mime)
	}
	if w <= 0 || h <= 0 {
		t.Errorf("bad dims %dx%d", w, h)
	}
	if _, _, err := image.Decode(bytes.NewReader(out)); err != nil {
		t.Errorf("output not decodable: %v", err)
	}
}

func TestFitBlobPassesThroughSmall(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	out, mime, w, h, err := fitBlob(buf.Bytes(), "image/png")
	if err != nil || mime != "image/png" || len(out) != buf.Len() || w != 4 || h != 4 {
		t.Errorf("small image should pass through unchanged: mime=%s len=%d/%d dims=%dx%d err=%v",
			mime, len(out), buf.Len(), w, h, err)
	}
}

func TestFitBlobNonImagePassThrough(t *testing.T) {
	out, mime, w, h, err := fitBlob([]byte("not an image"), "application/octet-stream")
	if err != nil || mime != "application/octet-stream" || string(out) != "not an image" || w != 0 || h != 0 {
		t.Errorf("non-image small passthrough: mime=%s w=%d h=%d err=%v", mime, w, h, err)
	}
}

// bigJPEG encodes deterministic noise (rand.NewSource(42)) at growing
// dimensions until the resulting JPEG exceeds minBytes but stays under
// 1,998,848 bytes (the ~2 MB image ceiling). It starts at 1300x975 at q90
// and steps up by 50 px; the test fails if no size hits the window.
func bigJPEG(t *testing.T, minBytes int) []byte {
	t.Helper()
	const maxBytes = 1_998_848
	for w := 1300; w <= 4000; w += 50 {
		h := w * 3 / 4
		rng := rand.New(rand.NewSource(42))
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		// Fill with deterministic noise to defeat JPEG compression.
		for i := 0; i < len(img.Pix); i += 4 {
			img.Pix[i] = uint8(rng.Intn(256))
			img.Pix[i+1] = uint8(rng.Intn(256))
			img.Pix[i+2] = uint8(rng.Intn(256))
			img.Pix[i+3] = 255
		}
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
			t.Fatalf("bigJPEG: encode %dx%d: %v", w, h, err)
		}
		size := buf.Len()
		if size >= minBytes && size < maxBytes {
			t.Logf("bigJPEG: settled on %dx%d → %d bytes", w, h, size)
			return buf.Bytes()
		}
	}
	t.Fatalf("bigJPEG: could not produce a JPEG in [%d, %d) bytes", minBytes, maxBytes)
	return nil
}

func TestFitThumbUsesOneMBCeiling(t *testing.T) {
	// 1.5 MB JPEG: fits the ~2 MB image ceiling but NOT the 1 MB thumb
	// ceiling (external lexicon thumb maxSize is still 1,000,000).
	src := bigJPEG(t, 1500*1024)
	out, _, _, _, err := fitBlob(src, "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, src) {
		t.Fatal("1.5 MB image should pass the ~2 MB image ceiling untouched")
	}
	tout, _, err := fitThumb(src, "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(tout)) > transcode.BlueskyThumb.MaxBytes {
		t.Fatalf("thumb output %d bytes exceeds the 1 MB thumb ceiling", len(tout))
	}
}
