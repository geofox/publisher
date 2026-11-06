package transcode

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/jpeg"
	"testing"
)

func plainJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 0x80
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func segment(marker byte, payload []byte) []byte {
	s := []byte{0xFF, marker, 0, 0}
	binary.BigEndian.PutUint16(s[2:], uint16(len(payload)+2))
	return append(s, payload...)
}

// withSegs inserts segments right after a JPEG's SOI.
func withSegs(j []byte, segs ...[]byte) []byte {
	out := append([]byte{}, j[:2]...)
	for _, s := range segs {
		out = append(out, s...)
	}
	return append(out, j[2:]...)
}

func orientationExif(o int) []byte {
	return withOrientation([]byte{0xFF, 0xD8}, o)[2:]
}

// A secondary image (MPF, gain map) or a Motion Photo video after the EOI
// carries its own EXIF: it goes.
func TestStripJPEGDropsTrailingImages(t *testing.T) {
	gps := segment(0xE1, []byte("Exif\x00\x00SECONDARY-GPS"))
	src := append(withSegs(plainJPEG(t, 8, 8), segment(0xE1, []byte("Exif\x00\x00PRIMARY-GPS"))), withSegs(plainJPEG(t, 4, 4), gps)...)
	out, changed := StripMetadata(src, "image/jpeg")
	if !changed || bytes.Contains(out, []byte("GPS")) {
		t.Fatalf("changed=%v, GPS left=%v", changed, bytes.Contains(out, []byte("GPS")))
	}
	if _, err := jpeg.Decode(bytes.NewReader(out)); err != nil {
		t.Fatalf("stripped file doesn't decode: %v", err)
	}
}

func TestStripJPEGDropsC2PA(t *testing.T) {
	src := withSegs(plainJPEG(t, 8, 8), segment(0xEB, []byte("JP\x00\x00jumb c2pa location")))
	out, changed := StripMetadata(src, "image/jpeg")
	if !changed || bytes.Contains(out, []byte("c2pa")) {
		t.Fatal("APP11 (JUMBF/C2PA) kept")
	}
}

// One the segment parser can't follow is re-encoded, not passed through.
func TestStripJPEGUnparseableIsReencoded(t *testing.T) {
	j := withSegs(plainJPEG(t, 8, 8), segment(0xE1, []byte("Exif\x00\x00GPSLatitude")))
	bad := append(append(append([]byte{}, j[:2]...), 0x00), j[2:]...) // a stray byte before the first marker
	if _, err := jpeg.Decode(bytes.NewReader(bad)); err != nil {
		t.Skipf("decoder refuses it too: %v", err)
	}
	out, changed := StripMetadata(bad, "image/jpeg")
	if !changed || bytes.Contains(out, []byte("GPSLatitude")) {
		t.Fatalf("changed=%v, GPS left=%v", changed, bytes.Contains(out, []byte("GPSLatitude")))
	}
}

// Turning a JPEG upright keeps its color profile.
func TestStripJPEGTurnKeepsICC(t *testing.T) {
	icc := segment(0xE2, append([]byte("ICC_PROFILE\x00\x01\x01"), bytes.Repeat([]byte{7}, 64)...))
	src := withSegs(plainJPEG(t, 16, 8), orientationExif(6), icc)
	out, changed := StripMetadata(src, "image/jpeg")
	if !changed || !bytes.Contains(out, []byte("ICC_PROFILE")) {
		t.Fatalf("changed=%v, ICC kept=%v", changed, bytes.Contains(out, []byte("ICC_PROFILE")))
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(out))
	if err != nil || cfg.Width != 8 || cfg.Height != 16 {
		t.Fatalf("cfg=%+v err=%v, want 8x16 upright", cfg, err)
	}
}

// withOrientation's segment reads back as that orientation and nothing else.
func TestWithOrientationRoundTrips(t *testing.T) {
	src := withSegs(plainJPEG(t, 8, 8), segment(0xE1, []byte("Exif\x00\x00GPSLatitude")))
	out, _, ok := stripJPEG(src)
	if !ok {
		t.Fatal("not parsed")
	}
	out = withOrientation(out, 8)
	if o := jpegOrientation(out); o != 8 {
		t.Fatalf("orientation = %d, want 8", o)
	}
	if bytes.Contains(out, []byte("GPS")) {
		t.Fatal("GPS left")
	}
	if _, err := jpeg.Decode(bytes.NewReader(out)); err != nil {
		t.Fatal(err)
	}
}
