package transcode

import (
	"encoding/binary"
	"image"
)

// jpegOrientation extracts the EXIF orientation (tag 0x0112) from a JPEG, or 1
// (upright) when absent/unparseable. Hand-rolled rather than a dependency: we
// need exactly one SHORT tag from IFD0 and nothing else.
func jpegOrientation(b []byte) int {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return 1
	}
	i := 2
	for i+4 <= len(b) && b[i] == 0xFF {
		// Skip 0xFF fill bytes (legal padding before any marker).
		if b[i+1] == 0xFF {
			i++
			continue
		}
		marker, size := b[i+1], int(binary.BigEndian.Uint16(b[i+2:]))
		if marker == 0xDA { // start-of-scan: no more metadata segments follow
			return 1
		}
		if size < 2 { // malformed length field — bail upright
			return 1
		}
		if marker == 0xE1 && i+4+size-2 <= len(b) {
			if o := exifOrientation(b[i+4 : i+2+size]); o > 0 {
				return o
			}
		}
		i += 2 + size
	}
	return 1
}

// exifOrientation parses an APP1 payload ("Exif\0\0" + TIFF) for tag 0x0112.
// Returns 0 when not found.
func exifOrientation(p []byte) int {
	if len(p) < 14 || string(p[:6]) != "Exif\x00\x00" {
		return 0
	}
	tiff := p[6:]
	if len(tiff) < 8 {
		return 0
	}
	var bo binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 0
	}
	if bo.Uint16(tiff[2:]) != 42 {
		return 0
	}
	off := int(bo.Uint32(tiff[4:]))
	if off < 8 || off+2 > len(tiff) {
		return 0
	}
	n := int(bo.Uint16(tiff[off:]))
	for e := 0; e < n; e++ {
		entry := off + 2 + e*12
		if entry+12 > len(tiff) {
			return 0
		}
		// Type SHORT (3) only — deliberate strictness. Some broken writers
		// emit LONG (4); they fail safe to upright rather than risk a
		// misparsed value.
		if bo.Uint16(tiff[entry:]) == 0x0112 && bo.Uint16(tiff[entry+2:]) == 3 {
			o := int(bo.Uint16(tiff[entry+8:]))
			if o >= 1 && o <= 8 {
				return o
			}
		}
	}
	return 0
}

// applyOrientation bakes an EXIF orientation (2–8) into the bitmap so the
// re-encoded JPEG renders upright without the (stripped) tag.
func applyOrientation(img image.Image, o int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	ow, oh := w, h
	if o >= 5 { // transpositions swap the axes
		ow, oh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, ow, oh))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var dx, dy int
			switch o {
			case 2: // mirror horizontal
				dx, dy = w-1-x, y
			case 3: // rotate 180
				dx, dy = w-1-x, h-1-y
			case 4: // mirror vertical
				dx, dy = x, h-1-y
			case 5: // mirror horizontal + rotate 270 CW (transpose)
				dx, dy = y, x
			case 6: // rotate 90 CW
				dx, dy = h-1-y, x
			case 7: // mirror horizontal + rotate 90 CW (transverse)
				dx, dy = h-1-y, w-1-x
			case 8: // rotate 270 CW
				dx, dy = y, w-1-x
			default:
				dx, dy = x, y
			}
			dst.Set(dx, dy, img.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}
