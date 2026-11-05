package dispatch

import (
	"context"
	"testing"

	gonostr "fiatjaf.com/nostr"
	"github.com/geofox/publisher/internal/store"
)

// perSegNostr records the imetas passed to each PublishText call (one per
// segment), minting a distinct event id per call so the chain advances.
type perSegNostr struct {
	calls  int
	perSeg [][]gonostr.Tag
}

func (p *perSegNostr) PublishText(ctx context.Context, text string, pow *int, imetas []gonostr.Tag, replyTo *ReplyRef) (legacyResult, error) {
	p.perSeg = append(p.perSeg, imetas)
	p.calls++
	return legacyResult{Platform: "nostr", Status: "success", RemoteID: "ev" + itoa(p.calls)}, nil
}
func (p *perSegNostr) RebroadcastToRelay(context.Context, string, string) (bool, string) {
	return true, ""
}
func (p *perSegNostr) Repost(context.Context, string, string, int, string) (legacyResult, error) {
	return legacyResult{}, nil
}
func (p *perSegNostr) Quote(context.Context, string, string, string, string, []gonostr.Tag) (legacyResult, error) {
	return legacyResult{}, nil
}

// itoa is defined in chain_test.go — do not redefine.

func TestResumeSegmentsNostrPlacesImetasPerSegment(t *testing.T) {
	cn := &perSegNostr{}
	d := &Dispatcher{Store: openDispatchStore(t), Accounts: legacyAccounts(legacy{Nostr: cn})}
	imgs := []Img{{BlossomURL: "https://b/x"}, {BlossomURL: "https://b/y"}}
	tg := store.Target{Platform: "nostr", Segments: []store.Segment{
		{Ordinal: 0, Text: "a", Status: "pending", Images: []int{0}},
		{Ordinal: 1, Text: "b", Status: "pending", Images: []int{1}},
	}}
	o := d.resumeSegments(context.Background(), tg, Overrides{}, imgs, "t")
	if cn.calls != 2 {
		t.Fatalf("expected 2 posts, got %d", cn.calls)
	}
	if len(cn.perSeg[0]) != 1 || len(cn.perSeg[1]) != 1 {
		t.Fatalf("resume imetas per segment = [%d,%d], want [1,1]", len(cn.perSeg[0]), len(cn.perSeg[1]))
	}
	_ = o
}

func TestResumeSegmentsNostrLegacyHeadOnly(t *testing.T) {
	cn := &perSegNostr{}
	d := &Dispatcher{Store: openDispatchStore(t), Accounts: legacyAccounts(legacy{Nostr: cn})}
	imgs := []Img{{BlossomURL: "https://b/x"}, {BlossomURL: "https://b/y"}}
	// Legacy: segments saved before placement → nil Images.
	tg := store.Target{Platform: "nostr", Segments: []store.Segment{
		{Ordinal: 0, Text: "a", Status: "pending"},
		{Ordinal: 1, Text: "b", Status: "pending"},
	}}
	d.resumeSegments(context.Background(), tg, Overrides{}, imgs, "t")
	if len(cn.perSeg[0]) != 2 || len(cn.perSeg[1]) != 0 {
		t.Fatalf("legacy imetas = [%d,%d], want [2,0] (head-only, no regression)", len(cn.perSeg[0]), len(cn.perSeg[1]))
	}
}

func TestRunChainNostrPlacesImetasPerSegment(t *testing.T) {
	cn := &perSegNostr{}
	d := &Dispatcher{Store: openDispatchStore(t), Accounts: legacyAccounts(legacy{Nostr: cn})}
	imgs := []Img{{BlossomURL: "https://b/x", Mime: "image/png"}, {BlossomURL: "https://b/y", Mime: "image/jpeg"}}
	// 2-part thread; image 0 → head (part 0), image 1 → part 1 (second post).
	o := d.runChain(context.Background(), "nostr", "a\n---\nb", Overrides{}, imgs, false, []int{0, 1}, nil, "t")
	if o.Status != "success" {
		t.Fatalf("status=%s", o.Status)
	}
	if cn.calls != 2 {
		t.Fatalf("expected 2 segment posts, got %d", cn.calls)
	}
	if len(cn.perSeg[0]) != 1 || len(cn.perSeg[1]) != 1 {
		t.Fatalf("imetas per segment = [%d,%d], want [1,1] (one image each, not both on head)", len(cn.perSeg[0]), len(cn.perSeg[1]))
	}
}
