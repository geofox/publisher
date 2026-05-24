package bluesky

import (
	"bytes"
	"image"
	"image/png"
	"math/rand"
	"testing"
)

func TestFitBlobShrinksLargeImage(t *testing.T) {
	// 1500x1500 random RGBA → PNG is incompressible and well over 1 MB.
	img := image.NewRGBA(image.Rect(0, 0, 1500, 1500))
	rng := rand.New(rand.NewSource(1))
	_, _ = rng.Read(img.Pix)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if buf.Len() <= maxBlobBytes {
		t.Fatalf("precondition: test image only %d bytes", buf.Len())
	}
	out, mime, w, h, err := fitBlob(buf.Bytes(), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > maxBlobBytes {
		t.Errorf("output %d bytes exceeds cap %d", len(out), maxBlobBytes)
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
