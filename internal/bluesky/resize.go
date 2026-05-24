package bluesky

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// Bluesky blobs are capped at ~1 MB (lexicon allows 2 MB; we stay well under).
const maxBlobBytes = 976 * 1024

// maxBlobPixels defuses decompression bombs (a tiny header declaring gigapixel
// dimensions) before a full Decode allocates the bitmap. ~100 MP covers any
// real photo.
const maxBlobPixels = 100_000_000

// fitBlob returns image bytes guaranteed <= maxBlobBytes plus its mime and
// pixel dimensions. Inputs already under the cap pass through unchanged.
// Oversized inputs are decoded and re-encoded as JPEG, scaling down 15% per
// iteration until they fit.
func fitBlob(in []byte, mime string) (out []byte, outMime string, w, h int, err error) {
	// Reject pixel-bomb inputs up front. An over-budget image that's already
	// small enough still ships as-is (no decode needed); a large one can't be
	// safely resized, so it errors rather than risk an OOM.
	if cfg, _, cerr := image.DecodeConfig(bytes.NewReader(in)); cerr == nil &&
		int64(cfg.Width)*int64(cfg.Height) > maxBlobPixels {
		if len(in) <= maxBlobBytes {
			return in, mime, 0, 0, nil
		}
		return nil, "", 0, 0, fmt.Errorf("image %dx%d exceeds %d-pixel cap", cfg.Width, cfg.Height, maxBlobPixels)
	}
	img, _, derr := image.Decode(bytes.NewReader(in))
	if derr != nil {
		if len(in) <= maxBlobBytes {
			return in, mime, 0, 0, nil // not a decodable image, but small enough
		}
		return nil, "", 0, 0, fmt.Errorf("decode for resize: %w", derr)
	}
	b := img.Bounds()
	if len(in) <= maxBlobBytes {
		return in, mime, b.Dx(), b.Dy(), nil
	}
	// Flatten any transparency onto white before JPEG re-encode (JPEG has no alpha).
	opaque := image.NewRGBA(img.Bounds())
	draw.Draw(opaque, opaque.Bounds(), image.White, image.Point{}, draw.Src)
	draw.Draw(opaque, opaque.Bounds(), img, image.Point{}, draw.Over)
	cur := opaque
	for i := 0; i < 10; i++ {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, cur, &jpeg.Options{Quality: 85}); err != nil {
			return nil, "", 0, 0, err
		}
		cb := cur.Bounds()
		if buf.Len() <= maxBlobBytes {
			return buf.Bytes(), "image/jpeg", cb.Dx(), cb.Dy(), nil
		}
		nw, nh := cb.Dx()*85/100, cb.Dy()*85/100
		if nw < 1 || nh < 1 {
			return buf.Bytes(), "image/jpeg", cb.Dx(), cb.Dy(), nil
		}
		dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
		draw.CatmullRom.Scale(dst, dst.Bounds(), cur, cb, draw.Over, nil)
		cur = dst
	}
	return nil, "", 0, 0, fmt.Errorf("could not shrink image under %d bytes", maxBlobBytes)
}
