package transcode

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// exifJPEG encodes img as JPEG and splices in a minimal EXIF APP1 segment
// carrying only the orientation tag (0x0112).
func exifJPEG(t *testing.T, img image.Image, orientation uint16) []byte {
	t.Helper()
	return buildExifJPEG(img, orientation)
}

// cornerImg is 4x2: red top-left 2x2 block, white everywhere else.  A 2x2
// red area is the minimum that JPEG DCT (8x8 blocks) can preserve as
// distinguishably red (r>=200) after a quality-90 encode/decode cycle.
func cornerImg() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 4, 2))
	for i := range img.Pix {
		img.Pix[i] = 0xFF
	}
	img.SetRGBA(0, 0, color.RGBA{R: 0xFF, A: 0xFF})
	img.SetRGBA(1, 0, color.RGBA{R: 0xFF, A: 0xFF})
	img.SetRGBA(0, 1, color.RGBA{R: 0xFF, A: 0xFF})
	img.SetRGBA(1, 1, color.RGBA{R: 0xFF, A: 0xFF})
	return img
}

func TestJPEGOrientationParses(t *testing.T) {
	src := exifJPEG(t, cornerImg(), 6)
	if got := jpegOrientation(src); got != 6 {
		t.Fatalf("orientation = %d, want 6", got)
	}
	if got := jpegOrientation(encJPEG(t, cornerImg(), 90)); got != 1 {
		t.Fatalf("no-EXIF JPEG orientation = %d, want 1", got)
	}
}

func TestImageBakesOrientation(t *testing.T) {
	// Orientation 6 = rotate 90° CW: 4x2 with red at (0,0) becomes 2x4 with
	// red at (1,0).
	src := exifJPEG(t, cornerImg(), 6)
	r, err := Image(src, "image/jpeg", ImageParams{Format: JPEG, Quality: 90})
	if err != nil {
		t.Fatal(err)
	}
	if r.W != 2 || r.H != 4 {
		t.Fatalf("dims = %dx%d, want 2x4 after rotation", r.W, r.H)
	}
	out, err := jpeg.Decode(bytes.NewReader(r.Bytes))
	if err != nil {
		t.Fatal(err)
	}
	c := out.At(1, 0)
	cr, cg, _, _ := c.RGBA()
	if cr>>8 < 200 || cg>>8 > 120 {
		t.Fatalf("pixel (1,0) = %v, want red after 90° CW bake", c)
	}
}

func TestImageStripsEXIF(t *testing.T) {
	src := exifJPEG(t, cornerImg(), 6)
	r, err := Image(src, "image/jpeg", ImageParams{Format: JPEG, Quality: 90})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(r.Bytes, []byte("Exif\x00\x00")) {
		t.Fatal("re-encoded output still contains an EXIF segment")
	}
}

// TestApplyOrientationAll8 pins every EXIF transform exactly (no JPEG
// round-trip, so assertions are precise). Source is an asymmetric 3x2 with
// red at (0,0) ("A") and green at (2,0) ("B"); each case asserts output dims
// and both corner destinations, which discriminates all 8 transforms.
func TestApplyOrientationAll8(t *testing.T) {
	red := color.RGBA{R: 0xFF, A: 0xFF}
	green := color.RGBA{G: 0xFF, A: 0xFF}
	src := image.NewRGBA(image.Rect(0, 0, 3, 2))
	for i := range src.Pix {
		src.Pix[i] = 0xFF
	}
	src.SetRGBA(0, 0, red)
	src.SetRGBA(2, 0, green)

	cases := []struct {
		o      int
		w, h   int
		ax, ay int // where red (src 0,0) lands
		bx, by int // where green (src 2,0) lands
	}{
		{1, 3, 2, 0, 0, 2, 0},
		{2, 3, 2, 2, 0, 0, 0},
		{3, 3, 2, 2, 1, 0, 1},
		{4, 3, 2, 0, 1, 2, 1},
		{5, 2, 3, 0, 0, 0, 2},
		{6, 2, 3, 1, 0, 1, 2},
		{7, 2, 3, 1, 2, 1, 0},
		{8, 2, 3, 0, 2, 0, 0},
	}
	for _, c := range cases {
		out := applyOrientation(src, c.o)
		b := out.Bounds()
		if b.Dx() != c.w || b.Dy() != c.h {
			t.Fatalf("o=%d dims %dx%d, want %dx%d", c.o, b.Dx(), b.Dy(), c.w, c.h)
		}
		if out.At(c.ax, c.ay) != red {
			t.Fatalf("o=%d red at (%d,%d) got %v, want %v", c.o, c.ax, c.ay, out.At(c.ax, c.ay), red)
		}
		if out.At(c.bx, c.by) != green {
			t.Fatalf("o=%d green at (%d,%d) got %v, want %v", c.o, c.bx, c.by, out.At(c.bx, c.by), green)
		}
	}
}

// TestJPEGOrientationHostileInput: the parser runs on arbitrary uploads and
// URL-fetched bytes — every malformed shape must degrade to upright (1),
// never panic.
func TestJPEGOrientationHostileInput(t *testing.T) {
	valid := exifJPEG(t, cornerImg(), 6)
	cases := map[string][]byte{
		"empty":            nil,
		"not jpeg":         []byte("GIF89a"),
		"soi only":         {0xFF, 0xD8},
		"truncated app1":   valid[:20],
		"size zero":        {0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x00, 0xFF},
		"size one":         {0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x01, 0xFF},
		"bad byte order":   spliceTIFF(t, []byte("XX"), nil),
		"orientation zero": exifJPEG(t, cornerImg(), 0),
		"orientation nine": exifJPEG(t, cornerImg(), 9),
		"fill bytes":       append([]byte{0xFF, 0xD8, 0xFF, 0xFF, 0xFF}, exifJPEG(t, cornerImg(), 6)[2:]...),
	}
	for name, b := range cases {
		got := jpegOrientation(b)
		want := 1
		if name == "fill bytes" {
			want = 6 // fill bytes are legal padding; the real APP1 follows
		}
		if got != want {
			t.Fatalf("%s: orientation = %d, want %d", name, got, want)
		}
	}
	// TIFF IFD offset / entry count out of bounds.
	// Layout: SOI(2) + APP1 marker(2) + size(2) + "Exif\0\0"(6) = TIFF at byte 12.
	// IFD0 offset field = TIFF[4:8] = bytes 16–19 of the full JPEG.
	huge := exifJPEG(t, cornerImg(), 6)
	copy(huge[16:], []byte{0xFF, 0xFF, 0xFF, 0xF0})
	if got := jpegOrientation(huge); got != 1 {
		t.Fatalf("oob IFD offset: orientation = %d, want 1", got)
	}
}

// spliceTIFF builds a JPEG whose APP1 contains "Exif\0\0" + the given tiff
// header bytes (for byte-order/garbage probes).
func spliceTIFF(t *testing.T, bo, rest []byte) []byte {
	t.Helper()
	tiff := append(append([]byte{}, bo...), rest...)
	for len(tiff) < 10 {
		tiff = append(tiff, 0)
	}
	app1 := &bytes.Buffer{}
	app1.Write([]byte{0xFF, 0xE1})
	payload := append([]byte("Exif\x00\x00"), tiff...)
	binary.Write(app1, binary.BigEndian, uint16(len(payload)+2))
	app1.Write(payload)
	out := append([]byte{0xFF, 0xD8}, app1.Bytes()...)
	return append(out, 0xFF, 0xD9)
}

// buildExifJPEG encodes img as JPEG with a minimal EXIF APP1 orientation tag.
// Usable from non-test contexts (e.g. fuzz seeds) where *testing.T is unavailable.
func buildExifJPEG(img image.Image, orientation uint16) []byte {
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}) // jpeg.Encode on valid RGBA never fails
	plain := buf.Bytes()

	tiff := &bytes.Buffer{}
	tiff.WriteString("MM")
	binary.Write(tiff, binary.BigEndian, uint16(42))
	binary.Write(tiff, binary.BigEndian, uint32(8))
	binary.Write(tiff, binary.BigEndian, uint16(1))
	binary.Write(tiff, binary.BigEndian, uint16(0x0112))
	binary.Write(tiff, binary.BigEndian, uint16(3))
	binary.Write(tiff, binary.BigEndian, uint32(1))
	binary.Write(tiff, binary.BigEndian, orientation)
	binary.Write(tiff, binary.BigEndian, uint16(0))
	binary.Write(tiff, binary.BigEndian, uint32(0))

	app1 := &bytes.Buffer{}
	app1.Write([]byte{0xFF, 0xE1})
	payload := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	binary.Write(app1, binary.BigEndian, uint16(len(payload)+2))
	app1.Write(payload)

	out := append([]byte{0xFF, 0xD8}, app1.Bytes()...)
	return append(out, plain[2:]...)
}

// FuzzJPEGOrientation: result must always be in [1,8] and never panic. Seeds
// run in plain `go test`; `go test -fuzz=FuzzJPEGOrientation` digs deeper.
func FuzzJPEGOrientation(f *testing.F) {
	f.Add([]byte{0xFF, 0xD8})
	f.Add(buildExifJPEG(cornerImg(), 6))
	f.Fuzz(func(t *testing.T, b []byte) {
		if o := jpegOrientation(b); o < 1 || o > 8 {
			t.Fatalf("orientation %d out of range", o)
		}
	})
}
