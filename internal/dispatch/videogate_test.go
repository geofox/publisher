package dispatch

import (
	"strings"
	"testing"
)

func TestGateVideo(t *testing.T) {
	held := make([]byte, 1024)
	cases := []struct {
		plat string
		img  Img
		want string // substring of the failure, "" = pass
	}{
		{"bluesky", Img{Mime: "video/mp4", Bytes: held, DurationSecs: 60}, ""},
		{"bluesky", Img{Mime: "video/mp4", Bytes: nil, BlossomURL: "u", DurationSecs: 60}, "bytes unavailable"},
		{"mastodon", Img{Mime: "video/mp4", Bytes: nil, BlossomURL: "u"}, "bytes unavailable"},
		{"threads", Img{Mime: "video/mp4", Bytes: nil, BlossomURL: "u", DurationSecs: 301}, "over 5 min"},
		{"threads", Img{Mime: "video/mp4", Bytes: nil, BlossomURL: "u", DurationSecs: 299}, ""},
		{"threads", Img{Mime: "video/mp4", Bytes: []byte("v"), BlossomURL: "", DurationSecs: 60}, "no canonical URL"},
		{"nostr", Img{Mime: "video/mp4", Bytes: nil, BlossomURL: "u", DurationSecs: 9999}, ""},
		{"bluesky", Img{Mime: "image/jpeg", Bytes: held}, ""}, // images untouched
		// Canonical SizeBytes drives gate even without bytes (retry path).
		{"bluesky", Img{Mime: "video/mp4", SizeBytes: 101_000_000, BlossomURL: "u"}, "100 MB"},
		{"mastodon", Img{Mime: "video/mp4", SizeBytes: 104_000_000, BlossomURL: "u"}, "99 MB"},
	}
	for i, c := range cases {
		got := gateVideo(c.plat, []Img{c.img})
		if (c.want == "") != (got == "") || (c.want != "" && !strings.Contains(got, c.want)) {
			t.Fatalf("case %d (%s): got %q want ~%q", i, c.plat, got, c.want)
		}
	}
}

func TestGateVideoUsesCanonicalSize(t *testing.T) {
	over := Img{Mime: "video/mp4", BlossomURL: "u", SizeBytes: 1<<30 + 1, DurationSecs: 60}
	if got := gateVideo("threads", []Img{over}); !strings.Contains(got, "over 1 GB") {
		t.Fatalf("threads must gate on canonical size without bytes: %q", got)
	}
}

func TestFitImgsSkipsVideo(t *testing.T) {
	v := Img{Mime: "video/mp4", Bytes: []byte("vid"), BlossomURL: "u"}
	out, err := fitImgs("mastodon", []Img{v})
	if err != nil {
		t.Fatal(err)
	}
	if string(out[0].Bytes) != "vid" {
		t.Fatal("video must pass through image fitting untouched")
	}
}

func TestPrepThreadsImgsSkipsVideo(t *testing.T) {
	imgs := []Img{
		{Mime: "video/mp4", BlossomURL: "https://b/v"},
		{Mime: "image/png", Bytes: tinyPNG(t), BlossomURL: "https://b/p", Alt: "pic"},
	}
	ti, err := prepThreadsImgs(t.Context(), imgs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ti) != 1 || ti[0].URL != "https://b/p" {
		t.Fatalf("video must not enter the threads image list: %+v", ti)
	}
}
