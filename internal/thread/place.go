// Package thread placement: assign images to a fixed post skeleton.
package thread

import (
	"sort"
	"strconv"
	"strings"
)

// PlaceMedia distributes images across a fixed chain of posts, honoring per-image
// part anchors. postPart[j] is the --- part post j belongs to; imgParts[i] is the
// part image i is pinned to (0 = head/default, caller pre-clamps to a valid part).
// Two passes: anchored images claim a post in their part (spilling forward if the
// part is full), then unanchored images fill the first post head-first. cap<=0
// means unbounded (nostr). Within a post, indices are sorted ascending. The plan
// has exactly len(postPart) entries; with all imgParts==0 it reproduces today's
// head-first contiguous fill.
func PlaceMedia(imgParts []int, postPart []int, cap int) [][]int {
	plan := make([][]int, len(postPart))
	for j := range plan {
		plan[j] = []int{}
	}
	if len(postPart) == 0 {
		return plan
	}
	room := func(j int) bool { return cap <= 0 || len(plan[j]) < cap }

	// firstInPart returns the first post of part p with room, else the first post
	// at-or-after that part with room (spill forward), else the last post.
	place := func(img, part int) {
		// Prefer a post in the image's part.
		for j := 0; j < len(postPart); j++ {
			if postPart[j] == part && room(j) {
				plan[j] = append(plan[j], img)
				return
			}
		}
		// Part full: spill forward from the part's first post.
		start := 0
		for j := 0; j < len(postPart); j++ {
			if postPart[j] == part {
				start = j
				break
			}
		}
		for j := start; j < len(postPart); j++ {
			if room(j) {
				plan[j] = append(plan[j], img)
				return
			}
		}
		// Anchored part and everything after it is full: spill to ANY post with
		// room (backward). The skeleton is sized by PlanMedia(nImages,…) so total
		// room >= nImages — this pass always succeeds, holding the per-post cap.
		for j := 0; j < len(postPart); j++ {
			if room(j) {
				plan[j] = append(plan[j], img)
				return
			}
		}
		plan[len(plan)-1] = append(plan[len(plan)-1], img) // unreachable when total room >= nImages
	}

	// Pass 1: anchored (non-zero part) images, in attach order.
	for i, part := range imgParts {
		if part != 0 {
			place(i, part)
		}
	}
	// Pass 2: unanchored (part 0) images fill head-first globally.
	for i, part := range imgParts {
		if part == 0 {
			for j := 0; j < len(postPart); j++ {
				if room(j) {
					plan[j] = append(plan[j], i)
					break
				}
			}
		}
	}
	for j := range plan {
		sort.Ints(plan[j])
	}
	return plan
}

// splitWithMediaPlan is SplitWithMedia plus partOf (the --- part index per post,
// aligned 1:1 with segs). Overflow image-only posts ride the last part. This is
// the single implementation; SplitWithMedia and SplitPlace both wrap it, so the
// part labels can never diverge from the real numbered skeleton.
func splitWithMediaPlan(text string, limit, nImages, cap int, opts Opts) (segs []string, partOf []int, plan []int, warnings []string) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	// text segs + partOf, matching Split()'s output exactly:
	segs, partOf, warnings = splitAtParts(text, limit)
	if opts.Number && len(segs) >= 2 {
		if limit <= 0 {
			segs = appendCounters(segs, len(segs))
		} else {
			segs, partOf, warnings = number(text, limit, 0)
		}
	}
	plan = PlanMedia(nImages, len(segs), cap)
	if extra := len(plan) - len(segs); extra > 0 && opts.Number {
		if limit <= 0 {
			segs, partOf, warnings = splitAtParts(text, limit)
			segs = appendCounters(segs, len(plan))
		} else {
			for i := 0; i < 4; i++ {
				segs, partOf, warnings = number(text, limit, extra)
				plan = PlanMedia(nImages, len(segs), cap)
				if len(plan)-len(segs) == extra {
					break
				}
				extra = len(plan) - len(segs)
			}
		}
	}
	last := 0
	if len(partOf) > 0 {
		last = partOf[len(partOf)-1]
	}
	for i := len(segs); i < len(plan); i++ {
		t := ""
		if opts.Number {
			t = strconv.Itoa(i+1) + "/" + strconv.Itoa(len(plan))
		}
		segs = append(segs, t)
		partOf = append(partOf, last)
	}
	return segs, partOf, plan, warnings
}

// SplitPlace places images per the anchor assignment over today's exact
// skeleton. Post count and numbering are independent of imgParts. len(segs)==len(plan).
func SplitPlace(text string, limit int, imgParts []int, cap int, opts Opts) (segs []string, plan [][]int, warnings []string) {
	var partOf []int
	segs, partOf, _, warnings = splitWithMediaPlan(text, limit, len(imgParts), cap, opts)
	maxPart := 0
	for _, pp := range partOf {
		if pp > maxPart {
			maxPart = pp
		}
	}
	clamped := make([]int, len(imgParts))
	for i, p := range imgParts {
		if p < 0 {
			p = 0
		} else if p > maxPart {
			p = maxPart
		}
		clamped[i] = p
	}
	plan = PlaceMedia(clamped, partOf, cap)
	return segs, plan, warnings
}
