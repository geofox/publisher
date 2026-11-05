package transcode

import (
	"testing"
)

func TestFitVideoDims(t *testing.T) {
	localMax := func(a, b int) int {
		if a > b {
			return a
		}
		return b
	}

	cases := []struct {
		name         string
		w, h         int
		preset       string
		wantW, wantH int
	}{
		{"4k landscape to 1080p", 3840, 2160, "1080p", 1920, 1080},
		{"4k portrait to 1080p", 2160, 3840, "1080p", 1080, 1920},
		{"already small, no upscale", 640, 360, "1080p", 640, 360},
		{"landscape 720p box", 3840, 2160, "720p", 1280, 720},
		{"portrait 480p box", 1080, 1920, "480p", 480, 854},
		{"odd dims rounded even", 641, 361, "1080p", 640, 360},
		{"non-16:9 fits inside box", 4000, 3000, "1080p", 1440, 1080},
		{"degenerate wide", 10000, 10, "1080p", 1920, 2},
		{"one-pixel tall", 100, 1, "1080p", 100, 2},
		{"box boundary identity", 1920, 1080, "1080p", 1920, 1080},
		// 1919x1081: h exceeds box (1081>1080) so no-scale path skipped.
		// pin-width ineligible (w=1919 < bw=1920); pin-height: qw=(1919*1080/1081)&^1=1916, qh=1080.
		// Result: 1916x1080 — within box, even, ≤ input on both axes, AR-reasonable.
		{"near-boundary odd no-upscale", 1919, 1081, "1080p", 1916, 1080},
	}
	for _, c := range cases {
		gw, gh := FitVideoDims(c.w, c.h, c.preset)
		if gw != c.wantW || gh != c.wantH {
			t.Fatalf("%s: got %dx%d want %dx%d", c.name, gw, gh, c.wantW, c.wantH)
		}
		if gw%2 != 0 || gh%2 != 0 {
			t.Fatalf("%s: odd output %dx%d", c.name, gw, gh)
		}
		// No-upscale property: output must not exceed input on either axis
		// (floor-2 exception for degenerate sources smaller than 2px).
		if gw > localMax(c.w, 2) || gh > localMax(c.h, 2) {
			t.Fatalf("%s: upscaled %dx%d from %dx%d", c.name, gw, gh, c.w, c.h)
		}
	}
	// Unknown preset must fall back to 1080p — large input gets scaled down.
	if w, h := FitVideoDims(3840, 2160, "bogus"); w != 1920 || h != 1080 {
		t.Fatalf("unknown preset must fall back to 1080p, got %dx%d", w, h)
	}
}
