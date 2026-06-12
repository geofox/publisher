package dispatch

import (
	"testing"
)

func TestSplitVideoNilInput(t *testing.T) {
	v, imgs := splitVideo(nil)
	if v != nil || imgs != nil {
		t.Errorf("nil input: v=%v, imgs=%v", v, imgs)
	}
}

func TestSplitVideoOneImage(t *testing.T) {
	src := []Img{{Mime: "image/jpeg", Alt: "img"}}
	v, imgs := splitVideo(src)
	if v != nil {
		t.Errorf("expected nil video, got %+v", v)
	}
	if len(imgs) != 1 || imgs[0].Alt != "img" {
		t.Errorf("images wrong: %+v", imgs)
	}
}

func TestSplitVideoOneVideo(t *testing.T) {
	src := []Img{{Mime: "video/mp4", Alt: "clip"}}
	v, imgs := splitVideo(src)
	if v == nil {
		t.Fatal("expected video, got nil")
	}
	if v.Alt != "clip" {
		t.Errorf("video alt = %q, want clip", v.Alt)
	}
	if len(imgs) != 0 {
		t.Errorf("images must be empty, got %+v", imgs)
	}
}

func TestSplitVideoVideoAndImages(t *testing.T) {
	src := []Img{
		{Mime: "video/mp4", Alt: "clip"},
		{Mime: "image/jpeg", Alt: "img1"},
		{Mime: "image/png", Alt: "img2"},
	}
	v, imgs := splitVideo(src)
	if v == nil || v.Alt != "clip" {
		t.Fatalf("video wrong: %+v", v)
	}
	if len(imgs) != 2 {
		t.Fatalf("expected 2 images, got %d: %+v", len(imgs), imgs)
	}
	if imgs[0].Alt != "img1" || imgs[1].Alt != "img2" {
		t.Errorf("images order wrong: %+v", imgs)
	}
}

func TestSplitVideoTwoVideosDropsSecond(t *testing.T) {
	src := []Img{
		{Mime: "video/mp4", Alt: "first"},
		{Mime: "video/mp4", Alt: "second"},
	}
	v, imgs := splitVideo(src)
	if v == nil || v.Alt != "first" {
		t.Fatalf("first video must be kept: %+v", v)
	}
	if len(imgs) != 0 {
		t.Errorf("second video must be dropped (not in images): %+v", imgs)
	}
}
