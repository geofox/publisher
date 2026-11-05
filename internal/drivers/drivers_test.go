package drivers

import (
	"strings"
	"testing"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/dispatch"
	"github.com/geofox/publisher/internal/transcode"
)

func manifest(t *testing.T, typ string) destination.Manifest {
	t.Helper()
	Register()
	d, ok := destination.Lookup(typ)
	if !ok {
		t.Fatalf("%s not registered", typ)
	}
	return d.Manifest()
}

func TestRegisterIsIdempotent(t *testing.T) {
	Register()
	Register()
	want := []string{"bluesky", "mastodon", "nostr", "threads"}
	if got := destination.Types(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Types() = %v, want %v", got, want)
	}
}

// The limits the splitter and the composer use come from the manifests.
func TestTextAndImageLimits(t *testing.T) {
	cases := []struct {
		typ         string
		chars, imgs int
		transport   string
		linkPreview string
	}{
		{"bluesky", 300, 10, destination.TransportBytes, destination.LinkPreviewClient},
		{"mastodon", 500, 4, destination.TransportBytes, destination.LinkPreviewServer},
		{"threads", 500, 10, destination.TransportURL, destination.LinkPreviewServer},
		{"nostr", 0, 0, destination.TransportURL, destination.LinkPreviewNone},
	}
	for _, c := range cases {
		caps := manifest(t, c.typ).Defaults
		if caps.Text.MaxLength != c.chars || caps.Media.Images.MaxCount != c.imgs {
			t.Errorf("%s: %d chars, %d images; want %d, %d", c.typ, caps.Text.MaxLength, caps.Media.Images.MaxCount, c.chars, c.imgs)
		}
		if caps.Media.Transport != c.transport || caps.LinkPreview != c.linkPreview {
			t.Errorf("%s: transport %q, link preview %q; want %q, %q", c.typ, caps.Media.Transport, caps.LinkPreview, c.transport, c.linkPreview)
		}
		if caps.Text.Counting.Unit != destination.UnitGraphemes {
			t.Errorf("%s counts %q, want graphemes", c.typ, caps.Text.Counting.Unit)
		}
	}
}

// Image limits plan the same re-encodes the transcode profiles did.
func TestImageFitPlans(t *testing.T) {
	cases := []struct {
		typ    string
		m      transcode.Meta
		reason string // "" = fits
	}{
		{"bluesky", transcode.Meta{SizeBytes: 1952 * 1024, Mime: "image/jpeg"}, ""},
		{"bluesky", transcode.Meta{SizeBytes: 1952*1024 + 1, Mime: "image/jpeg"}, "over"},
		{"mastodon", transcode.Meta{SizeBytes: 8<<20 + 1, Mime: "image/jpeg"}, "over"},
		{"mastodon", transcode.Meta{SizeBytes: 1000, Mime: "image/jpeg", W: 6000, H: 3000}, "16 MP"},
		{"threads", transcode.Meta{SizeBytes: 1000, Mime: "image/webp"}, "webp"},
		{"threads", transcode.Meta{SizeBytes: 1000, Mime: "image/png"}, ""},
		{"threads", transcode.Meta{Mime: ""}, ""}, // unknown plans optimistically
		{"nostr", transcode.Meta{SizeBytes: 50 << 20, Mime: "image/webp"}, ""},
	}
	for _, c := range cases {
		m := manifest(t, c.typ)
		notes := dispatch.PlanMediaFit(m.Name, m.Defaults.Media, []transcode.Meta{c.m})
		switch {
		case c.reason == "" && len(notes) != 0:
			t.Errorf("%s %+v: unexpected fit %v", c.typ, c.m, notes)
		case c.reason != "" && (len(notes) != 1 || !strings.Contains(notes[0].Note, c.reason)):
			t.Errorf("%s %+v: notes %v, want one containing %q", c.typ, c.m, notes, c.reason)
		}
	}
}

// Video limits fail and warn as the old per-platform gate did.
func TestVideoLimits(t *testing.T) {
	cases := []struct {
		typ       string
		size, dur int64
		failPart  string
		warnPart  string
	}{
		{"bluesky", 100_000_000, 60, "", ""},
		{"bluesky", 100_000_001, 60, "over 100 MB", ""},
		{"bluesky", 5_000_000, 181, "", "3 min"},
		{"mastodon", 103_809_025, 60, "over 99 MB", ""},
		{"mastodon", 50_000_000, 3600, "", ""},
		{"threads", 1 << 30, 299, "", ""},
		{"threads", 1<<30 + 1, 60, "over 1 GB", ""},
		{"threads", 1000, 301, "over 5 min", ""},
		{"nostr", 5 << 30, 9999, "", ""},
	}
	for i, c := range cases {
		m := manifest(t, c.typ)
		fail, warns := dispatch.VideoCheck(m.Name, m.Defaults.Media, c.size, c.dur)
		if (c.failPart == "") != (fail == "") || !strings.Contains(fail, c.failPart) {
			t.Errorf("case %d %s: fail %q, want ~%q", i, c.typ, fail, c.failPart)
		}
		joined := strings.Join(warns, ";")
		if (c.warnPart == "") != (len(warns) == 0) || !strings.Contains(joined, c.warnPart) {
			t.Errorf("case %d %s: warns %q, want ~%q", i, c.typ, joined, c.warnPart)
		}
	}
}
