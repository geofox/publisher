package dispatch

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/store"
)

// fakeBsky is a scriptable BlueskyPoster: it records each call (text, replyTo,
// image count) and returns sequential ids/cids, failing from failAt onward.
type fakeBsky struct {
	calls  []fakeCall
	failAt int // index (0-based) from which calls fail; -1 = never
}
type fakeCall struct {
	text    string
	replyTo *ReplyRef
	nImgs   int
}

func (f *fakeBsky) PostBsky(_ context.Context, text string, _ Overrides, imgs []Img, replyTo *ReplyRef) (TargetResult, error) {
	i := len(f.calls)
	f.calls = append(f.calls, fakeCall{text: text, replyTo: replyTo, nImgs: len(imgs)})
	if f.failAt >= 0 && i >= f.failAt {
		return TargetResult{Platform: "bluesky", Status: "failed", Error: "boom"}, nil
	}
	return TargetResult{
		Platform: "bluesky", Status: "success",
		RemoteID: "at://post" + itoa(i), RemoteURL: "https://bsky/" + itoa(i), CID: "cid" + itoa(i),
	}, nil
}

func (f *fakeBsky) RepostBsky(context.Context, string, string) (TargetResult, error) {
	return TargetResult{Platform: "bluesky", Status: "success"}, nil
}
func (f *fakeBsky) QuoteBsky(context.Context, string, Overrides, []Img, string, string) (TargetResult, error) {
	return TargetResult{Platform: "bluesky", Status: "success"}, nil
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestRunPlatformForwardsReplyAndCID(t *testing.T) {
	f := &fakeBsky{failAt: -1}
	d := &Dispatcher{Bluesky: f}
	ref := &ReplyRef{RootID: "r", RootCID: "rc", ParentID: "p", ParentCID: "pc"}
	r := d.runPlatform(context.Background(), "bluesky", "hi", Overrides{}, nil, nil, ref)
	if r.Status != "success" || r.CID != "cid0" {
		t.Fatalf("result: %+v", r)
	}
	if len(f.calls) != 1 || f.calls[0].replyTo != ref {
		t.Fatalf("replyTo not forwarded: %+v", f.calls)
	}
}

func TestRunChainThreadsSegments(t *testing.T) {
	f := &fakeBsky{failAt: -1}
	d := &Dispatcher{Bluesky: f}
	text := "aaa\n---\nbbb\n---\nccc" // 3 segments via --- markers (deterministic)
	out := d.runChain(context.Background(), "bluesky", text, Overrides{}, nil, nil, false, nil)

	if out.Status != "success" {
		t.Fatalf("status=%s segs=%+v", out.Status, out.Segments)
	}
	if len(out.Segments) != 3 {
		t.Fatalf("want 3 segments, got %d", len(out.Segments))
	}
	if out.HeadRemoteID != "at://post0" {
		t.Errorf("head remote id = %q", out.HeadRemoteID)
	}
	if f.calls[0].replyTo != nil {
		t.Errorf("head must not reply: %+v", f.calls[0].replyTo)
	}
	if f.calls[1].replyTo == nil || f.calls[1].replyTo.ParentID != "at://post0" || f.calls[1].replyTo.RootID != "at://post0" {
		t.Errorf("seg1 reply wrong: %+v", f.calls[1].replyTo)
	}
	if f.calls[2].replyTo.ParentID != "at://post1" || f.calls[2].replyTo.RootID != "at://post0" {
		t.Errorf("seg2 reply wrong: %+v", f.calls[2].replyTo)
	}
	if f.calls[2].replyTo.ParentCID != "cid1" || f.calls[2].replyTo.RootCID != "cid0" {
		t.Errorf("seg2 cids wrong: %+v", f.calls[2].replyTo)
	}
}

func TestRunChainStopsOnFailure(t *testing.T) {
	f := &fakeBsky{failAt: 1} // segment 0 ok, segment 1 fails
	d := &Dispatcher{Bluesky: f}
	out := d.runChain(context.Background(), "bluesky", "aaa\n---\nbbb\n---\nccc", Overrides{}, nil, nil, false, nil)
	if out.Status != "partial" {
		t.Fatalf("status=%s", out.Status)
	}
	if len(out.Segments) != 3 { // all planned segments recorded; the tail stays pending
		t.Fatalf("want 3 recorded segments, got %d: %+v", len(out.Segments), out.Segments)
	}
	if out.Segments[0].Status != "success" || out.Segments[1].Status != "failed" {
		t.Errorf("seg statuses wrong: %+v", out.Segments)
	}
	if out.Segments[2].Status != "pending" || out.Segments[2].RemoteID != "" {
		t.Errorf("unattempted tail should be pending with no id: %+v", out.Segments[2])
	}
	if len(f.calls) != 2 { // posting stops after the failure
		t.Errorf("should have stopped after the failure: %d calls", len(f.calls))
	}
}

func TestResumeSkipsLiveSegments(t *testing.T) {
	tg := store.Target{
		ID: 9, Platform: "bluesky", Status: "partial",
		Segments: []store.Segment{
			{Ordinal: 0, Text: "aaa", RemoteID: "at://HEAD", CID: "cidHEAD", Status: "success"},
			{Ordinal: 1, Text: "bbb", RemoteID: "at://LIVE1", CID: "cidLIVE1", Status: "partial"}, // live but partial
			{Ordinal: 2, Text: "ccc", Status: "pending"},                                          // never sent
		},
	}
	f := &fakeBsky{failAt: -1}
	out := (&Dispatcher{Bluesky: f}).resumeSegments(context.Background(), tg, Overrides{}, nil, nil)

	if len(f.calls) != 1 { // ONLY seg2 reposts; the live partial seg1 is left alone (no duplicate)
		t.Fatalf("expected 1 repost, got %d: %+v", len(f.calls), f.calls)
	}
	if f.calls[0].text != "ccc" {
		t.Errorf("should repost only the unsent segment: %+v", f.calls[0])
	}
	// seg2 threads onto the live seg1 (parent) with root = head.
	if f.calls[0].replyTo == nil || f.calls[0].replyTo.ParentID != "at://LIVE1" || f.calls[0].replyTo.RootID != "at://HEAD" {
		t.Errorf("seg2 reply wrong: %+v", f.calls[0].replyTo)
	}
	// seg1 must be untouched (still its original live id/status).
	if out.Segments[1].RemoteID != "at://LIVE1" || out.Segments[1].Status != "partial" {
		t.Errorf("live partial seg1 was modified: %+v", out.Segments[1])
	}
	if out.Segments[2].Status != "success" || out.Segments[2].RemoteID == "" {
		t.Errorf("seg2 not resumed: %+v", out.Segments[2])
	}
}

func TestResumePostsPendingTail(t *testing.T) {
	tg := store.Target{
		ID: 11, Platform: "bluesky", Status: "partial",
		Segments: []store.Segment{
			{Ordinal: 0, Text: "aaa", RemoteID: "at://HEAD", CID: "cidHEAD", Status: "success"},
			{Ordinal: 1, Text: "bbb", Status: "failed", Error: "boom"}, // failed, no id
			{Ordinal: 2, Text: "ccc", Status: "pending"},               // never attempted
		},
	}
	f := &fakeBsky{failAt: -1}
	out := (&Dispatcher{Bluesky: f}).resumeSegments(context.Background(), tg, Overrides{}, nil, nil)
	if len(f.calls) != 2 {
		t.Fatalf("expected 2 reposts (failed + pending tail), got %d", len(f.calls))
	}
	if f.calls[0].text != "bbb" || f.calls[1].text != "ccc" {
		t.Errorf("repost order wrong: %+v", f.calls)
	}
	if out.Status != "success" {
		t.Errorf("status should be success after full resume: %s segs=%+v", out.Status, out.Segments)
	}
}

func TestRunChainSingleSegmentNoChain(t *testing.T) {
	f := &fakeBsky{failAt: -1}
	d := &Dispatcher{Bluesky: f}
	out := d.runChain(context.Background(), "bluesky", "short", Overrides{}, nil, nil, false, nil)
	if len(out.Segments) != 0 {
		t.Fatalf("single post must have no Segments: %+v", out.Segments)
	}
	if out.Status != "success" || out.HeadRemoteID != "at://post0" {
		t.Errorf("single outcome wrong: %+v", out)
	}
	if f.calls[0].replyTo != nil {
		t.Errorf("single post must not reply")
	}
}

func TestRunChainImagesUnderCapStayOnHead(t *testing.T) {
	// When the number of images is at or below the platform cap, the plan is
	// [n, 0, ...]: all images land on the head segment, none on subsequent ones.
	f := &fakeBsky{failAt: -1}
	d := &Dispatcher{Bluesky: f}
	imgs := []Img{{BlossomURL: "https://b/x"}}
	d.runChain(context.Background(), "bluesky", "aaa\n---\nbbb", Overrides{}, imgs, nil, false, nil)
	if f.calls[0].nImgs != 1 {
		t.Errorf("head should carry images: %d", f.calls[0].nImgs)
	}
	if f.calls[1].nImgs != 0 {
		t.Errorf("non-head segments must carry no images: %d", f.calls[1].nImgs)
	}
}

func TestResumeSegmentsContinuesFromFailure(t *testing.T) {
	// Stored target: head succeeded (distinct id "at://HEAD"), segment 1 failed,
	// segment 2 never attempted. resumeSegments reposts seg1 and seg2.
	tg := store.Target{
		ID: 7, Platform: "bluesky", Status: "partial",
		Segments: []store.Segment{
			{Ordinal: 0, Text: "aaa", RemoteID: "at://HEAD", CID: "cidHEAD", Status: "success"},
			{Ordinal: 1, Text: "bbb", Status: "failed", Error: "boom"},
			{Ordinal: 2, Text: "ccc", Status: "failed"},
		},
	}
	f := &fakeBsky{failAt: -1}
	d := &Dispatcher{Bluesky: f}
	out := d.resumeSegments(context.Background(), tg, Overrides{}, nil, nil)

	if out.Status != "success" {
		t.Fatalf("status=%s segs=%+v", out.Status, out.Segments)
	}
	if len(f.calls) != 2 { // only seg1 and seg2 reposted; head untouched
		t.Fatalf("expected 2 reposts, got %d", len(f.calls))
	}
	// seg1 (resume call 0): parent and root are the STORED head "at://HEAD".
	if f.calls[0].text != "bbb" || f.calls[0].replyTo == nil ||
		f.calls[0].replyTo.RootID != "at://HEAD" || f.calls[0].replyTo.ParentID != "at://HEAD" ||
		f.calls[0].replyTo.RootCID != "cidHEAD" || f.calls[0].replyTo.ParentCID != "cidHEAD" {
		t.Errorf("seg1 reply wrong: %+v", f.calls[0])
	}
	// seg2 (resume call 1): root still stored head; parent is the freshly-posted
	// seg1, which is fakeBsky call index 0 → "at://post0"/"cid0".
	if f.calls[1].text != "ccc" || f.calls[1].replyTo.RootID != "at://HEAD" ||
		f.calls[1].replyTo.ParentID != "at://post0" || f.calls[1].replyTo.ParentCID != "cid0" {
		t.Errorf("seg2 reply wrong: %+v", f.calls[1])
	}
	if out.Segments[1].RemoteID != "at://post0" || out.Segments[2].RemoteID != "at://post1" {
		t.Errorf("resumed segment ids wrong: %+v", out.Segments)
	}
}

// fakeMastoChain records PostText calls so chain media-plan tests can assert
// the per-segment image distribution.
type fakeMastoChain struct{ calls []fakeCall }

func (f *fakeMastoChain) PostText(_ context.Context, text string, _ Overrides, imgs []Img, replyTo *ReplyRef) (TargetResult, error) {
	i := len(f.calls)
	f.calls = append(f.calls, fakeCall{text: text, replyTo: replyTo, nImgs: len(imgs)})
	return TargetResult{
		Platform: "mastodon", Status: "success",
		RemoteID: "m" + itoa(i), RemoteURL: "https://m/" + itoa(i),
	}, nil
}
func (f *fakeMastoChain) Reblog(context.Context, string) (TargetResult, error) {
	return TargetResult{Platform: "mastodon", Status: "success"}, nil
}
func (f *fakeMastoChain) QuoteStatus(context.Context, string, string, []Img) (TargetResult, error) {
	return TargetResult{Platform: "mastodon", Status: "success"}, nil
}

func TestRunChainSplitsImagesOverMastodonCap(t *testing.T) {
	f := &fakeMastoChain{}
	d := &Dispatcher{Mastodon: f}
	out := d.runChain(context.Background(), "mastodon", "hello", Overrides{}, make([]Img, 10), nil, false, nil)
	if out.Status != "success" {
		t.Fatalf("status=%s err=%s", out.Status, out.Error)
	}
	if len(f.calls) != 3 {
		t.Fatalf("want 3 posts (4+4+2), got %d", len(f.calls))
	}
	for i, want := range []int{4, 4, 2} {
		if f.calls[i].nImgs != want {
			t.Errorf("seg%d images=%d want %d", i, f.calls[i].nImgs, want)
		}
	}
	if f.calls[0].text != "hello" || f.calls[1].text != "" || f.calls[2].text != "" {
		t.Errorf("texts wrong: %+v", f.calls)
	}
	if f.calls[1].replyTo == nil || f.calls[1].replyTo.RootID != "m0" {
		t.Errorf("image-only segments must thread under the head: %+v", f.calls[1].replyTo)
	}
	if len(out.Segments) != 3 {
		t.Errorf("want 3 recorded segments, got %d", len(out.Segments))
	}
}

func TestRunChainBlueskyTenImagesSinglePost(t *testing.T) {
	f := &fakeBsky{failAt: -1}
	d := &Dispatcher{Bluesky: f}
	out := d.runChain(context.Background(), "bluesky", "hello", Overrides{}, make([]Img, 10), nil, false, nil)
	if out.Status != "success" || len(f.calls) != 1 || f.calls[0].nImgs != 10 {
		t.Fatalf("want single post with 10 images: %+v (status=%s)", f.calls, out.Status)
	}
	if len(out.Segments) != 0 {
		t.Errorf("single post must not record segments: %+v", out.Segments)
	}
}

func TestRunChainSpreadsImagesAcrossTextSegments(t *testing.T) {
	f := &fakeMastoChain{}
	d := &Dispatcher{Mastodon: f}
	out := d.runChain(context.Background(), "mastodon", "one\n---\ntwo\n---\nthree", Overrides{}, make([]Img, 10), nil, false, nil)
	if out.Status != "success" || len(f.calls) != 3 {
		t.Fatalf("calls=%d status=%s", len(f.calls), out.Status)
	}
	for i, want := range []int{4, 4, 2} {
		if f.calls[i].nImgs != want {
			t.Errorf("seg%d images=%d want %d", i, f.calls[i].nImgs, want)
		}
	}
	for i, want := range []string{"one", "two", "three"} {
		if f.calls[i].text != want {
			t.Errorf("seg%d text=%q want %q", i, f.calls[i].text, want)
		}
	}
}

func TestRetryResumesPartialThread(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "resume.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	f := &fakeBsky{failAt: -1}
	d := &Dispatcher{Bluesky: f, Store: db}

	rec := &store.Post{
		ID: "rp", CreatedAt: time.Now().UTC(), Platforms: []string{"bluesky"}, Source: "web", Status: "partial",
		Targets: []store.Target{{
			Platform: "bluesky", FinalText: "x", Status: "partial", RemoteID: "at://HEAD",
			Segments: []store.Segment{
				{Ordinal: 0, Text: "aaa", RemoteID: "at://HEAD", CID: "cidHEAD", Status: "success"},
				{Ordinal: 1, Text: "bbb", Status: "failed", Error: "boom"},
			},
			Attempts: []store.Attempt{{AttemptNo: 1, Status: "partial", AttemptedAt: time.Now()}},
		}},
	}
	if err := db.SavePost(rec); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Retry(context.Background(), "rp", nil); err != nil {
		t.Fatal(err)
	}
	after, _ := db.GetPost("rp")
	tg := after.Targets[0]
	if tg.Status != "success" {
		t.Fatalf("resumed target status=%s segs=%+v", tg.Status, tg.Segments)
	}
	if len(tg.Segments) != 2 || tg.Segments[1].Status != "success" || tg.Segments[1].RemoteID == "" {
		t.Errorf("segment 1 not resumed: %+v", tg.Segments)
	}
}

func TestResumeRepostsSegmentImageSlices(t *testing.T) {
	f := &fakeMastoChain{}
	d := &Dispatcher{Mastodon: f}
	tg := store.Target{
		Platform: "mastodon",
		Segments: []store.Segment{
			{Ordinal: 0, Text: "hello", RemoteID: "m0", Status: "success"},
			{Ordinal: 1, Text: "", Status: "failed"},
			{Ordinal: 2, Text: "", Status: "pending"},
		},
	}
	out := d.resumeSegments(context.Background(), tg, Overrides{}, make([]Img, 10), nil)
	if out.Status != "success" {
		t.Fatalf("status=%s err=%s", out.Status, out.Error)
	}
	if len(f.calls) != 2 {
		t.Fatalf("want 2 re-posts (segments 1 and 2), got %d", len(f.calls))
	}
	// Plan for 10 images over 3 segments at cap 4 is [4,4,2]: the resumed
	// segments must carry THEIR slices, not zero (old head-only rule).
	if f.calls[0].nImgs != 4 || f.calls[1].nImgs != 2 {
		t.Errorf("resumed image counts = %d,%d want 4,2", f.calls[0].nImgs, f.calls[1].nImgs)
	}
}
