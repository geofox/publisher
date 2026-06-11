// Package transcode is the single home for media re-encoding: the on-demand
// composer compression, the HEIC ingest conversion, and the per-platform fit
// applied at dispatch. Functions are pure — identical input bytes and params
// yield identical output bytes — which is what lets dispatch, thread-preview
// and the retrier derive the same variant independently (preview == post ==
// retry, the link-card idiom).
package transcode

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// maxPixels defuses decompression bombs (a tiny header declaring gigapixel
// dimensions) before a full Decode allocates the bitmap. ~100 MP covers any
// real photo. Single authority — internal/bluesky and internal/media used to
// carry their own copies.
const maxPixels = 100_000_000

// maxFitIterations bounds the downscale ladder; with 15% shrink per round the
// pixel count drops ~28%/round, so 10 rounds ≈ 25× total byte reduction.
const maxFitIterations = 10

type Format int

const (
	// KeepIfAllowed passes the source through untouched when it already
	// satisfies the params; re-encodes to JPEG otherwise. Used by the
	// platform-fit profiles.
	KeepIfAllowed Format = iota
	// JPEG always re-encodes. Used by the on-demand presets so the output
	// format is predictable and metadata is always stripped.
	JPEG
)

type ImageParams struct {
	MaxBytes    int64 // output byte ceiling; 0 = none
	MaxLongEdge int   // output long-edge ceiling in px; 0 = keep dimensions
	Format      Format
	Quality     int // JPEG quality for re-encodes
}

type Result struct {
	Bytes []byte
	Mime  string
	W, H  int // 0 when unknown (undecodable passthrough)
	// Changed reports whether Bytes differ from the input. False means the
	// caller may keep using its original buffer/URL.
	Changed bool
}

// Image transcodes src according to p. Any re-encode decodes the bitmap, bakes
// the EXIF orientation in, flattens alpha onto white and emits JPEG — so
// transcoded output never carries metadata (EXIF/GPS stripped by construction).
// Undecodable or pixel-bomb input passes through untouched when it already
// fits p.MaxBytes (the old fitBlob contract) and errors otherwise.
func Image(src []byte, mime string, p ImageParams) (Result, error) {
	underCap := p.MaxBytes == 0 || int64(len(src)) <= p.MaxBytes

	cfg, _, cerr := image.DecodeConfig(bytes.NewReader(src))
	if cerr == nil && int64(cfg.Width)*int64(cfg.Height) > maxPixels {
		if underCap {
			return Result{Bytes: src, Mime: mime}, nil
		}
		return Result{}, fmt.Errorf("image %dx%d exceeds %d-pixel cap", cfg.Width, cfg.Height, maxPixels)
	}
	if p.Format == KeepIfAllowed && underCap &&
		cerr == nil && edgeOK(cfg.Width, cfg.Height, p.MaxLongEdge) {
		return Result{Bytes: src, Mime: mime, W: cfg.Width, H: cfg.Height}, nil
	}

	img, err := decode(src, mime)
	if err != nil {
		if underCap {
			return Result{Bytes: src, Mime: mime}, nil
		}
		return Result{}, fmt.Errorf("transcode decode: %w", err)
	}

	cur := flattenToWhite(img)
	if !edgeOK(cur.Bounds().Dx(), cur.Bounds().Dy(), p.MaxLongEdge) {
		cur = scaleToEdge(cur, p.MaxLongEdge)
	}
	for i := 0; i < maxFitIterations; i++ {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, cur, &jpeg.Options{Quality: p.Quality}); err != nil {
			return Result{}, err
		}
		b := cur.Bounds()
		if p.MaxBytes == 0 || int64(buf.Len()) <= p.MaxBytes {
			return Result{Bytes: buf.Bytes(), Mime: "image/jpeg", W: b.Dx(), H: b.Dy(), Changed: true}, nil
		}
		nw, nh := b.Dx()*85/100, b.Dy()*85/100
		if nw < 1 || nh < 1 {
			return Result{Bytes: buf.Bytes(), Mime: "image/jpeg", W: b.Dx(), H: b.Dy(), Changed: true}, nil
		}
		dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
		draw.CatmullRom.Scale(dst, dst.Bounds(), cur, b, draw.Over, nil)
		cur = dst
	}
	return Result{}, fmt.Errorf("could not shrink image under %d bytes", p.MaxBytes)
}

func edgeOK(w, h, maxEdge int) bool {
	if maxEdge == 0 {
		return true
	}
	return w <= maxEdge && h <= maxEdge
}

// flattenToWhite composites img over a white background (JPEG has no alpha).
func flattenToWhite(img image.Image) *image.RGBA {
	opaque := image.NewRGBA(img.Bounds())
	draw.Draw(opaque, opaque.Bounds(), image.White, image.Point{}, draw.Src)
	draw.Draw(opaque, opaque.Bounds(), img, img.Bounds().Min, draw.Over)
	return opaque
}

func scaleToEdge(img *image.RGBA, maxEdge int) *image.RGBA {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	long := w
	if h > w {
		long = h
	}
	nw, nh := w*maxEdge/long, h*maxEdge/long
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Over, nil)
	return dst
}

// decode dispatches to the right decoder and bakes JPEG EXIF orientation in.
// HEIC needs an explicit decoder (gen2brain/heic registers no format); see
// heic.go. Orientation: libheif applies HEIC transforms itself, and PNG/WebP
// don't carry EXIF orientation in practice, so only JPEG needs the bake.
func decode(src []byte, mime string) (image.Image, error) {
	if IsHEIC(mime, src) {
		return decodeHEIC(src)
	}
	img, format, err := image.Decode(bytes.NewReader(src))
	if err != nil {
		return nil, err
	}
	if format == "jpeg" {
		if o := jpegOrientation(src); o > 1 {
			img = applyOrientation(img, o)
		}
	}
	return img, nil
}

// Stubs — replaced by orient.go (Task 2) and heic.go (Task 3).
func jpegOrientation([]byte) int                        { return 1 }
func applyOrientation(i image.Image, _ int) image.Image { return i }
func IsHEIC(string, []byte) bool                        { return false }
func decodeHEIC([]byte) (image.Image, error)            { return nil, fmt.Errorf("heic: not wired") }
