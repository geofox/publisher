package transcode

import (
	"os"
	"testing"
)

// testdata/sample.heic is a synthetic 64x48 gradient generated with heif-enc
// (libheif) — no third-party image rights involved.
func TestIsHEIC(t *testing.T) {
	b, err := os.ReadFile("testdata/sample.heic")
	if err != nil {
		t.Fatal(err)
	}
	if !IsHEIC("", b) {
		t.Fatal("fixture not sniffed as HEIC from bytes")
	}
	if !IsHEIC("image/heic", nil) || !IsHEIC("image/heif", nil) {
		t.Fatal("mime-declared HEIC not detected")
	}
	if IsHEIC("image/jpeg", []byte("\xFF\xD8\xFF")) {
		t.Fatal("JPEG misdetected as HEIC")
	}
}

func TestImageConvertsHEIC(t *testing.T) {
	b, err := os.ReadFile("testdata/sample.heic")
	if err != nil {
		t.Fatal(err)
	}
	r, err := Image(b, "image/heic", ImageParams{Format: JPEG, Quality: 90})
	if err != nil {
		t.Fatal(err)
	}
	if r.Mime != "image/jpeg" || !r.Changed || r.W == 0 || r.H == 0 {
		t.Fatalf("HEIC must convert to JPEG with dims: %+v", r)
	}
}

func TestImageCorruptHEICWithJPEGFormatPassesThroughUnchanged(t *testing.T) {
	// A corrupt HEIC that can't decode falls into Image()'s
	// undecodable-passthrough (no MaxBytes set ⇒ under cap). Callers that
	// must guarantee JPEG out (the ingest safety net) detect this via
	// Changed/Mime — pin that contract here.
	corrupt := []byte("\x00\x00\x00\x18ftypheic garbage garbage")
	r, err := Image(corrupt, "image/heic", ImageParams{Format: JPEG, Quality: 90})
	if err != nil {
		t.Fatal(err)
	}
	if r.Changed || r.Mime != "image/heic" {
		t.Fatalf("corrupt HEIC must pass through unchanged: %+v", r)
	}
}
