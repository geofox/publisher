package dispatch

import (
	"strings"
	"testing"

	"github.com/geofox/publisher/internal/unfurl"
)

func card(uri string) *unfurl.Card {
	return &unfurl.Card{URI: uri, Title: "T", Description: "D"}
}

func TestPlanCardNilCardIsPlainSplit(t *testing.T) {
	cp := PlanBlueskyCard("hello https://x.com/a", nil, nil, true)
	if cp.Card != nil || cp.Text != "hello https://x.com/a" || len(cp.Segs) != 1 {
		t.Fatalf("plan: %+v", cp)
	}
}

func TestPlanCardTrailingStripsURL(t *testing.T) {
	cp := PlanBlueskyCard("hello https://x.com/a", card("https://x.com/a"), nil, true)
	if cp.Card == nil || cp.Card.Segment != 0 {
		t.Fatalf("card: %+v", cp.Card)
	}
	if cp.Text != "hello" || cp.Segs[0] != "hello" {
		t.Fatalf("strip: text=%q segs=%v", cp.Text, cp.Segs)
	}
}

func TestPlanCardMidTextKeepsURL(t *testing.T) {
	cp := PlanBlueskyCard("see https://x.com/a for more", card("https://x.com/a"), nil, true)
	if cp.Card == nil || cp.Card.Segment != 0 {
		t.Fatalf("card: %+v", cp.Card)
	}
	if cp.Text != "see https://x.com/a for more" {
		t.Fatalf("mid-text must not strip: %q", cp.Text)
	}
}

func TestPlanCardImagesWinRevert(t *testing.T) {
	cp := PlanBlueskyCard("hello https://x.com/a", card("https://x.com/a"), []int{0, 0}, true)
	if cp.Card != nil {
		t.Fatal("images own the embed slot — card must revert")
	}
	if cp.Text != "hello https://x.com/a" {
		t.Fatalf("revert must restore the URL: %q", cp.Text)
	}
}

func TestPlanCardThreadTrailingGoesToLastSegment(t *testing.T) {
	long := strings.Repeat("word ", 120) + "\nhttps://x.com/a" // > 300 graphemes → threads
	cp := PlanBlueskyCard(long, card("https://x.com/a"), nil, true)
	if len(cp.Segs) < 2 {
		t.Fatalf("expected a thread, got %d segs", len(cp.Segs))
	}
	if cp.Card == nil || cp.Card.Segment != len(cp.Segs)-1 {
		t.Fatalf("card segment: %+v of %d segs", cp.Card, len(cp.Segs))
	}
	for _, s := range cp.Segs {
		if strings.Contains(s, "https://x.com/a") {
			t.Fatal("URL must be stripped from every segment")
		}
	}
}

func TestPlanCardURLMismatchIsPlain(t *testing.T) {
	cp := PlanBlueskyCard("hello https://other.com/b", card("https://x.com/a"), nil, true)
	if cp.Card != nil {
		t.Fatal("stale card (text edited) must be dropped")
	}
}

func TestPlanCardURLOnlyPostKeepsURL(t *testing.T) {
	cp := PlanBlueskyCard("https://x.com/a", card("https://x.com/a"), nil, true)
	if cp.Card == nil || cp.Text != "https://x.com/a" {
		t.Fatalf("URL-only post: card=%v text=%q", cp.Card, cp.Text)
	}
}

func TestPlanCardMidTextImagesOnHeadSegmentReverts(t *testing.T) {
	// The mid-text URL lands on the image-bearing head segment; the rule is a
	// full revert — never a fallback to a later, URL-free segment.
	cp := PlanBlueskyCard("see https://x.com/a for more", card("https://x.com/a"), []int{0, 0}, true)
	if cp.Card != nil {
		t.Fatal("images on the head segment must revert even for a mid-text URL")
	}
	if cp.Text != "see https://x.com/a for more" {
		t.Fatalf("revert must restore text: %q", cp.Text)
	}
}

func TestPlanCardTrailingWithoutNumbering(t *testing.T) {
	cp := PlanBlueskyCard("hello https://x.com/a", card("https://x.com/a"), nil, false)
	if cp.Card == nil || cp.Card.Segment != 0 || cp.Text != "hello" {
		t.Fatalf("number=false must not change card planning: %+v", cp)
	}
}
