package dispatch

import (
	"context"
	"reflect"
	"testing"

	"github.com/geofox/publisher/internal/store"
)

// TestRunChainPlacesAnchoredImage drives a single chain through the real
// fakeMastoChain harness and asserts the recorded per-segment placement plan
// (out.Segments[i].Images) matches the anchored imgParts. Two images over a
// 3-post chain (mastodon cap 4): image 1 is pinned to part 2 (3rd post), image
// 0 is unanchored and fills head-first.
func TestRunChainPlacesAnchoredImage(t *testing.T) {
	f := &fakeMastoChain{}
	d := &Dispatcher{Mastodon: f}
	out := d.runChain(context.Background(), "mastodon", "a\n---\nb\n---\nc",
		Overrides{}, make([]Img, 2), nil, false, []int{0, 2}, nil, "t")

	if out.Status != "success" {
		t.Fatalf("status=%s err=%s", out.Status, out.Error)
	}
	if len(out.Segments) != 3 {
		t.Fatalf("want 3 recorded segments, got %d: %+v", len(out.Segments), out.Segments)
	}
	// post 0 carries image 0; post 1 carries none; post 2 carries image 1.
	if got := out.Segments[0].Images; !reflect.DeepEqual(got, []int{0}) {
		t.Errorf("seg0 images=%v want [0]", got)
	}
	if got := out.Segments[1].Images; len(got) != 0 {
		t.Errorf("seg1 images=%v want []", got)
	}
	if got := out.Segments[2].Images; !reflect.DeepEqual(got, []int{1}) {
		t.Errorf("seg2 images=%v want [1]", got)
	}
	// The fake also records how many images each post actually carried.
	for i, want := range []int{1, 0, 1} {
		if f.calls[i].nImgs != want {
			t.Errorf("post%d carried %d images, want %d", i, f.calls[i].nImgs, want)
		}
	}
}

// TestResumeReadsPersistedPlacement asserts that resumeSegments reads per-segment
// Images from the persisted Segment.Images rather than re-deriving a front-fill
// plan. The non-front-loaded case: image 1 is on segment 2, not segment 0.
func TestResumeReadsPersistedPlacement(t *testing.T) {
	f := &fakeMastoChain{}
	d := &Dispatcher{Mastodon: f}
	// Segment 0 already succeeded (RemoteID set) — resume continues from seg 1.
	// The persisted placement: seg0 had image 0, seg1 has no images, seg2 has image 1.
	tg := store.Target{
		Platform: "mastodon", Status: "partial", FinalText: "a\n---\nb\n---\nc",
		Segments: []store.Segment{
			{Ordinal: 0, Text: "a", Status: "success", RemoteID: "m0", Images: []int{0}},
			{Ordinal: 1, Text: "b", Status: "pending", Images: []int{}},
			{Ordinal: 2, Text: "c", Status: "pending", Images: []int{1}},
		},
	}
	imgs := []Img{{BlossomURL: "https://b/0"}, {BlossomURL: "https://b/1"}}
	out := d.resumeSegments(context.Background(), tg, Overrides{}, imgs, nil, "t")
	if out.Status != "success" {
		t.Fatalf("status=%s err=%s", out.Status, out.Error)
	}
	// 2 calls: seg1 and seg2 (seg0 skipped because RemoteID already set).
	if len(f.calls) != 2 {
		t.Fatalf("want 2 re-posts (seg1 and seg2), got %d", len(f.calls))
	}
	// seg1 (f.calls[0]) must carry 0 images (Images: []int{}).
	if f.calls[0].nImgs != 0 {
		t.Errorf("resumed seg1 images=%d want 0", f.calls[0].nImgs)
	}
	// seg2 (f.calls[1]) must carry 1 image (image index 1, from Images: []int{1}).
	// A front-fill would give it image 0 at index 0, NOT the correct image 1.
	if f.calls[1].nImgs != 1 {
		t.Errorf("resumed seg2 images=%d want 1", f.calls[1].nImgs)
	}
	_ = out
}
