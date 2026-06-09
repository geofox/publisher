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
		{"negative images treated as zero", -1, 2, 4, []int{0, 0}},
	}
	for _, c := range cases {
		got := PlanMedia(c.nImages, c.nSegments, c.cap)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: PlanMedia(%d,%d,%d)=%v want %v", c.name, c.nImages, c.nSegments, c.cap, got, c.want)
		}
	}
}

func TestSplitWithMediaNoOverflowMatchesSplit(t *testing.T) {
	segs, plan, _ := SplitWithMedia("short note", 300, 3, 10, Opts{})
	if len(segs) != 1 || segs[0] != "short note" {
		t.Fatalf("segs=%v", segs)
	}
	if !reflect.DeepEqual(plan, []int{3}) {
		t.Fatalf("plan=%v", plan)
	}
}

func TestSplitWithMediaUncappedAllOnHead(t *testing.T) {
	segs, plan, _ := SplitWithMedia("a\n---\nb", 0, 5, 0, Opts{})
	if len(segs) != 2 {
		t.Fatalf("segs=%v", segs)
	}
	if !reflect.DeepEqual(plan, []int{5, 0}) {
		t.Fatalf("plan=%v", plan)
	}
}

func TestSplitWithMediaOverflowAppendsEmptySegments(t *testing.T) {
	segs, plan, _ := SplitWithMedia("hello", 500, 10, 4, Opts{})
	if !reflect.DeepEqual(plan, []int{4, 4, 2}) {
		t.Fatalf("plan=%v", plan)
	}
	if len(segs) != 3 || segs[0] != "hello" || segs[1] != "" || segs[2] != "" {
		t.Fatalf("segs=%v", segs)
	}
}

func TestSplitWithMediaOverflowNumbered(t *testing.T) {
	segs, plan, _ := SplitWithMedia("hello", 500, 10, 4, Opts{Number: true})
	if !reflect.DeepEqual(plan, []int{4, 4, 2}) {
		t.Fatalf("plan=%v", plan)
	}
	if len(segs) != 3 || segs[0] != "hello 1/3" || segs[1] != "2/3" || segs[2] != "3/3" {
		t.Fatalf("segs=%v", segs)
	}
}

func TestSplitWithMediaNumberedTextThreadCountsExtras(t *testing.T) {
	// Two marker segments + 10 images at cap 4 → one appended post, total 3.
	segs, plan, _ := SplitWithMedia("one\n---\ntwo", 500, 10, 4, Opts{Number: true})
	if !reflect.DeepEqual(plan, []int{4, 4, 2}) {
		t.Fatalf("plan=%v", plan)
	}
	if len(segs) != 3 || segs[0] != "one 1/3" || segs[1] != "two 2/3" || segs[2] != "3/3" {
		t.Fatalf("segs=%v", segs)
	}
}

func TestSplitWithMediaNoLimitCappedNumbered(t *testing.T) {
	// limit<=0 with a cap is unreachable from current platforms (nostr is
	// uncapped) but the exported API must keep counters consistent: marker
	// segments are fixed, so totals just re-stamp with the full chain length.
	segs, plan, _ := SplitWithMedia("a\n---\nb", 0, 10, 4, Opts{Number: true})
	if !reflect.DeepEqual(plan, []int{4, 4, 2}) {
		t.Fatalf("plan=%v", plan)
	}
	if len(segs) != 3 || segs[0] != "a 1/3" || segs[1] != "b 2/3" || segs[2] != "3/3" {
		t.Fatalf("segs=%v", segs)
	}
}
