package dispatch

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/transcode"
)

// Mastodon refuses media whose description is over its limit: the alt is
// cut to fit instead of failing the upload.
func TestFitAltCutsToTheLimit(t *testing.T) {
	long := strings.Repeat("é", 1600)
	got := FitAlt(long, 1500)
	if n := utf8.RuneCountInString(got); n != 1500 || !strings.HasSuffix(got, "…") {
		t.Errorf("cut alt has %d characters (%q…)", n, got[len(got)-8:])
	}
	if FitAlt("short", 1500) != "short" || FitAlt(long, 0) != long {
		t.Error("an alt within the limit, or with no limit, must stay as is")
	}
}

// Threads refuses images wider than 10:1, which no re-encode fixes.
func TestImageRejectsThreadsAspect(t *testing.T) {
	mc := destination.MediaCaps{Images: destination.Limit{Supported: true, MaxAspect: 10}}
	if why := ImageRejects("Threads", mc, transcode.Meta{Mime: "image/jpeg", W: 3300, H: 300}); why == "" {
		t.Error("an 11:1 image passed for Threads")
	}
	if why := ImageRejects("Threads", mc, transcode.Meta{Mime: "image/jpeg", W: 3000, H: 300}); why != "" {
		t.Errorf("a 10:1 image was refused: %s", why)
	}
}
