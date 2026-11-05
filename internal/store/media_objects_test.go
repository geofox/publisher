package store

import (
	"context"
	"errors"
	"testing"
)

func TestMediaObjects(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	m := &MediaObject{Status: MediaProcessing, Mime: "video/mp4", JobID: "j1", Error: "token=abc"}
	if err := s.CreateMediaObject(ctx, m); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MediaObject(ctx, "own_other", m.ID); !errors.Is(err, ErrNoMedia) {
		t.Fatalf("another owner's media: %v", err)
	}
	m.Status, m.URL, m.Error = MediaReady, "https://b/x", ""
	if ok, err := s.FinishMediaObject(ctx, m); err != nil || !ok {
		t.Fatalf("finish = %v, %v", ok, err)
	}
	m.Status, m.Error = MediaFailed, "late"
	if ok, _ := s.FinishMediaObject(ctx, m); ok {
		t.Fatal("a finished object changed again")
	}
	if err := s.SetMediaAlt(ctx, DefaultOwner, m.ID, "alt"); err != nil {
		t.Fatal(err)
	}
	got, err := s.MediaObject(ctx, DefaultOwner, m.ID)
	if err != nil || got.Status != MediaReady || got.URL != "https://b/x" || got.Alt != "alt" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if err := s.SetMediaAlt(ctx, "own_other", m.ID, "x"); !errors.Is(err, ErrNoMedia) {
		t.Fatalf("alt on another owner's media: %v", err)
	}
}

func TestAdoptMedia(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	m := Media{BlossomURL: "https://b.test/aa.png", SHA256: "aa", Mime: "image/png", Dim: "2x2", SizeBytes: 5, Alt: "first"}
	a, err := s.AdoptMedia(ctx, DefaultOwner, m)
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != MediaReady || a.URL != m.BlossomURL || a.Alt != "first" {
		t.Fatalf("adopted = %+v", a)
	}
	m.Alt = "second"
	b, err := s.AdoptMedia(ctx, DefaultOwner, m)
	if err != nil || b.ID != a.ID {
		t.Fatalf("again = %+v, %v; want %s", b, err, a.ID)
	}
	m.SHA256 = "bb"
	if c, err := s.AdoptMedia(ctx, DefaultOwner, m); err != nil || c.ID == a.ID {
		t.Fatalf("another file = %+v, %v", c, err)
	}
}
