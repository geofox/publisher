package thread

import (
	"reflect"
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
