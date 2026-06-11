package transcode

import (
	"bytes"
	"fmt"
	"image"

	"github.com/gen2brain/heic"
)

func init() {
	// Force the WASM decoder: the optional dynamic-library path (purego +
	// system libheif) would make decode output depend on whatever lib version
	// the host has, breaking the determinism contract — and the scratch
	// container has no system libs anyway. Importing this package also registers
	// the library's heic format process-wide, so image.Decode elsewhere (bluesky
	// resize, media meta extraction) gains heic-brand decoding as a side effect.
	heic.ForceWasmMode = true
}

// heicBrands are the ISO-BMFF ftyp major brands that mean HEIC/HEIF stills.
// mif1/msf1 are detect-only: this library's decoder rejects them (libheif
// 'maybe' filetype), so they classify as HEIC for ingest policy but fail decode
// cleanly — exactly the pinned corrupt-HEIC passthrough contract.
var heicBrands = map[string]bool{
	"heic": true, "heix": true, "heim": true, "heis": true,
	"hevc": true, "hevx": true, "mif1": true, "msf1": true,
}

// IsHEIC reports whether the media is HEIC/HEIF, by declared mime or by
// sniffing the ISO-BMFF ftyp box (browsers often upload HEIC with an empty or
// generic Content-Type).
func IsHEIC(mime string, b []byte) bool {
	if mime == "image/heic" || mime == "image/heif" {
		return true
	}
	if len(b) < 12 || !bytes.Equal(b[4:8], []byte("ftyp")) {
		return false
	}
	return heicBrands[string(b[8:12])]
}

// decodeHEIC decodes with its own pixel-bomb guard: the generic guard in
// Image() only covers brands the library registers with image.DecodeConfig
// (heic), so this is the only guard for the other ftyp brands.
func decodeHEIC(src []byte) (image.Image, error) {
	cfg, err := heic.DecodeConfig(bytes.NewReader(src))
	if err != nil {
		return nil, err
	}
	if int64(cfg.Width)*int64(cfg.Height) > maxPixels {
		return nil, fmt.Errorf("heic %dx%d exceeds %d-pixel cap", cfg.Width, cfg.Height, maxPixels)
	}
	return heic.Decode(bytes.NewReader(src))
}
