package transcode

import "fmt"

// videoBoxes are the landscape bounding boxes per composer preset; portrait
// input gets the box transposed. Unknown presets fall back to 1080p.
var videoBoxes = map[string][2]int{
	"1080p": {1920, 1080},
	"720p":  {1280, 720},
	"480p":  {854, 480},
}

// FitVideoDims fits (w,h) inside the preset's box, preserving aspect ratio,
// never upscaling, rounding DOWN to even (libx264 yuv420p needs even dims).
// Inputs are DISPLAY dimensions (Probe rotation-corrects them), so portrait
// phone video lands in the transposed box with no rotation handling here.
func FitVideoDims(w, h int, preset string) (int, int) {
	box, ok := videoBoxes[preset]
	if !ok {
		box = videoBoxes["1080p"]
	}
	bw, bh := box[0], box[1]
	if h > w { // portrait: transpose the box
		bw, bh = bh, bw
	}
	if w <= bw && h <= bh {
		return w &^ 1, h &^ 1
	}
	// Try both pin-width and pin-height; pick whichever result fits inside the
	// box (integer division can cause the other to exceed by a pixel or two).
	// Prefer the larger area when both fit.
	pw, ph := bw, (h*bw/w)&^1
	qw, qh := (w*bh/h)&^1, bh
	pwFits := pw <= bw && ph <= bh
	qwFits := qw <= bw && qh <= bh
	if pwFits && qwFits {
		if pw*ph >= qw*qh {
			return pw, ph
		}
		return qw, qh
	}
	if pwFits {
		return pw, ph
	}
	return qw, qh
}

// VideoInfo is the metadata-only description used by the per-platform gates
// (preview + dispatch share this — same philosophy as Profile.Needs for
// images). Zero fields are unknown and pass (optimistic planning).
type VideoInfo struct {
	SizeBytes    int64
	DurationSecs int64
	W, H         int
}

// Platform video ceilings, verified 2026-06-11 (see phase-2 spec §2).
const (
	BlueskyVideoMaxBytes     = 100_000_000 // app.bsky.embed.video lexicon
	BlueskyVideoAdvisorySecs = 180         // official pipeline norm; not lexicon-enforced
	MastodonVideoMaxBytes    = 103_809_024 // announced by the operator's instance (99 MB)
	ThreadsVideoMaxBytes     = 1 << 30
	ThreadsVideoMaxSecs      = 300
)

// VideoGate reports a hard per-platform failure reason ("" = none) and any
// advisory warnings. Hard failures fail the target at dispatch (before any
// network call) and badge the preview; advisories only badge.
func VideoGate(plat string, v VideoInfo) (fail string, warns []string) {
	switch plat {
	case "bluesky":
		if v.SizeBytes > BlueskyVideoMaxBytes {
			fail = "video over 100 MB (Bluesky blob cap)"
		}
		if v.DurationSecs > BlueskyVideoAdvisorySecs {
			warns = append(warns, "over 3 min — Bluesky may reject or truncate playback")
		}
	case "mastodon":
		if v.SizeBytes > MastodonVideoMaxBytes {
			fail = "video over 99 MB (instance cap)"
		}
	case "threads":
		if v.SizeBytes > ThreadsVideoMaxBytes {
			fail = "video over 1 GB (Threads cap)"
		}
		if v.DurationSecs > ThreadsVideoMaxSecs {
			fail = joinFail(fail, fmt.Sprintf("video over 300 s (%ds)", v.DurationSecs))
		}
	}
	return fail, warns
}

func joinFail(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}
