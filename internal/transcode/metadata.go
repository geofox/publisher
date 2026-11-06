package transcode

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/jpeg"
)

// StripMetadata returns src without the metadata that can identify where and
// on what it was made: EXIF (GPS, camera, time), XMP, IPTC and text chunks.
// JPEG, PNG and WebP are rewritten losslessly, chunk by chunk; the pixels and
// the color profile (ICC) stay. A JPEG whose EXIF orientation isn't upright
// is decoded, turned, and re-encoded instead, so dropping the tag doesn't
// leave the photo sideways. Anything else, or input it can't parse, comes
// back as is with changed=false.
func StripMetadata(src []byte, mime string) (out []byte, changed bool) {
	switch {
	case isJPEG(src):
		if o := jpegOrientation(src); o > 1 && withinPixelCap(src) {
			if img, err := decode(src, "image/jpeg"); err == nil { // decode bakes the orientation in
				var buf bytes.Buffer
				if jpeg.Encode(&buf, img, &jpeg.Options{Quality: 92}) == nil {
					return buf.Bytes(), true
				}
			}
		}
		return stripJPEG(src)
	case isPNG(src):
		return stripPNG(src)
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

// stripJPEG drops APP1 (EXIF, XMP), APP13 (IPTC/Photoshop) and comment
// segments before the scan; APP0 (JFIF), APP2 (ICC) and APP14 (Adobe color
// transform) stay.
func stripJPEG(b []byte) ([]byte, bool) {
	out := make([]byte, 0, len(b))
	out = append(out, b[:2]...)
	i := 2
	dropped := false
	for i+4 <= len(b) {
		if b[i] != 0xFF {
			return b, false // not at a marker: leave the file alone
		}
		if b[i+1] == 0xFF { // fill byte
			out = append(out, b[i])
			i++
			continue
		}
		marker := b[i+1]
		if marker == 0xDA { // start of scan: the rest is image data
			out = append(out, b[i:]...)
			if !dropped {
				return b, false
			}
			return out, true
		}
		size := int(binary.BigEndian.Uint16(b[i+2:]))
		if size < 2 || i+2+size > len(b) {
			return b, false
		}
		seg := b[i : i+2+size]
		if marker == 0xE1 || marker == 0xED || marker == 0xFE {
			dropped = true
		} else {
			out = append(out, seg...)
		}
		i += 2 + size
	}
	return b, false
}

// stripPNG drops the eXIf, tEXt, iTXt, zTXt and tIME chunks.
func stripPNG(b []byte) ([]byte, bool) {
	out := make([]byte, 0, len(b))
	out = append(out, b[:8]...)
	i := 8
	dropped := false
	for i+12 <= len(b) {
		n := int(binary.BigEndian.Uint32(b[i:]))
		if n < 0 || i+12+n > len(b) {
			return b, false
		}
		typ := string(b[i+4 : i+8])
		chunk := b[i : i+12+n]
		switch typ {
		case "eXIf", "tEXt", "iTXt", "zTXt", "tIME":
			dropped = true
		default:
			out = append(out, chunk...)
		}
		i += 12 + n
		if typ == "IEND" {
			break
		}
	}
	if !dropped {
		return b, false
	}
	return out, true
}

// stripWebP drops the EXIF and XMP chunks of an extended WebP and clears
// their flags in VP8X.
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
		if n < 0 || end > len(b) {
			return b, false
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
	if vp8x >= 0 && vp8x+8 < len(out) {
		out[vp8x+8] &^= 0x08 | 0x04 // EXIF and XMP flags
	}
	binary.LittleEndian.PutUint32(out[4:], uint32(len(out)-8))
	return out, true
}
