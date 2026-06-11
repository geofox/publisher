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
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	plain := buf.Bytes()

	// TIFF body, big-endian: header + one-entry IFD0.
	tiff := &bytes.Buffer{}
	tiff.WriteString("MM")                               // big-endian
	binary.Write(tiff, binary.BigEndian, uint16(42))     // TIFF magic
	binary.Write(tiff, binary.BigEndian, uint32(8))      // IFD0 offset
	binary.Write(tiff, binary.BigEndian, uint16(1))      // 1 entry
	binary.Write(tiff, binary.BigEndian, uint16(0x0112)) // Orientation
	binary.Write(tiff, binary.BigEndian, uint16(3))      // SHORT
	binary.Write(tiff, binary.BigEndian, uint32(1))      // count
	binary.Write(tiff, binary.BigEndian, orientation)    // value
	binary.Write(tiff, binary.BigEndian, uint16(0))      // value padding
	binary.Write(tiff, binary.BigEndian, uint32(0))      // next IFD: none

	app1 := &bytes.Buffer{}
	app1.Write([]byte{0xFF, 0xE1})
	payload := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	binary.Write(app1, binary.BigEndian, uint16(len(payload)+2))
	app1.Write(payload)

	// Splice right after SOI (FF D8).
	out := append([]byte{0xFF, 0xD8}, app1.Bytes()...)
	return append(out, plain[2:]...)
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
