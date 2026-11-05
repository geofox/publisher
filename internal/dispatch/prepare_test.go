package dispatch

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"strings"
	"testing"

	"github.com/geofox/publisher/internal/bluesky"
	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/mastodon"
	"github.com/geofox/publisher/internal/nostr"
	"github.com/geofox/publisher/internal/threads"
)

func capsOf(d destination.Driver) (string, destination.MediaCaps) {
	m := d.Manifest()
	return m.Name, m.Defaults.Media
}

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func readHEICFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../transcode/testdata/sample.heic")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// flatJPEG returns an over-16MP JPEG that violates the Mastodon pixel cap
// while staying tiny in bytes (uniform color compresses to almost nothing).
func flatJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = 30, 90, 200, 255
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestGateVideo(t *testing.T) {
	held := make([]byte, 1024)
	cases := []struct {
		drv   destination.Driver
		img   Img
		want  string // substring of the failure, "" = pass
		class destination.ErrorClass
	}{
		{bluesky.Driver{}, Img{Mime: "video/mp4", Bytes: held, DurationSecs: 60}, "", ""},
		{bluesky.Driver{}, Img{Mime: "video/mp4", BlossomURL: "u", DurationSecs: 60}, "bytes unavailable", destination.ClassTransient},
		{mastodon.Driver{}, Img{Mime: "video/mp4", BlossomURL: "u"}, "bytes unavailable", destination.ClassTransient},
		{threads.Driver{}, Img{Mime: "video/mp4", BlossomURL: "u", DurationSecs: 301}, "over 5 min", destination.ClassInvalid},
		{threads.Driver{}, Img{Mime: "video/mp4", BlossomURL: "u", DurationSecs: 299}, "", ""},
		{threads.Driver{}, Img{Mime: "video/mp4", Bytes: []byte("v"), DurationSecs: 60}, "no canonical URL", destination.ClassInvalid},
		{nostr.Driver{}, Img{Mime: "video/mp4", BlossomURL: "u", DurationSecs: 9999}, "", ""},
		{bluesky.Driver{}, Img{Mime: "image/jpeg", Bytes: held}, "", ""}, // images untouched
		// The canonical SizeBytes drives the gate even without bytes (retry path).
		{bluesky.Driver{}, Img{Mime: "video/mp4", SizeBytes: 101_000_000, BlossomURL: "u"}, "100 MB", destination.ClassInvalid},
		{mastodon.Driver{}, Img{Mime: "video/mp4", SizeBytes: 104_000_000, BlossomURL: "u"}, "99 MB", destination.ClassInvalid},
		{threads.Driver{}, Img{Mime: "video/mp4", BlossomURL: "u", SizeBytes: 1<<30 + 1, DurationSecs: 60}, "over 1 GB", destination.ClassInvalid},
	}
	for i, c := range cases {
		name, mc := capsOf(c.drv)
		err := gateVideo(name, mc, []Img{c.img})
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Fatalf("case %d (%s): got %v want ~%q", i, name, err, c.want)
		}
		if err != nil && destination.ClassOf(err) != c.class {
			t.Fatalf("case %d (%s): class %s, want %s", i, name, destination.ClassOf(err), c.class)
		}
	}
}

func TestPrepareMediaPassthrough(t *testing.T) {
	png := tinyPNG(t)
	imgs := []Img{{Bytes: png, Mime: "image/png", Alt: "a", BlossomURL: "https://b/x", SHA256: "abc", Dim: "8x8"}}
	for _, d := range []destination.Driver{mastodon.Driver{}, nostr.Driver{}, bluesky.Driver{}} {
		name, mc := capsOf(d)
		out, err := prepareMedia(t.Context(), name, mc, imgs, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		m := out[0]
		if !bytes.Equal(m.Bytes, png) || m.Alt != "a" || m.URL != "https://b/x" || m.SHA256 != "abc" || m.Width != 8 || m.Height != 8 {
			t.Fatalf("%s: a fitting image must pass through intact: %+v", name, m)
		}
	}
}

func TestPrepareMediaReencodesViolations(t *testing.T) {
	src := flatJPEG(t, 5000, 4000) // 20 MP > Mastodon's 16 MP cap
	imgs := []Img{{Bytes: src, Mime: "image/jpeg", Alt: "big", BlossomURL: "https://b/big", SHA256: "canon"}}
	name, mc := capsOf(mastodon.Driver{})
	out, err := prepareMedia(t.Context(), name, mc, imgs, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := out[0]
	if bytes.Equal(m.Bytes, src) || m.Mime != "image/jpeg" || m.Alt != "big" {
		t.Fatalf("over-pixel-cap image must be re-encoded with its alt kept: %+v", m)
	}
	if m.SHA256 == "canon" || int64(m.Width)*int64(m.Height) > mc.Images.MaxPixels {
		t.Fatalf("variant must describe itself (hash, dimensions): %+v", m)
	}
	if !bytes.Equal(imgs[0].Bytes, src) {
		t.Fatal("input must not be mutated")
	}
}

// Instance limits are applied: a smaller announced pixel cap fits harder.
func TestPrepareMediaUsesAccountLimits(t *testing.T) {
	name, mc := capsOf(mastodon.Driver{})
	mc.Images.MaxPixels = 1_000_000
	src := flatJPEG(t, 2000, 1000)
	out, err := prepareMedia(t.Context(), name, mc, []Img{{Bytes: src, Mime: "image/jpeg"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if px := out[0].Width * out[0].Height; px > 1_000_000 || px == 0 {
		t.Fatalf("variant is %dx%d, want ≤ 1 MP", out[0].Width, out[0].Height)
	}
}

func TestPrepareMediaUnfittableIsInvalid(t *testing.T) {
	name, mc := capsOf(threads.Driver{})
	host := func(context.Context, []byte, string) (string, error) { return "https://b/v", nil }
	_, err := prepareMedia(t.Context(), name, mc, []Img{{Bytes: []byte("not an image"), Mime: "image/gif", BlossomURL: "u"}}, host)
	if destination.ClassOf(err) != destination.ClassInvalid {
		t.Fatalf("an unconvertible image must fail as invalid, got %v", err)
	}
}

func TestPrepareMediaMissingBytes(t *testing.T) {
	img := []Img{{Mime: "image/png", BlossomURL: "https://b/p"}}
	name, mc := capsOf(bluesky.Driver{})
	if _, err := prepareMedia(t.Context(), name, mc, img, nil); destination.ClassOf(err) != destination.ClassTransient {
		t.Fatalf("byte upload without bytes must be a transient failure, got %v", err)
	}
	name, mc = capsOf(threads.Driver{})
	out, err := prepareMedia(t.Context(), name, mc, img, nil)
	if err != nil || out[0].URL != "https://b/p" {
		t.Fatalf("URL ingestion without bytes must pass the canonical URL: %+v %v", out, err)
	}
}

func TestPrepareMediaHostsVariant(t *testing.T) {
	// The HEIC fixture violates the Threads profile (format) even when tiny.
	imgs := []Img{
		{Bytes: readHEICFixture(t), Mime: "image/heic", Alt: "v", BlossomURL: "https://b/heic"},
		{Bytes: tinyPNG(t), Mime: "image/png", Alt: "ok", BlossomURL: "https://b/png"},
	}
	hosted := 0
	host := func(_ context.Context, _ []byte, mime string) (string, error) {
		hosted++
		if mime != "image/jpeg" {
			t.Fatalf("hosted variant mime = %s, want image/jpeg", mime)
		}
		return "https://b/variant", nil
	}
	name, mc := capsOf(threads.Driver{})
	out, err := prepareMedia(t.Context(), name, mc, imgs, host)
	if err != nil {
		t.Fatal(err)
	}
	if hosted != 1 || out[0].URL != "https://b/variant" || out[0].Alt != "v" || out[1].URL != "https://b/png" {
		t.Fatalf("hosted=%d out=%+v", hosted, out)
	}
}

func TestPrepareMediaHostErrorIsTransient(t *testing.T) {
	imgs := []Img{{Bytes: readHEICFixture(t), Mime: "image/heic", BlossomURL: "https://b/heic"}}
	host := func(context.Context, []byte, string) (string, error) { return "", errors.New("blossom down") }
	name, mc := capsOf(threads.Driver{})
	if _, err := prepareMedia(t.Context(), name, mc, imgs, host); destination.ClassOf(err) != destination.ClassTransient {
		t.Fatalf("a host failure must be transient (the retrier redrives), got %v", err)
	}
}

func TestPrepareMediaNilHostKeepsCanonical(t *testing.T) {
	heic := readHEICFixture(t)
	imgs := []Img{{Bytes: heic, Mime: "image/heic", Alt: "v", BlossomURL: "https://b/heic"}}
	name, mc := capsOf(threads.Driver{})
	out, err := prepareMedia(t.Context(), name, mc, imgs, nil)
	if err != nil || out[0].URL != "https://b/heic" || !bytes.Equal(out[0].Bytes, heic) {
		t.Fatalf("without a host the canonical object is sent unchanged: %+v err=%v", out, err)
	}
}

func TestPrepareMediaVideoFirstAndUntouched(t *testing.T) {
	imgs := []Img{
		{Mime: "image/png", Bytes: tinyPNG(t), BlossomURL: "https://b/p"},
		{Mime: "video/mp4", Bytes: []byte("vid"), BlossomURL: "https://b/v", Dim: "1080x1920", PosterURL: "https://b/poster"},
	}
	name, mc := capsOf(bluesky.Driver{})
	out, err := prepareMedia(t.Context(), name, mc, imgs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !out[0].IsVideo() || string(out[0].Bytes) != "vid" || out[0].Width != 1080 || out[0].Height != 1920 || out[0].PosterURL != "https://b/poster" {
		t.Fatalf("video must lead, untouched, with its dimensions: %+v", out[0])
	}
}
