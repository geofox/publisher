package transcode

import (
	"strings"
	"testing"
)

func TestPresetParams(t *testing.T) {
	for name, edge := range map[string]int{"convert": 0, "large": 2048, "medium": 1600, "small": 1080} {
		p, ok := PresetParams(name)
		if !ok {
			t.Fatalf("preset %q missing", name)
		}
		if p.MaxLongEdge != edge || p.Format != JPEG {
			t.Fatalf("preset %q = %+v, want JPEG with edge %d", name, p, edge)
		}
	}
	if _, ok := PresetParams("original"); ok {
		t.Fatal(`"original" must not be a server preset (client-side revert)`)
	}
}

func TestProfileNeeds(t *testing.T) {
	cases := []struct {
		plat   string
		m      Meta
		need   bool
		reason string
	}{
		{"bluesky", Meta{SizeBytes: 1952 * 1024, Mime: "image/jpeg"}, false, ""},
		{"bluesky", Meta{SizeBytes: 1952*1024 + 1, Mime: "image/jpeg"}, true, "over"},
		{"mastodon", Meta{SizeBytes: 8<<20 + 1, Mime: "image/jpeg"}, true, "over"},
		{"mastodon", Meta{SizeBytes: 1000, Mime: "image/jpeg", W: 6000, H: 3000}, true, "16 MP"},
		{"threads", Meta{SizeBytes: 1000, Mime: "image/webp"}, true, "webp"},
		{"threads", Meta{SizeBytes: 1000, Mime: "image/png"}, false, ""},
		// Unknown metadata plans optimistically; dispatch re-checks real bytes.
		{"threads", Meta{Mime: ""}, false, ""},
	}
	for _, c := range cases {
		prof, ok := ProfileFor(c.plat)
		if !ok {
			t.Fatalf("no profile for %s", c.plat)
		}
		need, reason := prof.Needs(c.m)
		if need != c.need || (c.reason != "" && !strings.Contains(reason, c.reason)) {
			t.Fatalf("%s %+v → need=%v reason=%q, want need=%v reason~%q", c.plat, c.m, need, reason, c.need, c.reason)
		}
	}
	if _, ok := ProfileFor("nostr"); ok {
		t.Fatal("nostr must have no profile (passthrough)")
	}
}

func TestProfileFitThreadsConvertsWebP(t *testing.T) {
	src := encPNG(t, noiseImg(t, 50, 50))
	// Declared mime drives the Needs check; the decoder sniffs real bytes for
	// the re-encode. PNG bytes declared as webp = cheap way to force the
	// format-violation path without a webp encoder.
	r, err := Threads.Fit(src, "image/webp")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Changed || r.Mime != "image/jpeg" {
		t.Fatalf("webp-for-threads must re-encode to JPEG, got changed=%v mime=%s", r.Changed, r.Mime)
	}
	r2, err := Threads.Fit(src, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if r2.Changed {
		t.Fatal("small PNG already satisfies threads — must pass through")
	}
}

func TestProfileFitBlueskyCeiling(t *testing.T) {
	src := encPNG(t, noiseImg(t, 1800, 1800)) // > 2 MB as PNG
	if int64(len(src)) <= Bluesky.MaxBytes {
		t.Skip("fixture unexpectedly small")
	}
	r, err := Bluesky.Fit(src, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(r.Bytes)) > Bluesky.MaxBytes {
		t.Fatalf("fitted %d > ceiling %d", len(r.Bytes), Bluesky.MaxBytes)
	}
	if r.Mime != "image/jpeg" {
		t.Fatalf("oversize re-encode mime = %s, want image/jpeg", r.Mime)
	}
}

func TestParseDim(t *testing.T) {
	w, h := ParseDim("1200x800")
	if w != 1200 || h != 800 {
		t.Fatalf("ParseDim = %d,%d", w, h)
	}
	if w, h := ParseDim("garbage"); w != 0 || h != 0 {
		t.Fatalf("bad dim must yield 0,0, got %d,%d", w, h)
	}
}
