package store

import (
	"reflect"
	"testing"
)

func TestSegmentImagesRoundTrip(t *testing.T) {
	s := openTestStore(t) // real helper (store/drafts_crud_test.go:9)
	p := &Post{
		MasterText: "a\n---\nb", Platforms: []string{"bluesky"}, Status: "partial",
		Targets: []Target{{
			Platform: "bluesky", Status: "partial", FinalText: "a\n---\nb",
			Segments: []Segment{
				{Ordinal: 0, Text: "a", Status: "success", Images: []int{0, 2}},
				{Ordinal: 1, Text: "b", Status: "pending", Images: []int{1}},
			},
		}},
	}
	if err := s.SavePost(p); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetPost(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Targets[0].Segments[0].Images, []int{0, 2}) {
		t.Fatalf("seg0 images=%v", got.Targets[0].Segments[0].Images)
	}
	if !reflect.DeepEqual(got.Targets[0].Segments[1].Images, []int{1}) {
		t.Fatalf("seg1 images=%v", got.Targets[0].Segments[1].Images)
	}
}

func TestDraftMediaClientIDRoundTrip(t *testing.T) {
	s := openTestStore(t)
	d := &Draft{
		ID: "d1", Title: "t", MasterText: "x", Spec: `{"master_text":"x"}`,
		Media: []Media{{Ordinal: 0, BlossomURL: "https://b/1", ClientID: "img-abc"}},
	}
	if err := s.CreateDraft(d); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetDraft("d1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Media[0].ClientID != "img-abc" {
		t.Fatalf("client_id=%q", got.Media[0].ClientID)
	}
}
