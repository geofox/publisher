package dispatch

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"os"
	"testing"
)

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

func TestFitImgsPassthrough(t *testing.T) {
	imgs := []Img{{Bytes: tinyPNG(t), Mime: "image/png", Alt: "a", BlossomURL: "https://b/x"}}
	out, err := fitImgs("mastodon", imgs)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out[0].Bytes, imgs[0].Bytes) || out[0].Alt != "a" {
		t.Fatal("fitting mastodon must pass small PNG through with fields intact")
	}
	// nostr: no profile → identity.
	out, err = fitImgs("nostr", imgs)
	if err != nil || !bytes.Equal(out[0].Bytes, imgs[0].Bytes) {
		t.Fatal("platform without profile must be identity")
	}
}

func TestFitImgsErrorFailsLoudly(t *testing.T) {
	// Undecodable bytes whose size violates no mastodon cap pass through; to
	// force the loud-failure path use bytes that claim a disallowed THREADS
	// format — but fitImgs is platform-generic, so instead use a profile
	// violation that cannot be fixed: undecodable bytes over a byte cap is
	// awkward to build small. Simplest honest probe: threads + undecodable
	// disallowed-format bytes.
	if _, err := fitImgs("threads", []Img{{Bytes: []byte("not an image"), Mime: "image/gif"}}); err == nil {
		t.Fatal("unconvertible profile violation must fail the target loudly")
	}
}

func TestPrepThreadsImgsHostsVariant(t *testing.T) {
	// The HEIC fixture genuinely violates the Threads profile (format), even
	// when tiny — forces the variant path without a multi-MB fixture.
	heicBytes := readHEICFixture(t)
	imgs := []Img{
		{Bytes: heicBytes, Mime: "image/heic", Alt: "v", BlossomURL: "https://b/heic"},
		{Bytes: tinyPNG(t), Mime: "image/png", Alt: "ok", BlossomURL: "https://b/png"},
	}
	hosted := 0
	host := func(ctx context.Context, body []byte, mime string) (string, error) {
		hosted++
		if mime != "image/jpeg" {
			t.Fatalf("hosted variant mime = %s, want image/jpeg", mime)
		}
		return "https://b/variant", nil
	}
	ti, err := prepThreadsImgs(context.Background(), imgs, host)
	if err != nil {
		t.Fatal(err)
	}
	if hosted != 1 {
		t.Fatalf("hosted %d variants, want exactly 1 (PNG passes through)", hosted)
	}
	if ti[0].URL != "https://b/variant" || ti[0].Alt != "v" {
		t.Fatalf("violating image must point at the hosted variant: %+v", ti[0])
	}
	if ti[1].URL != "https://b/png" {
		t.Fatalf("fitting image must keep its canonical URL: %+v", ti[1])
	}
}

func TestPrepThreadsImgsHostErrorFailsTarget(t *testing.T) {
	imgs := []Img{{Bytes: readHEICFixture(t), Mime: "image/heic", BlossomURL: "https://b/heic"}}
	host := func(context.Context, []byte, string) (string, error) { return "", errors.New("blossom down") }
	if _, err := prepThreadsImgs(context.Background(), imgs, host); err == nil {
		t.Fatal("host failure must fail the threads target (retrier redrives)")
	}
}

func TestPrepThreadsImgsNilHostIsIdentity(t *testing.T) {
	imgs := []Img{{Bytes: readHEICFixture(t), Mime: "image/heic", Alt: "v", BlossomURL: "https://b/heic"}}
	ti, err := prepThreadsImgs(context.Background(), imgs, nil)
	if err != nil || ti[0].URL != "https://b/heic" {
		t.Fatalf("nil host must keep canonical URLs (old behavior): %+v err=%v", ti, err)
	}
}
