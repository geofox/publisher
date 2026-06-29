package thread

import (
	"reflect"
	"strings"
	"testing"
)

func TestPlaceMedia(t *testing.T) {
	cases := []struct {
		name     string
		imgParts []int
		postPart []int
		cap      int
		want     [][]int
	}{
		// No anchors (all part 0) flattens to today's head-first fill.
		{"no anchors head-first", []int{0, 0, 0, 0, 0}, []int{0, 1}, 4, [][]int{{0, 1, 2, 3}, {4}}},
		{"no anchors single post", []int{0, 0, 0}, []int{0}, 4, [][]int{{0, 1, 2}}},
		// Anchored image diverted to its part.
		{"anchor img2 to part1", []int{0, 0, 1}, []int{0, 1}, 4, [][]int{{0, 1}, {2}}},
		{"split across parts", []int{0, 1, 1}, []int{0, 1}, 4, [][]int{{0}, {1, 2}}},
		// Anchored first, then unanchored fill remaining head-first.
		{"anchored claims slot then unanchored", []int{1, 0, 0}, []int{0, 1}, 4, [][]int{{1, 2}, {0}}},
		// Unanchored over-cap head-fills across posts (no anchors here).
		{"unanchored head-fills past cap", []int{0, 0, 0, 0, 0}, []int{0, 0, 1}, 2, [][]int{{0, 1}, {2, 3}, {4}}},
		// GENUINE anchored spill-forward: two images anchored to part 1 (post idx 1,
		// cap 1) — the second can't fit its part, spills forward to post 2; the
		// unanchored image then head-fills post 0.
		{"anchored spills forward to later post", []int{1, 1, 0}, []int{0, 1, 2}, 1, [][]int{{2}, {0}, {1}}},
		// Anchored over-cap with NO forward room: 5 images all pinned to part 1
		// (post idx 1, cap 4). Post 1 holds 4; the 5th spills BACKWARD to post 0
		// ("any post with room" pass) — never over-cap. (Caught the over-cap bug.)
		{"anchored over-cap spills backward, cap held", []int{1, 1, 1, 1, 1}, []int{0, 1}, 4, [][]int{{4}, {0, 1, 2, 3}}},
		// Uncapped (nostr): everything assigned lands on its part's post.
		{"uncapped per part", []int{0, 1, 1, 0}, []int{0, 1}, 0, [][]int{{0, 3}, {1, 2}}},
		// Clamp/orphan is handled in SplitPlace (Task 2); PlaceMedia assumes
		// pre-clamped imgParts, so no out-of-range case here.
		{"no images", []int{}, []int{0, 1}, 4, [][]int{{}, {}}},
	}
	for _, c := range cases {
		got := PlaceMedia(c.imgParts, c.postPart, c.cap)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: PlaceMedia(%v,%v,%d)=%v want %v", c.name, c.imgParts, c.postPart, c.cap, got, c.want)
		}
	}
}

func TestSplitPlaceNoAnchorsByteIdentical(t *testing.T) {
	// Zero-regression: all-zero imgParts ⇒ identical segs AND identical per-post
	// INDICES (not just counts — correction: a scrambled-index impl must fail here).
	text := "one\n---\ntwo\n---\nthree"
	segs, plan, _ := SplitPlace(text, 500, []int{0, 0, 0}, 4, Opts{Number: true})
	wantSegs, _, _ := SplitWithMedia(text, 500, 3, 4, Opts{Number: true})
	if !reflect.DeepEqual(segs, wantSegs) {
		t.Fatalf("segs=%v want %v", segs, wantSegs)
	}
	// 3 parts, 3 images, cap 4 ⇒ head-first fill puts all 3 on post 0 by index.
	if !reflect.DeepEqual(plan, [][]int{{0, 1, 2}, {}, {}}) {
		t.Fatalf("plan=%v want [[0 1 2] [] []]", plan)
	}
}

func TestSplitPlaceAnchorOverflowNumbered(t *testing.T) {
	// The case derivePostPart got WRONG: numbered, two ~4-word parts at a tiny
	// limit with heavy media overflow, image anchored to part 1. partOf must come
	// from the real numbered split, so the anchored image lands on part 1's FIRST
	// post, not one post late. imgParts length = nImages; only index 5 anchored.
	imgParts := make([]int, 6)
	imgParts[5] = 1 // image 5 → part 1
	segs, plan, _ := SplitPlace(
		"alpha bravo charlie delta\n---\necho foxtrot golf hotel",
		15, imgParts, 3, Opts{Number: true})
	// Find the first post whose part is 1 by re-deriving partOf the authoritative
	// way and asserting image 5 is on the first post of part 1. Simplest assertion:
	// image 5 must share a post with the text segment that begins part 1 ("echo …").
	firstPart1 := -1
	for i, s := range segs {
		if strings.Contains(s, "echo") {
			firstPart1 = i
			break
		}
	}
	if firstPart1 < 0 {
		t.Fatalf("could not locate part-1 head in segs=%v", segs)
	}
	found := false
	for _, ix := range plan[firstPart1] {
		if ix == 5 {
			found = true
		}
	}
	if !found {
		t.Fatalf("image 5 not on part-1 head post %d; plan=%v segs=%v", firstPart1, plan, segs)
	}
}

func TestSplitPlaceAnchorsToPart(t *testing.T) {
	// 3 parts, image 0 unanchored, image 1 pinned to part 2 (post index 2).
	text := "a\n---\nb\n---\nc"
	segs, plan, _ := SplitPlace(text, 500, []int{0, 2}, 10, Opts{})
	if len(segs) != 3 {
		t.Fatalf("segs=%v", segs)
	}
	want := [][]int{{0}, {}, {1}}
	if !reflect.DeepEqual(plan, want) {
		t.Fatalf("plan=%v want %v", plan, want)
	}
}

func TestSplitPlaceOverflowPostsBelongToLastPart(t *testing.T) {
	// 1 part is out of scope for placement, but SplitPlace must still handle the
	// overflow skeleton: "x" + 10 images cap 4 ⇒ posts [4,4,2]; all unanchored.
	segs, plan, _ := SplitPlace("x", 500, make([]int, 10), 4, Opts{})
	if len(segs) != 3 || len(plan) != 3 {
		t.Fatalf("segs=%v plan=%v", segs, plan)
	}
	gotCounts := []int{len(plan[0]), len(plan[1]), len(plan[2])}
	if !reflect.DeepEqual(gotCounts, []int{4, 4, 2}) {
		t.Fatalf("counts=%v want [4 4 2]", gotCounts)
	}
}
