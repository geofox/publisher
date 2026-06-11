package transcode

import (
	"fmt"
	"strconv"
	"strings"
)

// Profile is one platform's media constraints. Needs (metadata-only, cheap —
// safe on every preview refresh) and Fit (pixel work, dispatch-time) apply the
// same predicate, which is what keeps preview badges and published bytes in
// agreement.
type Profile struct {
	Name      string
	MaxBytes  int64
	MaxPixels int64    // 0 = only the global decompression-bomb cap
	Mimes     []string // mimes accepted as-is; empty = any image
	Quality   int      // JPEG quality for re-encodes
}

var (
	// Bluesky: app.bsky.embed.images / .gallery lexicon maxSize is 2,000,000
	// bytes (decimal — bumped from 1 MB upstream 2026-04, atproto#4823;
	// verified against the geoffrey.cc PDS, pds:0.4.5001 / @atproto/pds 0.5.1).
	// 1952*1024 keeps the stay-just-under idiom of the old 976*1024.
	Bluesky = Profile{Name: "bluesky", MaxBytes: 1952 * 1024, Quality: 85}
	// Mastodon: conservative vanilla defaults; instances vary. A future
	// enhancement could read /api/v2/instance announced limits.
	Mastodon = Profile{Name: "mastodon", MaxBytes: 8 << 20, MaxPixels: 16_000_000, Quality: 85}
	// Threads ingests by URL and accepts only JPEG/PNG up to 8 MB (Meta image
	// spec) — a WebP canonical must be re-encoded and re-hosted.
	Threads = Profile{Name: "threads", MaxBytes: 8 << 20, Mimes: []string{"image/jpeg", "image/png"}, Quality: 85}
)

// ProfileFor returns the platform's image profile. ok=false (nostr, unknown)
// means passthrough: the platform takes the canonical object as-is.
func ProfileFor(platform string) (Profile, bool) {
	switch platform {
	case "bluesky":
		return Bluesky, true
	case "mastodon":
		return Mastodon, true
	case "threads":
		return Threads, true
	}
	return Profile{}, false
}

// Meta is the metadata-only description of a media object, as known to the
// composer (fresh file) or the media row (restored draft). Zero fields mean
// "unknown" and are treated as fitting — planning degrades to optimism, and
// dispatch re-checks against the real bytes.
type Meta struct {
	SizeBytes int64
	Mime      string
	W, H      int
}

// Needs reports whether media described by m violates the profile, with a
// human-readable reason ("" when it fits).
func (p Profile) Needs(m Meta) (bool, string) {
	if !p.mimeOK(m.Mime) {
		return true, strings.TrimPrefix(m.Mime, "image/") + " not accepted"
	}
	if p.MaxBytes > 0 && m.SizeBytes > p.MaxBytes {
		return true, fmt.Sprintf("over %s", fmtBytes(p.MaxBytes))
	}
	if p.MaxPixels > 0 && m.W > 0 && int64(m.W)*int64(m.H) > p.MaxPixels {
		return true, fmt.Sprintf("over %d MP", p.MaxPixels/1_000_000)
	}
	return false, ""
}

func (p Profile) mimeOK(mime string) bool {
	if len(p.Mimes) == 0 || mime == "" {
		return true
	}
	for _, m := range p.Mimes {
		if mime == m {
			return true
		}
	}
	return false
}

// Fit returns src unchanged when it satisfies the profile and a deterministic
// JPEG re-encode under the profile's ceilings otherwise. Note: when the
// profile has a pixel cap, the cap is translated to a square long-edge bound —
// an extreme panorama under the area cap but over that edge gets downscaled
// slightly more than strictly necessary; the output is still valid everywhere.
func (p Profile) Fit(src []byte, mime string) (Result, error) {
	maxEdge := 0
	if p.MaxPixels > 0 {
		maxEdge = intSqrt(p.MaxPixels)
	}
	format := KeepIfAllowed
	if !p.mimeOK(mime) {
		format = JPEG
	}
	return Image(src, mime, ImageParams{MaxBytes: p.MaxBytes, MaxLongEdge: maxEdge, Format: format, Quality: p.Quality})
}

func intSqrt(n int64) int {
	x := int64(1)
	for x*x <= n {
		x++
	}
	return int(x - 1)
}

// ParseDim splits a "WxH" media-row dim string; (0,0) when malformed.
func ParseDim(dim string) (w, h int) {
	a, b, ok := strings.Cut(dim, "x")
	if !ok {
		return 0, 0
	}
	w, _ = strconv.Atoi(a)
	h, _ = strconv.Atoi(b)
	if w < 0 || h < 0 {
		return 0, 0
	}
	return w, h
}

func fmtBytes(n int64) string {
	if n >= 1<<20 {
		mb := float64(n) / (1 << 20)
		if mb == float64(int64(mb)) {
			return fmt.Sprintf("%d MB", int64(mb))
		}
		return fmt.Sprintf("%.1f MB", mb)
	}
	return fmt.Sprintf("%d KB", n/1024)
}
