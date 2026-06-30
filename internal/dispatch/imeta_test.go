package dispatch

import (
	"context"
	"strings"
	"testing"

	gonostr "fiatjaf.com/nostr"
	"github.com/geofox/publisher/internal/store"
)

func TestBuildImetasIndexParallel(t *testing.T) {
	recs := []store.Media{
		{BlossomURL: "https://b/x", Mime: "image/png", SHA256: "aa", Dim: "10x10"},
		{BlossomURL: ""}, // no Blossom URL → empty placeholder, alignment preserved
		{BlossomURL: "https://b/z", Mime: "image/jpeg", SHA256: "cc"},
	}
	got := buildImetas(recs)
	if len(got) != 3 {
		t.Fatalf("buildImetas len=%d, want 3 (index-parallel)", len(got))
	}
	if len(got[1]) != 0 {
		t.Errorf("rec without Blossom URL should be an empty placeholder, got %v", got[1])
	}
	if s := strings.Join(got[0], " "); !strings.Contains(s, "url https://b/x") {
		t.Errorf("got[0]=%q", s)
	}
}

func TestPickImetasSelectsAndDropsEmpties(t *testing.T) {
	recs := []store.Media{
		{BlossomURL: "https://b/x", Mime: "image/png", SHA256: "aa"},
		{BlossomURL: ""},
		{BlossomURL: "https://b/z", Mime: "image/jpeg", SHA256: "cc"},
	}
	imetas := buildImetas(recs)
	// Select indices 1 (empty) and 2 (real): only the real one survives.
	got := pickImetas(imetas, []int{1, 2})
	if len(got) != 1 {
		t.Fatalf("pickImetas dropped-empty len=%d, want 1", len(got))
	}
	if s := strings.Join(got[0], " "); !strings.Contains(s, "url https://b/z") {
		t.Errorf("got[0]=%q", s)
	}
	// Out-of-range index is skipped, not a panic.
	if n := len(pickImetas(imetas, []int{0, 9})); n != 1 {
		t.Errorf("out-of-range skip len=%d, want 1", n)
	}
}

// perSegNostr records the imetas passed to each PublishText call (one per
// segment), minting a distinct event id per call so the chain advances.
type perSegNostr struct {
	calls  int
	perSeg [][]gonostr.Tag
}

func (p *perSegNostr) PublishText(ctx context.Context, text string, pow *int, imetas []gonostr.Tag, replyTo *ReplyRef) (TargetResult, error) {
	p.perSeg = append(p.perSeg, imetas)
	p.calls++
	return TargetResult{Platform: "nostr", Status: "success", RemoteID: "ev" + itoa(p.calls)}, nil
}
func (p *perSegNostr) RebroadcastToRelay(context.Context, string, string) (bool, string) {
	return true, ""
}
func (p *perSegNostr) Repost(context.Context, string, string, int, string) (TargetResult, error) {
	return TargetResult{}, nil
}
func (p *perSegNostr) Quote(context.Context, string, string, string, string, []gonostr.Tag) (TargetResult, error) {
	return TargetResult{}, nil
}

// NOTE: `itoa` already exists in internal/dispatch/chain_test.go:46
// (`func itoa(n int) string { return strconv.Itoa(n) }`) — DO NOT redefine it
// (duplicate symbol = package won't compile). Reuse it.

func TestRunChainNostrPlacesImetasPerSegment(t *testing.T) {
	cn := &perSegNostr{}
	d := &Dispatcher{Nostr: cn}
	imgs := []Img{{BlossomURL: "https://b/x", Mime: "image/png"}, {BlossomURL: "https://b/y", Mime: "image/jpeg"}}
	recs := []store.Media{{BlossomURL: "https://b/x", Mime: "image/png", SHA256: "aa"}, {BlossomURL: "https://b/y", Mime: "image/jpeg", SHA256: "bb"}}
	imetas := buildImetas(recs)
	// 2-part thread; image 0 → head (part 0), image 1 → part 1 (second post).
	o := d.runChain(context.Background(), "nostr", "a\n---\nb", Overrides{}, imgs, imetas, false, []int{0, 1}, nil)
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
