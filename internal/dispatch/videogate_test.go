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
		{"bluesky", Img{Mime: "video/mp4", Bytes: nil, BlossomURL: "u", DurationSecs: 60}, "size cap"},
		{"mastodon", Img{Mime: "video/mp4", Bytes: nil, BlossomURL: "u"}, "size cap"},
		{"threads", Img{Mime: "video/mp4", Bytes: nil, BlossomURL: "u", DurationSecs: 301}, "over 5 min"},
		{"threads", Img{Mime: "video/mp4", Bytes: nil, BlossomURL: "u", DurationSecs: 299}, ""},
		{"nostr", Img{Mime: "video/mp4", Bytes: nil, BlossomURL: "u", DurationSecs: 9999}, ""},
		{"bluesky", Img{Mime: "image/jpeg", Bytes: held}, ""}, // images untouched
	}
	for i, c := range cases {
		got := gateVideo(c.plat, []Img{c.img})
		if (c.want == "") != (got == "") || (c.want != "" && !strings.Contains(got, c.want)) {
			t.Fatalf("case %d (%s): got %q want ~%q", i, c.plat, got, c.want)
		}
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
