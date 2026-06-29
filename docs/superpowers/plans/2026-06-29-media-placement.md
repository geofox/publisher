# Media Placement Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** In an already-threaded draft (≥2 `---` parts), let the operator pin each attached image to a chosen part of the thread, shared across all platforms.

**Architecture:** Placement is a structured map `anchors: imageID → partIndex`, kept client-side and in the draft (`spec_json` + a `draft_media.client_id` column). At post/preview time the client flattens it against the current image order into a positional `img_parts []int`. A new pure `thread.SplitPlace` computes the post skeleton with **today's unchanged numbering logic** and then *permutes* images across those fixed posts via `thread.PlaceMedia` (anchored→its part, unanchored→head-first; over-cap parts spill forward). The resolved per-post plan is persisted on `Segment.Images` so resume/Fire read it instead of re-deriving.

**Tech Stack:** Go (stdlib + `github.com/rivo/uniseg`), SQLite, vanilla ES modules (no JS test runner — web tasks verified manually in a browser).

**Spec:** `docs/superpowers/specs/2026-06-29-media-placement-design.md`

## Global Constraints

- **v1 scope = manual/threaded drafts only**: placement UI activates only when `splitMarkers(master)` yields ≥2 parts. No auto-split/single-post placement.
- **Shared placement only**: one map maps onto every platform. No per-platform anchor delta.
- **Images only**: the existing "one video OR images per post" attach guard is unchanged.
- **Zero regression**: with no anchors (`img_parts` all 0 or absent), every output is byte-identical to today. This is a hard, tested invariant.
- **Post count & numbering never change from anchoring**: `SplitPlace`'s skeleton/numbering is today's logic untouched; anchoring only permutes images across the fixed posts.
- **No re-derivation of placement**: dispatch records the resolved `[][]int` on `Segment.Images`; resume/Fire read it.
- Go tests run with `go test ./...`. Commit after every green task.

---

## Plan Review Corrections (READ FIRST — verified against the codebase)

Three reviewers checked this plan against the real repo. The **production-code line cites and struct/function names are accurate**; the **test snippets are illustrative pseudocode that must be rewritten against the real test harnesses** below. Key corrections, all confirmed:

**C1 — `derivePostPart` is WRONG; thread `partOf` out of the real split instead (Task 2).** Verified empirically: `SplitWithMedia` numbers *text* posts with one total and *overflow* posts with another (e.g. text `…/8`, overflow `…/10`), so re-splitting at a guessed `limit - counterWidth(len(segs))` budget misaligns the part boundary under numbering+overflow — an image anchored to part 1 lands one post late. Do NOT re-derive; have the split that produces `segs` also emit `partOf`. See the rewritten Task 2.

**C2 — `splitAtParts` must use a CONTIGUOUS index over NON-EMPTY parts** (not the raw `splitMarkers` block index `j`). `splitMarkers` keeps empty blocks; the client's `masterParts()` does `.filter(Boolean)` (contiguous). For them to agree, the server must increment the part index only for non-empty parts. Otherwise placement on drafts with an empty `---` block silently lands on the wrong post.

**C3 — Real test harnesses (the plan's `newFakeDispatcher`/`newTestStore`/`newTestAPI`/`img()`/`dispatchOne`/`postedSegments` are INVENTED).** Use:
- dispatch: construct fakes directly (`f := &fakeMastoChain{}`; `d := &Dispatcher{Mastodon: f}`), call `d.runChain(ctx, "mastodon", text, Overrides{}, make([]Img, n), nil, false, imgParts, nil)` and `d.resumeSegments(ctx, tg, Overrides{}, make([]Img, n), nil)` directly; assert on `f.calls[i].nImgs` (chain_test.go:23/236/262) or `out.Segments[i].Images`. **`fakeMasto` records nothing — use `fakeBsky` or `fakeMastoChain`, which record `nImgs`.** `Img` is a plain struct literal (no `img()` helper).
- store: `openTestStore(t)` (not `newTestStore`); set `Post.ID` explicitly — `SavePost` does not mint one.
- api preview: `postPreview(t, body)` with inline `a := &API{}` + `json.Unmarshal(rec.Body.Bytes(), …)` (thread_preview_test.go:12).
- api drafts: file is **`internal/api/drafts_test.go`** (not `drafts_crud_test.go`, which is in `store/`); use `newDraftAPI(t)` + `postMultipart(t, a, path, spec, files)` + `a.Routes().ServeHTTP` (drafts_test.go:16/29).

**C4 — `PlanBlueskyCard` signature change breaks 9 existing call sites** in `card_test.go` (lines 15,22,32,42,53,68,75,84,94) that pass an `int` arg 3. Change them to `[]int` (e.g. `2`→`[]int{0,0}`, `0`→`nil`). These are call-arg fixes, not assertion fixes.

**C5 — Task 5 silently breaks two existing resume tests** that must be updated in the same task: `TestResumeRepostsSegmentImageSlices` (chain_test.go:344) and `TestResumeFullRetryDistributesImages` (chain_test.go:369). They expect resume to re-derive `PlanMedia(count)` from segments with nil `Images`; once resume reads `Segment.Images`, give those test fixtures explicit `Images` slices (the resolved plan) so they still assert the same distribution.

**C6 — Per-post nostr imeta (Task 4) cannot index `imetas` by image index.** `buildImetas` (dispatch.go:591-598) SKIPS records with empty `BlossomURL`, so `imetas` is NOT 1:1 with `imgs`. Build an `imgs`-parallel imeta slice (nil where a record has no Blossom URL / imeta) and gather by `plan[i]`, OR keep imeta head-only for v1. Do NOT ship the index-by-`plan[i]` version against raw `imetas`.

**C7 — Schedule/Fire align on `MediaRecords`, not `Images`.** `Schedule` (dispatch.go:1019) splits using `len(spec.MediaRecords)` and Fire rebuilds `imgs` from `post.Media` (dispatch.go:962-978). `ImgParts` for the scheduled path must correspond to the `MediaRecords`/`post.Media` ordering.

**C8 — JS landmarks:** the image-attach entry is built at compose.js:1252 and pushed at **1268** (line 795 is the *video* path) — set the stable `id` at 1268 (and the video path). The placement chip must be `kids.push(sel)` before the `c.append(el("div",{class:"thumb"}, ...kids))` at compose.js:943 — there is **no `tile`** variable in the image branch. Adding `client_id` to the draft SELECT needs `COALESCE(client_id,'')` (drafts.go:160) and `&m.ClientID` in the Scan; the `INSERT` is the shared `insertDraftMedia` helper (drafts.go:112), covering both create and update.

**C9 — Task 4 must also update `internal/api` to keep `go test ./...` green** (the `[][]int` output shape is cross-package): folded into Task 4 below (api preview `Imgs [][]int` + the two api preview test files).

---

## File Structure

- `internal/thread/place.go` (new) — `PlaceMedia`, `SplitPlace`, `splitAtParts`. Pure placement + part-aware split.
- `internal/thread/thread.go` (modify) — thread `partOf` through `number()`/`Split` internals as needed by `SplitPlace`.
- `internal/store/models.go` (modify) — `Segment.Images []int`; `Media.ClientID string`.
- `internal/store/store.go` (modify) — `addColumnIfMissing("draft_media","client_id","TEXT")`.
- `internal/store/drafts.go` (modify) — persist/scan `draft_media.client_id`.
- `internal/dispatch/dispatch.go` (modify) — `PostSpec.ImgParts`; `runChain`/`resumeSegments`/`Schedule` use `SplitPlace` + `Segment.Images`; per-post nostr imeta.
- `internal/dispatch/card.go` (modify) — `CardPlan.Plan [][]int`; `len(plan[target])` predicate.
- `internal/api/api.go` (modify) — `postSpecJSON.ImgParts`; `/api/thread-preview` `img_parts` + `Imgs [][]int`.
- `internal/api/drafts.go` (modify) — `draftSpecJSON.Anchors`, `draftImageEntry.ID`; translate copies anchors.
- `internal/web/assets/state.js` (modify) — image `id`; `anchors`; `imgParts` in `buildSpec`.
- `internal/web/assets/compose.js` (modify) — id at attach; placement chip; anchor hygiene on remove.
- `internal/web/assets/preview.js` (modify) — per-post media from `pv.imgs [][]int`.
- `internal/web/assets/drafts.js`, `drafts_recovery.js` (modify) — anchors round-trip + recovery.

---

## Task 1: `thread.PlaceMedia` — pure two-pass placement

**Files:**
- Create: `internal/thread/place.go`
- Test: `internal/thread/place_test.go`

**Interfaces:**
- Produces: `func PlaceMedia(imgParts []int, postPart []int, cap int) [][]int` — `imgParts[i]` is the 0-based part image `i` is pinned to (caller pre-clamps); `postPart[j]` is the part post `j` belongs to; `cap<=0` means unbounded per post (nostr). Returns the attachment indices each post carries, sorted ascending within a post.

- [ ] **Step 1: Write the failing test**

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/thread/ -run TestPlaceMedia -v`
Expected: FAIL — `undefined: PlaceMedia`.

- [ ] **Step 3: Write minimal implementation**

```go
// Package thread placement: assign images to a fixed post skeleton.
package thread

import "sort"

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
		plan[len(plan)-1] = append(plan[len(plan)-1], img) // last resort
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/thread/ -run TestPlaceMedia -v`
Expected: PASS. (All table rows were hand-verified against this algorithm; if one fails, the implementation diverges from the snippet above — fix the code, not the table.)

- [ ] **Step 5: Commit**

```bash
git add internal/thread/place.go internal/thread/place_test.go
git commit -m "feat(thread): PlaceMedia — pure two-pass anchor-aware image placement"
```

---

## Task 2: `thread.SplitPlace` — part-aware split orchestrator returning `[][]int`

**Files:**
- Modify: `internal/thread/place.go`
- Modify: `internal/thread/thread.go` (add `splitAtParts`, expose `partOf` from the numbered split)
- Test: `internal/thread/place_test.go`

**Interfaces:**
- Consumes: `PlaceMedia` (Task 1), existing `splitMarkers`, `splitAt`, `number`, `Opts`.
- Produces: `func SplitPlace(text string, limit int, imgParts []int, cap int, opts Opts) (segs []string, plan [][]int, warnings []string)`. `len(segs) == len(plan)` always. With `imgParts` all zeros it yields the same `segs` as `SplitWithMedia` and a `plan` that flattens to the same counts.

- [ ] **Step 1: Write the failing test**

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/thread/ -run TestSplitPlace -v`
Expected: FAIL — `undefined: SplitPlace`.

- [ ] **Step 3a: Make `splitAt` part-aware (the primitive becomes `splitAtParts`)**

In `internal/thread/thread.go`, replace `splitAt` with `splitAtParts` (which also returns, per emitted segment, the **contiguous index over NON-EMPTY parts** — per correction C2, this is what the client's `masterParts().filter(Boolean)` produces), and make `splitAt` a thin wrapper so its existing callers are unchanged:

```go
// splitAtParts is splitAt that also returns, for each emitted segment, the part
// index it came from — a CONTIGUOUS counter over non-empty --- parts (empty
// blocks are skipped and do NOT advance the index), so it matches the client's
// masterParts() which filters empties. partOf aligns 1:1 with segs.
func splitAtParts(text string, limit int) (segs []string, partOf []int, warns []string) {
	pi := -1
	for _, u := range splitMarkers(text) {
		if u == "" {
			continue
		}
		pi++
		if limit <= 0 || graphemeLen(u) <= limit {
			segs = append(segs, u)
			partOf = append(partOf, pi)
			continue
		}
		chunks, w := packParagraphs(u, limit)
		for _, c := range chunks {
			segs = append(segs, c)
			partOf = append(partOf, pi)
		}
		warns = append(warns, w...)
	}
	return segs, partOf, warns
}

// splitAt now delegates (existing callers unchanged).
func splitAt(text string, limit int) (segs []string, warns []string) {
	segs, _, warns = splitAtParts(text, limit)
	return segs, warns
}
```
(Delete the old `splitAt` body — `splitAtParts` replaces it.)

- [ ] **Step 3b: Thread `partOf` through the real split (NOT a re-derived budget — correction C1)**

`postPart` MUST come from the exact split that produces `segs`. Make `number()` part-aware, refactor `SplitWithMedia`'s body into a part-aware core `splitWithMediaPlan` (which `SplitWithMedia` then wraps), and build `SplitPlace` on it.

In `thread.go`, change `number` to also return `partOf` (it calls `splitAtParts`; `appendCounters` preserves order, so `partOf` stays aligned), and fix its callers:
```go
func number(text string, limit, extra int) (segs []string, partOf []int, warns []string) {
	segs, partOf, warns = splitAtParts(text, limit)
	n := len(segs)
	for i := 0; i < 6; i++ {
		w := counterWidth(n + extra)
		eff := limit - w
		if eff < 1 {
			return segs, partOf, warns
		}
		segs, partOf, warns = splitAtParts(text, eff)
		if len(segs)+extra < 2 {
			return splitAtParts(text, limit)
		}
		if len(segs) == n {
			break
		}
		n = len(segs)
	}
	return appendCounters(segs, len(segs)+extra), partOf, warns
}
```
`Split` (thread.go:88) calls `number`; update its last line: `s, _, w := number(text, limit, 0); return s, w`.

In `place.go`, add the part-aware core and the two wrappers. `splitWithMediaPlan` is the existing `SplitWithMedia` body with `partOf` tracked and the appended overflow posts assigned the last part:
```go
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
```
Then make the public `SplitWithMedia` (thread.go:110) a thin wrapper so there is one implementation:
```go
func SplitWithMedia(text string, limit, nImages, cap int, opts Opts) (segs []string, plan []int, warnings []string) {
	segs, _, plan, warnings = splitWithMediaPlan(text, limit, nImages, cap, opts)
	return segs, plan, warnings
}
```
Add `import ("strconv"; "strings")` to `place.go`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/thread/ -run 'TestSplitPlace|TestPlaceMedia' -v`
Expected: PASS, including `TestSplitPlaceAnchorOverflowNumbered` (Step 1) — the case `derivePostPart` got wrong.

- [ ] **Step 5: Run the whole thread package (guard the invariant)**

Run: `go test ./internal/thread/ -v`
Expected: PASS — all existing `thread_test.go`/`media_test.go` tests stay green because `SplitWithMedia` now delegates to `splitWithMediaPlan` but returns identically. If any differ, `splitWithMediaPlan` does not faithfully reproduce the old body — fix it, do not change the tests.

- [ ] **Step 6: Commit**

```bash
git add internal/thread/place.go internal/thread/thread.go internal/thread/place_test.go
git commit -m "feat(thread): SplitPlace + part-aware split (partOf from the real numbered skeleton)"
```

---

## Task 3: Store — `Segment.Images`, `Media.ClientID`, `draft_media.client_id`

**Files:**
- Modify: `internal/store/models.go:91` (Segment), `:67` (Media)
- Modify: `internal/store/store.go` (addColumnIfMissing), `internal/store/drafts.go` (scan/insert client_id)
- Test: `internal/store/media_placement_test.go` (new)

**Interfaces:**
- Produces: `store.Segment` gains `Images []int json:"images,omitempty"`; `store.Media` gains `ClientID string json:"client_id,omitempty"`. `draft_media` gains a `client_id TEXT` column populated on draft create/update and read on draft load.

- [ ] **Step 1: Write the failing test**

```go
package store

import (
	"reflect"
	"testing"
)

func TestSegmentImagesRoundTrip(t *testing.T) {
	s := newTestStore(t) // use the same helper other store tests use
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
	s := newTestStore(t)
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
```

> Check the existing store tests (e.g. `internal/store/models_test.go`) for the real test-store constructor name and reuse it instead of `newTestStore` if it differs.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run 'TestSegmentImages|TestDraftMediaClientID' -v`
Expected: FAIL — `unknown field Images` / `unknown field ClientID`.

- [ ] **Step 3a: Add struct fields**

`internal/store/models.go` — in `Segment` (after `Error`):
```go
	Images    []int  `json:"images,omitempty"` // attachment indices this post carries (placement plan)
```
In `Media` (after `PosterURL`):
```go
	ClientID  string `json:"client_id,omitempty"` // stable per-attachment id; drafts only (anchor key)
```

`Segment.Images` rides `segments_json` automatically (it is `json.Marshal`ed at models.go:136/480 — no SQL change).

- [ ] **Step 3b: Add the column**

`internal/store/store.go` — alongside the other `addColumnIfMissing` calls (~line 74):
```go
	if err := s.addColumnIfMissing("draft_media", "client_id", "TEXT"); err != nil {
		return err
	}
```

- [ ] **Step 3c: Persist & scan `client_id`**

`internal/store/drafts.go` — the `INSERT INTO draft_media(...)` at ~line 115: add `client_id` to the column list and `m.ClientID` to the values. Find the matching `SELECT ... FROM draft_media` load query and add `client_id` to its column list and scan target (`&m.ClientID`).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/store/ -run 'TestSegmentImages|TestDraftMediaClientID' -v`
Expected: PASS.

- [ ] **Step 5: Run the whole store package**

Run: `go test ./internal/store/ -v`
Expected: PASS — additive fields/column break nothing.

- [ ] **Step 6: Commit**

```bash
git add internal/store/
git commit -m "feat(store): persist Segment.Images plan and draft_media.client_id"
```

---

## Task 4: Dispatch — card `[][]int`, `PostSpec.ImgParts`, `runChain` placement, `Segment.Images`, per-post imeta

> The Bluesky-card change and the `runChain` change are one coupled unit: `runChain`'s Bluesky branch consumes `CardPlan.Plan`, so both must move to `[][]int` together (Step 3a). Task 6 then adds only the card-fallback regression test.

**Files:**
- Modify: `internal/dispatch/card.go` (CardPlan, PlanBlueskyCard)
- Modify: `internal/dispatch/dispatch.go` (PostSpec:156, runChain:376-465)
- Test: `internal/dispatch/media_placement_test.go` (new)

**Interfaces:**
- Consumes: `thread.SplitPlace` (Task 2), `store.Segment.Images` (Task 3).
- Produces: `PostSpec.ImgParts []int` (parallel to `Images`; nil ⇒ all front-load). `runChain` signature gains an `imgParts []int` parameter after `number`. `CardPlan.Plan` becomes `[][]int`; `PlanBlueskyCard(text string, card *unfurl.Card, imgParts []int, number bool) CardPlan`. Helper `pick(imgs []Img, idx []int) []Img`.

- [ ] **Step 1: Write the failing test**

Use the existing dispatch fake-client test harness (see `internal/dispatch/dispatch_test.go` for the fake transport + how a chain result is captured). Assert that an anchored image lands on the right segment:

```go
func TestRunChainPlacesAnchoredImage(t *testing.T) {
	d, fake := newFakeDispatcher(t) // reuse the existing helper
	spec := PostSpec{
		MasterText: "a\n---\nb\n---\nc",
		Platforms:  []string{"mastodon"},
		Images:     []Img{img(t, "0"), img(t, "1")},
		ImgParts:   []int{0, 2}, // image 1 pinned to part 2 (3rd post)
		Number:     false,
	}
	out := d.dispatchOne(t, spec) // reuse however tests drive a single dispatch
	segs := fake.mastodon.postedSegments() // the per-post image counts/ids the fake recorded
	// post 0 carries image 0; post 2 carries image 1; post 1 carries none.
	if got := segs[0].imageCount; got != 1 {
		t.Fatalf("post0 images=%d want 1", got)
	}
	if got := segs[1].imageCount; got != 0 {
		t.Fatalf("post1 images=%d want 0", got)
	}
	if got := segs[2].imageCount; got != 1 {
		t.Fatalf("post2 images=%d want 1", got)
	}
}
```

> Adapt the helper names to the real ones in `dispatch_test.go`. If the fake records posted images differently, assert on whatever it exposes (e.g. the alt text or blob count per call).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/dispatch/ -run TestRunChainPlacesAnchoredImage -v`
Expected: FAIL — `unknown field ImgParts` / compile error.

- [ ] **Step 3a: Convert `card.go` to `[][]int` (coupled prerequisite)**

`internal/dispatch/card.go`: change `CardPlan.Plan` to `[][]int`, change the signature `nImages int` → `imgParts []int`, call `thread.SplitPlace` instead of `thread.SplitWithMedia`, and change the embed-slot predicate `plan[target] > 0` → `len(plan[target]) > 0`:

```go
type CardPlan struct {
	Segs     []string
	Plan     [][]int
	Warnings []string
	Text     string
	Card     *unfurl.Card
}

func PlanBlueskyCard(text string, card *unfurl.Card, imgParts []int, number bool) CardPlan {
	limit, imgCap := thread.LimitFor("bluesky"), thread.MaxImagesFor("bluesky")
	plain := func() CardPlan {
		segs, plan, warns := thread.SplitPlace(text, limit, imgParts, imgCap, thread.Opts{Number: number})
		return CardPlan{Segs: segs, Plan: plan, Warnings: warns, Text: text}
	}
	if card == nil {
		return plain()
	}
	u, trailing, ok := unfurl.CardURL(text)
	if !ok || u != card.URI {
		return plain()
	}
	eff := text
	if trailing {
		eff = unfurl.StripTrailing(text, card.URI)
		if strings.TrimSpace(eff) == "" {
			eff, trailing = text, false
		}
	}
	segs, plan, warns := thread.SplitPlace(eff, limit, imgParts, imgCap, thread.Opts{Number: number})
	target := -1
	if trailing {
		target = len(segs) - 1
	} else {
		for i, s := range segs {
			if strings.Contains(s, card.URI) {
				target = i
				break
			}
		}
	}
	if target < 0 || len(plan[target]) > 0 { // images own the embed slot ⇒ card-wins fallback (no card)
		return plain()
	}
	c := *card
	c.Segment = target
	return CardPlan{Segs: segs, Plan: plan, Warnings: warns, Text: eff, Card: &c}
}
```

- [ ] **Step 3b: Add `PostSpec.ImgParts` and thread it into `runChain`**

`internal/dispatch/dispatch.go`:
- `PostSpec` (line 156): add `ImgParts []int`.
- `runChain` signature (line 376): add `imgParts []int` after `number bool`.
- The Bluesky branch (line 381): `cp := PlanBlueskyCard(text, ov.LinkCard, imgParts, number)` — `cp.Plan` is now `[][]int`.
- Update the three `runChain` call sites (lines 687, 824, 869) to pass `spec.ImgParts` (line 687/869) or the appropriate slice. For the non-PostSpec caller at 824 (resume/retry entry `dispatchTargets`), pass `nil` for now — Task 5 wires resume.

- [ ] **Step 3c: Use `SplitPlace` in `runChain` and record `Segment.Images`**

Replace the non-bluesky split at dispatch.go:384:
```go
	} else {
		segTexts, plan, _ = thread.SplitPlace(text, thread.LimitFor(plat), imgParts, thread.MaxImagesFor(plat), thread.Opts{Number: number})
	}
```
where `plan` is now `[][]int`. (Declare `var plan [][]int` instead of `[]int` at line 379; the Bluesky branch from Step 3a's `PlanBlueskyCard` also yields `[][]int`.)

Replace the prefix-sum + slice (lines 408-422) with an index gather and record the plan:
```go
	for i, st := range segTexts {
		var replyTo *ReplyRef
		if i > 0 {
			replyTo = &ReplyRef{RootID: rootID, RootCID: rootCID, ParentID: parentID, ParentCID: parentCID}
		}
		segImgs := pick(imgs, plan[i])
		var segImetas []gonostr.Tag
		if i == 0 {
			segImetas = imetas // see Step 3c for per-post nostr imeta
		}
		...
		out.Segments[i] = store.Segment{
			Ordinal: i, Text: st, RemoteID: r.RemoteID, RemoteURL: r.RemoteURL, CID: r.CID,
			Status: r.Status, Error: r.Error, Images: plan[i],
		}
		...
	}
```
Also set `Images: plan[i]` in the up-front pending record loop (dispatch.go:405-407):
```go
	for i, st := range segTexts {
		out.Segments = append(out.Segments, store.Segment{Ordinal: i, Text: st, Status: "pending", Images: plan[i]})
	}
```

Add the helper to `dispatch.go`:
```go
// pick returns the images at the given indices, preserving order.
func pick(imgs []Img, idx []int) []Img {
	out := make([]Img, 0, len(idx))
	for _, i := range idx {
		if i >= 0 && i < len(imgs) {
			out = append(out, imgs[i])
		}
	}
	return out
}
```

- [ ] **Step 3d: Per-post nostr imeta**

`imetas []gonostr.Tag` is parallel to `imgs` (one imeta tag per image). Replace the head-only attach (dispatch.go:424) so each post gets the imeta for the images it carries:
```go
		var segImetas []gonostr.Tag
		for _, ix := range plan[i] {
			if ix < len(imetas) {
				segImetas = append(segImetas, imetas[ix])
			}
		}
```
Verify `imetas` is index-aligned with `imgs` at the call site (`buildImetas(recs)`); if it is not 1:1, fall back to head-only and note it. (Check `buildImetas`.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/dispatch/ -run TestRunChainPlacesAnchoredImage -v`
Expected: PASS.

- [ ] **Step 5: Run the dispatch package**

Run: `go test ./internal/dispatch/ -v`
Expected: PASS. Step 3a changed `CardPlan.Plan` to `[][]int`, so the existing `card_test.go` will not compile until its assertions are updated to the new shape — update them now (flatten `[][]int` to counts where a test asserted `[]int`, e.g. `len(cp.Plan[i])`). Existing chain tests stay green because nil/all-zero `ImgParts` reproduces today's behavior; fix any caller that fails to compile by passing `nil` `ImgParts`.

- [ ] **Step 6: Commit**

```bash
git add internal/dispatch/dispatch.go internal/dispatch/card.go internal/dispatch/card_test.go internal/dispatch/media_placement_test.go
git commit -m "feat(dispatch): card+chain [][]int placement, record Segment.Images"
```

---

## Task 5: Dispatch — resume & Schedule read `Segment.Images` (no re-derivation)

**Files:**
- Modify: `internal/dispatch/dispatch.go` (resumeSegments:471-552, Schedule:1041)
- Test: `internal/dispatch/media_placement_test.go`

**Interfaces:**
- Consumes: persisted `store.Segment.Images` (Task 3), `pick` (Task 4).
- Produces: resume and Fire gather each segment's images from `tg.Segments[i].Images`, not a recomputed count plan.

- [ ] **Step 1: Write the failing test**

```go
func TestResumeReadsPersistedPlacement(t *testing.T) {
	d, fake := newFakeDispatcher(t)
	// A partial target: head succeeded, tail pending, with a non-front-loaded plan
	// (image 1 on segment 2) persisted on Segment.Images.
	tg := store.Target{
		Platform: "mastodon", Status: "partial", FinalText: "a\n---\nb\n---\nc",
		Segments: []store.Segment{
			{Ordinal: 0, Text: "a", Status: "success", RemoteID: "m0", Images: []int{0}},
			{Ordinal: 1, Text: "b", Status: "pending", Images: []int{}},
			{Ordinal: 2, Text: "c", Status: "pending", Images: []int{1}},
		},
	}
	imgs := []Img{img(t, "0"), img(t, "1")}
	out := d.resumeSegments(context.Background(), tg, Overrides{}, imgs, nil)
	// Segment 2 must carry image 1 (read from Images), NOT front-fill image 0.
	if got := fake.mastodon.lastSegmentImageCount(2); got != 1 {
		t.Fatalf("resumed post2 images=%d want 1", got)
	}
	_ = out
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/dispatch/ -run TestResumeReadsPersistedPlacement -v`
Expected: FAIL — resume currently front-fills via `PlanMedia`, so post 2 gets the wrong image (or count differs).

- [ ] **Step 3: Replace count-based re-derivation with persisted reads**

In `resumeSegments` (dispatch.go:496-527): delete the `thread.PlanMedia(...)` call (502), the `starts[]` prefix-sum (503-508), and the "trailing images exceed segments" warning block (511-518). Replace the per-segment image slice (527) with:
```go
		var segImgs []Img
		if i < len(segs) {
			segImgs = pick(imgs, segs[i].Images)
		}
```
And per-post nostr imeta (dispatch.go:530), mirroring Task 4 Step 3c:
```go
		var segImetas []gonostr.Tag
		for _, ix := range segs[i].Images {
			if ix < len(imetas) {
				segImetas = append(segImetas, imetas[ix])
			}
		}
```
Keep writing `Images: segs[i].Images` back when re-recording the segment (preserve the persisted plan).

For `Schedule` (dispatch.go:1041-1048): it pre-splits with `SplitWithMedia`. Change it to `thread.SplitPlace(text, ..., spec.ImgParts, ...)` and persist `Segment.Images = plan[i]` on each pre-split segment so Fire (which goes through `resumeSegments`/`runPlatform`) reads them.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/dispatch/ -run 'TestResumeReadsPersistedPlacement|TestRunChainPlacesAnchoredImage' -v`
Expected: PASS.

- [ ] **Step 5: Run the dispatch package**

Run: `go test ./internal/dispatch/ -v`
Expected: PASS — existing resume tests green (segments saved by Task 4 carry `Images`; legacy segments with nil `Images` gather nothing, matching "no media" — verify an all-text legacy resume test still passes; if a legacy test relied on count-based re-fill, update it to set `Segment.Images` since that is now the source of truth).

- [ ] **Step 6: Commit**

```bash
git add internal/dispatch/
git commit -m "feat(dispatch): resume/Schedule read Segment.Images instead of re-deriving"
```

---

## Task 6: Bluesky card — card-wins fallback regression test

> `card.go`'s conversion to `[][]int` already landed in Task 4 (it is coupled to `runChain`). This task adds the one behavioral test that pins the **card-wins fallback**: an image anchored onto the card's post ⇒ the card reverts and the image front-loads.

**Files:**
- Test: `internal/dispatch/card_test.go`

**Interfaces:**
- Consumes: `dispatch.PlanBlueskyCard(text string, card *unfurl.Card, imgParts []int, number bool) CardPlan` with `CardPlan.Plan [][]int` (Task 4).

- [ ] **Step 1: Write the failing test**

```go
func TestPlanBlueskyCardImageOnCardPostRevertsCard(t *testing.T) {
	card := &unfurl.Card{URI: "https://ex.com"}
	// Single trailing-URL post with an image anchored to it ⇒ images own the embed
	// slot ⇒ card reverts (card-wins fallback front-loads the image).
	cp := PlanBlueskyCard("see https://ex.com", card, []int{0}, false)
	if cp.Card != nil {
		t.Fatalf("expected card reverted when image owns the post")
	}
	if len(cp.Plan) == 0 || len(cp.Plan[0]) != 1 {
		t.Fatalf("plan=%v want image on post 0", cp.Plan)
	}
}
```

- [ ] **Step 2: Run test to verify it passes (behavior already implemented in Task 4)**

Run: `go test ./internal/dispatch/ -run TestPlanBlueskyCardImageOnCardPostRevertsCard -v`
Expected: PASS — this is a characterization test guarding the Task 4 fallback. If it FAILS, the `len(plan[target]) > 0` predicate in `card.go` (Task 4 Step 3a) is wrong — fix it there.

- [ ] **Step 3: Commit**

```bash
git add internal/dispatch/card_test.go
git commit -m "test(dispatch): pin card-wins fallback when an image owns the card post"
```

---

## Task 7: API — `postSpecJSON.ImgParts`, `/api/thread-preview` `img_parts` + `Imgs [][]int`

**Files:**
- Modify: `internal/api/api.go` (postSpecJSON:825, handleAPIPost:836, thread-preview:1442-1569)
- Test: `internal/api/thread_post_test.go` and the thread-preview test

**Interfaces:**
- Consumes: `dispatch.PostSpec.ImgParts` (Task 4), `thread.SplitPlace` (Task 2), `dispatch.PlanBlueskyCard` with `[][]int` plan (Task 4).
- Produces: `/api/thread-preview` accepts `"img_parts":[...]` and returns `"imgs": [[...],...]`; `/api/post` spec accepts `"img_parts":[...]`.

- [ ] **Step 1: Write the failing test**

```go
func TestThreadPreviewReturnsPlacementIndices(t *testing.T) {
	a := newTestAPI(t) // reuse the existing API test constructor
	body := `{"text":"a\n---\nb\n---\nc","platforms":["mastodon"],"images":2,"img_parts":[0,2]}`
	rec := postJSON(t, a, "/api/thread-preview", body)
	var resp struct {
		Previews []struct {
			Platform string  `json:"platform"`
			Imgs     [][]int `json:"imgs"`
		} `json:"previews"`
	}
	decode(t, rec, &resp)
	imgs := resp.Previews[0].Imgs
	if len(imgs) != 3 || len(imgs[0]) != 1 || len(imgs[1]) != 0 || len(imgs[2]) != 1 {
		t.Fatalf("imgs=%v want [[0],[],[1]]-shaped", imgs)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api/ -run TestThreadPreviewReturnsPlacementIndices -v`
Expected: FAIL — `img_parts` unread, `Imgs` is `[]int`.

- [ ] **Step 3a: thread-preview**

`internal/api/api.go` handleThreadPreview (1442): add `ImgParts []int json:"img_parts"` to the request struct; change `preview.Imgs` to `[][]int`. Replace the planner calls:
- bluesky (1553): `cp := dispatch.PlanBlueskyCard(req.Text, card, req.ImgParts, req.Number)` (build `req.ImgParts` of length `req.Images`, defaulting missing entries to 0).
- non-bluesky (1564): `segs, plan, warns := thread.SplitPlace(req.Text, thread.LimitFor(p), imgParts, thread.MaxImagesFor(p), thread.Opts{Number: req.Number})`.

Normalize `req.ImgParts` to length `req.Images` (pad with 0, clamp negatives) before use.

- [ ] **Step 3b: post spec**

`postSpecJSON` (825): add `ImgParts []int json:"img_parts"`. In `handleAPIPost` (878): set `ImgParts: sj.ImgParts` on the `dispatch.PostSpec`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/api/ -run TestThreadPreview -v`
Expected: PASS.

- [ ] **Step 5: Run the api package**

Run: `go test ./internal/api/ -v`
Expected: PASS — existing preview tests updated to the `[][]int` shape (flatten-and-compare where they asserted counts).

- [ ] **Step 6: Commit**

```bash
git add internal/api/api.go internal/api/thread_post_test.go
git commit -m "feat(api): thread-preview + post accept img_parts, return [][]int plan"
```

---

## Task 8: API drafts — `draftSpecJSON.Anchors`, `draftImageEntry.ID`, translate copies anchors

**Files:**
- Modify: `internal/api/drafts.go` (draftImageEntry:~25, draftSpecJSON:37, buildDraftFromRequest:85, handleTranslateDraft:225)
- Test: `internal/api/drafts_crud_test.go` (or the existing drafts test file)

**Interfaces:**
- Consumes: `store.Media.ClientID` (Task 3).
- Produces: drafts persist `anchors` (in `spec_json`) and per-image `client_id` (in `draft_media`); translate carries `anchors` to the new draft.

- [ ] **Step 1: Write the failing test**

```go
func TestDraftRoundTripsAnchorsAndClientID(t *testing.T) {
	a := newTestAPI(t)
	spec := `{"master_text":"a\n---\nb","platforms":["bluesky"],"overrides":{},"tags":[],` +
		`"anchors":{"img-x":1},"images":[{"id":"img-x","blossom_url":"https://b/1","sha256":"s"}]}`
	id := createDraft(t, a, spec) // helper that posts multipart with the spec field
	got := getDraft(t, a, id)
	if got.Media[0].ClientID != "img-x" {
		t.Fatalf("client_id=%q", got.Media[0].ClientID)
	}
	var stored struct {
		Anchors map[string]int `json:"anchors"`
	}
	decodeJSON(t, got.Spec, &stored)
	if stored.Anchors["img-x"] != 1 {
		t.Fatalf("anchors=%v", stored.Anchors)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api/ -run TestDraftRoundTripsAnchors -v`
Expected: FAIL — anchors dropped, `id`/`client_id` not carried.

- [ ] **Step 3: Implement**

`internal/api/drafts.go`:
- `draftImageEntry`: add `ID string json:"id,omitempty"`.
- `draftSpecJSON`: add `Anchors json.RawMessage json:"anchors,omitempty"`. (RawMessage so it round-trips verbatim through the re-marshal at line 146.)
- In `buildDraftFromRequest` media loop (125/131): set `ClientID: img.ID` on each `store.Media`.
- In `handleTranslateDraft` (260): add `Anchors: origSpec.Anchors` to `newSpec` (media is already copied verbatim at 279, so `client_id`s — and thus anchor keys — still match).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/api/ -run 'TestDraft' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/drafts.go internal/api/drafts_crud_test.go
git commit -m "feat(api): drafts round-trip anchors + client_id; translate preserves placement"
```

---

## Task 9: Web — image id at attach, anchors in state, `img_parts` in `buildSpec`

**Files:**
- Modify: `internal/web/assets/state.js` (image model, `imageSpecs`, `buildSpec`)
- Modify: `internal/web/assets/compose.js` (attach → assign `id`; remove → no-op for id-keyed map; draft load → carry `client_id`)

> No JS test runner — verify in a browser (steps below). Keep changes minimal and follow the existing module style.

- [ ] **Step 1: Add a stable id at attach**

In `compose.js` where an image entry is pushed (the `state.images.push(entry)` sites ~795 and the video path), set `entry.id = crypto.randomUUID()`. In the draft-load reconstruction (~519-538), set `id: m.client_id || crypto.randomUUID()` so loaded images inherit their persisted id.

- [ ] **Step 2: Add `anchors` to state and serialize `img_parts`**

`state.js`:
- Add `anchors: {}` to the `state` object (keyed by image id → 0-based part index).
- Add a helper that mirrors `internal/thread.splitMarkers` so the client's part count matches the server's:
```js
export function masterParts() {
  // mirrors internal/thread.splitMarkers — split on lines that are solely "---"
  const blocks = []; let cur = [];
  for (const ln of state.master.replace(/\r\n/g, "\n").split("\n")) {
    if (ln.trim() === "---") { blocks.push(cur.join("\n").trim()); cur = []; }
    else cur.push(ln);
  }
  blocks.push(cur.join("\n").trim());
  return blocks.filter(Boolean);
}
```
- In `imageSpecs()` add `id: i.id` to both branches.
- In `buildSpec()` compute and attach `img_parts` (positional, clamped to the part count) plus `anchors` for drafts:
```js
  const nParts = masterParts().length;
  const imgs = imageSpecs();
  const img_parts = state.images
    .filter(i => !(i.video && i.phase !== "ready"))
    .map(i => Math.min(state.anchors[i.id] || 0, Math.max(0, nParts - 1)));
  const spec = {
    master_text: state.master,
    platforms: [...state.platforms],
    delay_seconds: 0,
    overrides,
    images: imgs,
    img_parts,
    anchors: state.anchors,   // for draft persistence (id-keyed); ignored by /api/post
    number: document.getElementById("threadnum")?.checked ?? true,
  };
```

- [ ] **Step 3: Remove handlers leave anchors alone**

In the two `state.images.splice(i, 1)` sites (compose.js:890, 942), no map maintenance is needed — an id-keyed `state.anchors` entry for a removed image simply orphans and is ignored on resolution. Optionally `delete state.anchors[removedImg.id]` for tidiness.

- [ ] **Step 4: Browser verification**

```bash
go run ./cmd/publisher   # or the project's run command; see /run
```
- Attach 2 images to a draft with `a\n---\nb`. Confirm posting works unchanged (no anchors set → front-load).
- Open devtools, set `state.anchors[<img1 id>] = 1`, post to a test target, confirm image lands on post 2.

- [ ] **Step 5: Commit**

```bash
git add internal/web/assets/state.js internal/web/assets/compose.js
git commit -m "feat(web): stable image id + anchors state + img_parts in buildSpec"
```

---

## Task 10: Web — placement chip (≥2 parts) + per-post preview from `[][]int`

**Files:**
- Modify: `internal/web/assets/compose.js` (`renderImages` ~860 — add the chip)
- Modify: `internal/web/assets/preview.js` (235 — per-post media from `pv.imgs`)

- [ ] **Step 1: Per-post preview rendering**

`preview.js` ~221-235: `pv.imgs` is now `[][]int`. Replace the count-slice:
```js
  const media = previewMedia(platform);
  pv.segments.forEach((seg, i) => {
    ...
    const idxs = Array.isArray(pv.imgs) && Array.isArray(pv.imgs[i]) ? pv.imgs[i] : [];
    const segMedia = idxs.map(ix => media[ix]).filter(Boolean);
    if (segMedia.length) { const g = mediaGridFrom(segMedia); if (g) main.append(g); }
    ...
  });
```
Keep the stale-bundle fallback: if `pv.imgs` entries are numbers (old shape), fall back to the existing slice logic.

- [ ] **Step 2: Placement chip in the thumbnail strip**

`compose.js` `renderImages` (~860), per thumbnail, when `masterParts().length >= 2` **and not in interaction mode** (`!state.interaction` — placement is disabled there per the spec, matching the Edit-split sheet's `editable = !it` gate), append a small select bound to `state.anchors[img.id]`:
```js
  if (masterParts().length >= 2 && !state.interaction) {
    const sel = el("select", { class: "place-chip",
      onchange: e => { state.anchors[img.id] = parseInt(e.target.value, 10); markDirty(); renderPreview(); refreshCounts(); } });
    const n = masterParts().length;
    for (let p = 0; p < n; p++) {
      const o = el("option", { value: String(p), text: `▸ part ${p + 1}` });
      if ((state.anchors[img.id] || 0) === p) o.selected = true;
      sel.append(o);
    }
    tile.append(sel); // append to the thumbnail tile container used in this function
  }
```
Import `masterParts` from `state.js`. When `masterParts().length < 2` or in interaction mode, render no chip (placement inactive).

- [ ] **Step 3: Re-render chips when the part count changes**

The master textarea `oninput` already triggers preview/counts; ensure it also calls `renderImages()` so chips appear/disappear as `---` breaks are added/removed. (Find the master `oninput` handler and add `renderImages()` if not already invoked.)

- [ ] **Step 4: Browser verification**

- Load `a\n---\nb\n---\nc` with 2 images. Confirm each thumbnail shows a `▸ part 1/2/3` chip.
- Set image 2 to "part 3"; confirm the live preview moves it to the third post on each platform tab.
- Delete a `---` so only 1 part remains; confirm chips disappear.
- Confirm the thread badge count is unchanged by moving images (placement permutes, never adds posts).

- [ ] **Step 5: Commit**

```bash
git add internal/web/assets/compose.js internal/web/assets/preview.js
git commit -m "feat(web): per-thumbnail placement chip + per-post preview media"
```

---

## Task 11: Web — drafts + recovery round-trip anchors

**Files:**
- Modify: `internal/web/assets/compose.js` (draft load: hydrate `state.anchors`)
- Modify: `internal/web/assets/drafts.js` (recovery restore), `drafts_recovery.js` (snapshot)

- [ ] **Step 1: Hydrate anchors on draft load**

Where a draft is loaded into compose (the `loadDraft`/input handler ~505-538), after rebuilding `state.images` (each with `id = m.client_id`), set `state.anchors = (input.anchors || input.spec_anchors || {})` from the draft spec. (The draft GET returns `spec` JSON; parse its `anchors`.)

- [ ] **Step 2: Recovery snapshot + restore**

`drafts_recovery.js`: the snapshot uses `buildSpec()` (already includes `anchors` after Task 9) — confirm it is persisted to localStorage.
`drafts.js` recovery restore (~274-280): when filtering to `refs` (images with `blossom_url`) drops un-uploaded images, the id-keyed `state.anchors` entries for dropped images simply orphan — no renumber needed. Set `state.anchors` from the recovered spec.

- [ ] **Step 3: Browser verification**

- Create `a\n---\nb`, attach 2 images, set image 2 → part 2, Save draft. Reload the page, open the draft. Confirm image 2's chip still reads "part 2" and the preview places it there.
- Translate the draft (if DeepL configured). Confirm the translated draft keeps the placement.
- Trigger recovery (reload mid-edit). Confirm placements for still-present (uploaded) images survive.

- [ ] **Step 4: Commit**

```bash
git add internal/web/assets/drafts.js internal/web/assets/drafts_recovery.js internal/web/assets/compose.js
git commit -m "feat(web): drafts + recovery round-trip media-placement anchors"
```

---

## Task 12: Full regression + manual e2e

- [ ] **Step 1: Full Go test suite**

Run: `go test ./...`
Expected: PASS — the zero-regression invariant (no anchors ⇒ identical output) holds across thread/dispatch/api/store.

- [ ] **Step 2: Vet**

Run: `go vet ./...`
Expected: clean.

- [ ] **Step 3: Manual e2e (per spec Testing section)**

Build and run; create a 3-part thread (`a\n---\nb\n---\nc`) with an image anchored to part 2; post to all four platforms on a test account. Confirm:
- The image appears on the 2nd post on each platform.
- `k/n` numbering is unchanged from an un-anchored post of the same text.
- Force a mid-chain stop (kill after post 1) and resume; confirm the image still lands on post 2 (read from `Segment.Images`, not re-derived).

- [ ] **Step 4: Final commit / branch is ready for PR**

```bash
git status   # clean
git log --oneline feat/media-placement   # review the task commits
```

---

## Self-Review notes (for the implementer)

- **Zero-regression** is guarded by `TestSplitPlaceNoAnchorsMatchesSplitWithMedia` (Task 2) and the full suite (Task 12). If any existing test changes output, stop — anchoring must not change the no-anchor path.
- **`derivePostPart` budget alignment** (Task 2) is the subtlest part: its re-split must reproduce `SplitWithMedia`'s segment boundaries for numbered multi-part chains. If a test shows a mismatch, the fix is to match `number()`'s effective budget exactly, never to change `SplitWithMedia`.
- **Legacy data**: targets/segments saved before this change have `Images == nil`; resume treats that as "no media on this segment," which matches their pre-feature reality (those posts were created by the old front-fill and already sent). New posts always record `Images`.
