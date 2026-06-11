package transcode

import (
	"bytes"
	"fmt"
	"image"
	"strconv"
	"strings"
)

// Profile is one platform's media constraints. Needs (metadata-only, cheap —
// safe on every preview refresh) and Fit (pixel work, dispatch-time) apply the
// same constraints, so preview badges and published bytes agree for decodable
// input; Fit additionally sniff-corrects lying mimes and errors on
// unconvertible violations.
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
// JPEG re-encode under the profile's ceilings otherwise. The declared mime is
// corrected by sniffing the actual bytes first (browsers and remote servers
// lie), so the format decision is made on truth; Needs, which only has
// metadata, stays optimistic — dispatch re-checking here is the contract.
func (p Profile) Fit(src []byte, mime string) (Result, error) {
	if _, format, err := image.DecodeConfig(bytes.NewReader(src)); err == nil {
		mime = "image/" + format
	}
	enc := KeepIfAllowed
	if !p.mimeOK(mime) {
		enc = JPEG
	}
	r, err := Image(src, mime, ImageParams{MaxBytes: p.MaxBytes, MaxPixels: p.MaxPixels, Format: enc, Quality: p.Quality})
	if err != nil {
		return r, err
	}
	// Undecodable input that violates the profile passed through Image()'s
	// small-input escape: fail loudly rather than ship bytes the platform is
	// guaranteed to reject. (A decodable result always satisfies the profile.)
	// Note: this Needs check omits W/H (unknown for undecodable bytes), so a
	// >100MP decodable bomb that sneaks under MaxBytes still passes through here.
	if !r.Changed {
		if need, reason := p.Needs(Meta{SizeBytes: int64(len(r.Bytes)), Mime: r.Mime}); need {
			return Result{}, fmt.Errorf("media violates %s constraints (%s) and cannot be converted", p.Name, reason)
		}
	}
	return r, nil
}

// ParseDim splits a "WxH" media-row dim string; (0,0) when malformed.
func ParseDim(dim string) (w, h int) {
	a, b, ok := strings.Cut(dim, "x")
	if !ok {
		return 0, 0
	}
	w, errW := strconv.Atoi(a)
	h, errH := strconv.Atoi(b)
	if errW != nil || errH != nil || w <= 0 || h <= 0 {
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
