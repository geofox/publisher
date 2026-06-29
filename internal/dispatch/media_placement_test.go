package dispatch

import (
	"context"
	"reflect"
	"testing"
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
		Overrides{}, make([]Img, 2), nil, false, []int{0, 2}, nil)

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
