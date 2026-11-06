package dispatch

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/transcode"
)

// FitNote is one planned platform-fit conversion, keyed by image ordinal.
// Serialized into the thread-preview response so the composer can badge the
// affected platform ("image 2 → JPEG (over 1.9 MB)").
type FitNote struct {
	Ordinal int    `json:"ordinal"`
	Note    string `json:"note"`
}

// PlanMediaFit reports which images will be re-encoded for a destination
// with the given media caps, from metadata alone: no pixel work, safe on
// every preview keystroke. It applies the same Profile predicate dispatch
// applies to the real bytes (prepareMedia), so preview badges and published
// bytes agree for decodable, truthfully-labeled input; lying metadata is
// re-checked (sniffed) at dispatch.
func PlanMediaFit(name string, mc destination.MediaCaps, metas []transcode.Meta) []FitNote {
	prof, ok := imageProfile(name, mc.Images)
	if !ok {
		return nil
	}
	var notes []FitNote
	for i, m := range metas {
		if strings.HasPrefix(m.Mime, "video/") {
			continue // videos are checked separately (VideoCheck)
		}
		if need, reason := prof.Needs(m); need {
			notes = append(notes, FitNote{Ordinal: i, Note: fmt.Sprintf("→ JPEG (%s)", reason)})
		}
	}
	return notes
}

// ImageRejects is why an image described by m can't be sent to a
// destination with the given media caps at all ("" when it can).
func ImageRejects(name string, mc destination.MediaCaps, m transcode.Meta) string {
	prof, ok := imageProfile(name, mc.Images)
	if !ok || strings.HasPrefix(m.Mime, "video/") {
		return ""
	}
	return prof.Rejects(m)
}

// FitAlt cuts alt text to max characters (0 = no limit), ending it with an
// ellipsis, so a long description doesn't fail the upload on a network
// that caps it (Mastodon's description_limit).
func FitAlt(alt string, max int) string {
	if max <= 0 || utf8.RuneCountInString(alt) <= max {
		return alt
	}
	rs := []rune(alt)
	return strings.TrimRightFunc(string(rs[:max-1]), unicode.IsSpace) + "…"
}
