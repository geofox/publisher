package dispatch

import (
	"fmt"

	"github.com/geofox/publisher/internal/transcode"
)

// FitNote is one planned platform-fit conversion, keyed by image ordinal.
// Serialized into the thread-preview response so the composer can badge the
// affected platform ("image 2 → JPEG (over 1.9 MB)").
type FitNote struct {
	Ordinal int    `json:"ordinal"`
	Note    string `json:"note"`
}

// PlanMediaFit reports which images will be re-encoded for plat, from
// metadata alone — no pixel work, safe on every preview keystroke. It applies
// the same Profile predicate the dispatch-time executors (fitBlob, fitImgs,
// prepThreadsImgs) apply to the real bytes, so preview badges and published
// bytes agree for decodable, truthfully-labeled input; lying metadata is
// re-checked (sniffed) at dispatch.
func PlanMediaFit(plat string, metas []transcode.Meta) []FitNote {
	prof, ok := transcode.ProfileFor(plat)
	if !ok {
		return nil
	}
	var notes []FitNote
	for i, m := range metas {
		if need, reason := prof.Needs(m); need {
			notes = append(notes, FitNote{Ordinal: i, Note: fmt.Sprintf("→ JPEG (%s)", reason)})
		}
	}
	return notes
}
