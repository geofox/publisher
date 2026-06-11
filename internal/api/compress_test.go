package api

import (
	"bytes"
	"image"
	"image/jpeg"
	"math/rand"
	"mime/multipart"
	"net/http/httptest"
	"testing"
)

// noiseJPEG returns a w×h JPEG of deterministic per-pixel noise (incompressible,
// so sizes stay meaningful).
func noiseJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	rng := rand.New(rand.NewSource(42))
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = uint8(rng.Intn(256))
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func compressReq(t *testing.T, filename, preset string, payload []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	fw.Write(payload)
	mw.WriteField("preset", preset)
	mw.Close()
	req := httptest.NewRequest("POST", "/api/media/compress", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	// Routes() is used (as in api_test.go / TestAPIPost): when auth is disabled
	// withGates is a pass-through and withCSRFGuard only blocks when Origin is
	// set, so test requests without an Origin header flow straight through.
	a := &API{}
	a.Routes().ServeHTTP(rec, req)
	return rec
}

func TestCompressMedia(t *testing.T) {
	// 3000x1500 noise JPEG → "small" preset must bound the long edge to 1080.
	rec := compressReq(t, "photo.jpg", "small", noiseJPEG(t, 3000, 1500))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("Content-Type = %s", ct)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(rec.Body.Bytes()))
	if err != nil || cfg.Width != 1080 || cfg.Height != 540 {
		t.Fatalf("output dims %dx%d (err=%v), want 1080x540", cfg.Width, cfg.Height, err)
	}
}

func TestCompressMediaRejectsBadPreset(t *testing.T) {
	rec := compressReq(t, "x.jpg", "huge", []byte("junk"))
	if rec.Code != 400 {
		t.Fatalf("status %d, want 400 for unknown preset", rec.Code)
	}
}

func TestCompressMediaRejectsUndecodable(t *testing.T) {
	rec := compressReq(t, "x.bin", "large", []byte("definitely not an image"))
	if rec.Code != 422 {
		t.Fatalf("status %d, want 422 for undecodable input", rec.Code)
	}
}
