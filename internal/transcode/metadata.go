package transcode

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/jpeg"
	"image/png"
)

// StripMetadata returns src without the metadata that can identify where and
// on what it was made: EXIF (GPS, camera, time), XMP, IPTC, C2PA/JUMBF and
// text chunks, and whatever a JPEG carries after its image (Motion Photo
// videos, MPF and gain-map images with their own EXIF). JPEG, PNG and WebP
// are rewritten losslessly, segment by segment; the pixels and the color
// profile (ICC) stay. A JPEG whose EXIF orientation isn't upright is
// decoded, turned, and re-encoded instead (keeping its ICC profile), so
// dropping the tag doesn't leave the photo sideways; one too large to decode
// keeps only its orientation tag. A JPEG or PNG the segment parser can't
// follow but the decoder can read is re-encoded rather than passed through
// with its metadata. Anything else comes back as is with changed=false.
func StripMetadata(src []byte, mime string) (out []byte, changed bool) {
	switch {
	case isJPEG(src):
		o := jpegOrientation(src)
		if o > 1 && withinPixelCap(src) {
			if out, ok := reencodeJPEG(src); ok {
				return out, true
			}
		}
		out, changed, ok := stripJPEG(src)
		if !ok {
			if withinPixelCap(src) {
				if out, ok := reencodeJPEG(src); ok {
					return out, true
				}
			}
			return src, false
		}
		if o > 1 { // too large to turn here: keep the tag that says how
			out = withOrientation(out, o)
			changed = true
		}
		return out, changed
	case isPNG(src):
		out, changed, ok := stripPNG(src)
		if !ok && withinPixelCap(src) {
			if img, err := png.Decode(bytes.NewReader(src)); err == nil {
				var buf bytes.Buffer
				if png.Encode(&buf, img) == nil {
					return buf.Bytes(), true
				}
			}
		}
		if !ok {
			return src, false
		}
		return out, changed
	case isWebP(src):
		return stripWebP(src)
	}
	return src, false
}

func withinPixelCap(src []byte) bool {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(src))
	return err == nil && int64(cfg.Width)*int64(cfg.Height) <= maxPixels
}

func isJPEG(b []byte) bool { return len(b) > 3 && b[0] == 0xFF && b[1] == 0xD8 }
func isPNG(b []byte) bool  { return len(b) > 8 && string(b[:8]) == "\x89PNG\r\n\x1a\n" }
func isWebP(b []byte) bool {
	return len(b) > 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP"
}

// reencodeJPEG decodes src (decode bakes the orientation in) and encodes it
// again with the source's ICC profile segments.
func reencodeJPEG(src []byte) ([]byte, bool) {
	img, err := decode(src, "image/jpeg")
	if err != nil {
		return nil, false
	}
	var buf bytes.Buffer
	if jpeg.Encode(&buf, img, &jpeg.Options{Quality: 92}) != nil {
		return nil, false
	}
	enc := buf.Bytes()
	icc := iccSegments(src)
	if len(icc) == 0 {
		return enc, true
	}
	out := make([]byte, 0, len(enc)+len(icc))
	out = append(out, enc[:2]...) // SOI
	out = append(out, icc...)
	return append(out, enc[2:]...), true
}

// iccSegments returns the APP2 ICC_PROFILE segments before the scan.
func iccSegments(b []byte) []byte {
	var out []byte
	i := 2
	for i+4 <= len(b) && b[i] == 0xFF {
		if b[i+1] == 0xFF {
			i++
			continue
		}
		marker := b[i+1]
		if marker == 0xDA || marker == 0xD9 {
			break
		}
		size := int(binary.BigEndian.Uint16(b[i+2:]))
		if size < 2 || i+2+size > len(b) {
			break
		}
		seg := b[i : i+2+size]
		if marker == 0xE2 && bytes.HasPrefix(seg[4:], []byte("ICC_PROFILE\x00")) {
			out = append(out, seg...)
		}
		i += 2 + size
	}
	return out
}

// withOrientation puts a minimal EXIF segment holding only the orientation
// tag right after the SOI (and any JFIF APP0).
func withOrientation(b []byte, o int) []byte {
	tiff := &bytes.Buffer{}
	tiff.WriteString("MM")
	_ = binary.Write(tiff, binary.BigEndian, uint16(42))
	_ = binary.Write(tiff, binary.BigEndian, uint32(8))
	_ = binary.Write(tiff, binary.BigEndian, uint16(1))      // one entry
	_ = binary.Write(tiff, binary.BigEndian, uint16(0x0112)) // Orientation
	_ = binary.Write(tiff, binary.BigEndian, uint16(3))      // SHORT
	_ = binary.Write(tiff, binary.BigEndian, uint32(1))
	_ = binary.Write(tiff, binary.BigEndian, uint16(o))
	_ = binary.Write(tiff, binary.BigEndian, uint16(0))
	_ = binary.Write(tiff, binary.BigEndian, uint32(0)) // no next IFD
	payload := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	seg := []byte{0xFF, 0xE1, byte((len(payload) + 2) >> 8), byte(len(payload) + 2)}
	seg = append(seg, payload...)
	at := 2
	if len(b) >= 6 && b[2] == 0xFF && b[3] == 0xE0 { // after JFIF, which must come first
		if n := int(binary.BigEndian.Uint16(b[4:])); n >= 2 && 4+n <= len(b) {
			at = 4 + n
		}
	}
	out := make([]byte, 0, len(b)+len(seg))
	out = append(out, b[:at]...)
	out = append(out, seg...)
	return append(out, b[at:]...)
}

// stripJPEG drops APP1 (EXIF, XMP), APP11 (JUMBF/C2PA), APP13
// (IPTC/Photoshop) and comment segments, and everything after the EOI;
// APP0 (JFIF), APP2 (ICC) and APP14 (Adobe color transform) stay. ok is
// false when the file can't be followed segment by segment.
func stripJPEG(b []byte) (out []byte, changed, ok bool) {
	out = make([]byte, 0, len(b))
	out = append(out, b[:2]...)
	i := 2
	for i+2 <= len(b) {
		if b[i] != 0xFF {
			return nil, false, false // not at a marker
		}
		if b[i+1] == 0xFF { // fill byte
			out = append(out, b[i])
			i++
			continue
		}
		marker := b[i+1]
		if marker == 0xD9 { // EOI: anything after it is not this image
			out = append(out, b[i:i+2]...)
			return out, changed || i+2 < len(b), true
		}
		if marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) { // TEM, RSTn: no length
			out = append(out, b[i:i+2]...)
			i += 2
			continue
		}
		if i+4 > len(b) {
			return nil, false, false
		}
		size := int(binary.BigEndian.Uint16(b[i+2:]))
		if size < 2 || i+2+size > len(b) {
			return nil, false, false
		}
		seg := b[i : i+2+size]
		i += 2 + size
		switch {
		case marker == 0xE1 || marker == 0xEB || marker == 0xED || marker == 0xFE:
			changed = true
			continue
		case marker == 0xDA:
			// The scan's entropy-coded data runs to the next marker that
			// isn't stuffing (FF00) or a restart (RSTn).
			out = append(out, seg...)
			j := i
			for j+1 < len(b) && !(b[j] == 0xFF && b[j+1] != 0x00 && !(b[j+1] >= 0xD0 && b[j+1] <= 0xD7) && b[j+1] != 0xFF) {
				j++
			}
			if j+1 >= len(b) { // no EOI: a truncated file, kept as it is
				out = append(out, b[i:]...)
				return out, changed, true
			}
			out = append(out, b[i:j]...)
			i = j
			continue
		}
		out = append(out, seg...)
	}
	return nil, false, false
}

// stripPNG drops the eXIf, tEXt, iTXt, zTXt and tIME chunks and anything
// after IEND. ok is false when the chunks can't be followed.
func stripPNG(b []byte) (out []byte, changed, ok bool) {
	out = make([]byte, 0, len(b))
	out = append(out, b[:8]...)
	i := 8
	for i+12 <= len(b) {
		n := int(binary.BigEndian.Uint32(b[i:]))
		if n < 0 || i+12+n > len(b) {
			return nil, false, false
		}
		typ := string(b[i+4 : i+8])
		chunk := b[i : i+12+n]
		i += 12 + n
		switch typ {
		case "eXIf", "tEXt", "iTXt", "zTXt", "tIME":
			changed = true
			continue
		}
		out = append(out, chunk...)
		if typ == "IEND" {
			return out, changed || i < len(b), true
		}
	}
	return nil, false, false
}

// stripWebP drops the EXIF and XMP chunks of an extended WebP and clears
// their flags in VP8X. A last chunk missing its pad byte is accepted.
func stripWebP(b []byte) ([]byte, bool) {
	out := make([]byte, 0, len(b))
	out = append(out, b[:12]...)
	i := 12
	dropped := false
	vp8x := -1
	for i+8 <= len(b) {
		typ := string(b[i : i+4])
		n := int(binary.LittleEndian.Uint32(b[i+4:]))
		end := i + 8 + n + n%2 // chunks are padded to even sizes
		if n < 0 || i+8+n > len(b) {
			return b, false
		}
		if end > len(b) { // the last chunk without its pad
			end = len(b)
		}
		switch typ {
		case "EXIF", "XMP ":
			dropped = true
		default:
			if typ == "VP8X" {
				vp8x = len(out)
			}
			out = append(out, b[i:end]...)
		}
		i = end
	}
	if !dropped {
		return b, false
	}
	if len(out)%2 == 1 {
		out = append(out, 0) // keep the RIFF size even
	}
	if vp8x >= 0 && vp8x+8 < len(out) {
		out[vp8x+8] &^= 0x08 | 0x04 // EXIF and XMP flags
	}
	binary.LittleEndian.PutUint32(out[4:], uint32(len(out)-8))
	return out, true
}
