package thread

import (
	"reflect"
	"testing"
)

func TestMaxImagesFor(t *testing.T) {
	cases := map[string]int{"bluesky": 10, "mastodon": 4, "threads": 10, "nostr": 0, "weird": 0}
	for p, want := range cases {
		if got := MaxImagesFor(p); got != want {
			t.Errorf("MaxImagesFor(%q)=%d want %d", p, got, want)
		}
	}
}

func TestPlanMedia(t *testing.T) {
	cases := []struct {
		name                    string
		nImages, nSegments, cap int
		want                    []int
	}{
		{"no images", 0, 3, 4, []int{0, 0, 0}},
		{"fits on head", 3, 1, 4, []int{3}},
		{"uncapped all on head", 10, 3, 0, []int{10, 0, 0}},
		{"spills across text segments", 10, 3, 4, []int{4, 4, 2}},
		{"appends image-only segments", 10, 1, 4, []int{4, 4, 2}},
		{"exact fit no append", 8, 2, 4, []int{4, 4}},
		{"zero segments treated as one", 2, 0, 4, []int{2}},
	}
	for _, c := range cases {
		got := PlanMedia(c.nImages, c.nSegments, c.cap)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: PlanMedia(%d,%d,%d)=%v want %v", c.name, c.nImages, c.nSegments, c.cap, got, c.want)
		}
	}
}
