package dispatch

import (
	"fmt"
	"strings"

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
