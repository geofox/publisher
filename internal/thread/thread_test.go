package thread

import (
	"strconv"
	"strings"
	"testing"

	"github.com/rivo/uniseg"
)

func glen(s string) int { return uniseg.GraphemeClusterCount(s) }

func TestLimitFor(t *testing.T) {
	cases := map[string]int{"bluesky": 300, "mastodon": 500, "threads": 500, "nostr": 0, "weird": 0}
	for p, want := range cases {
		if got := LimitFor(p); got != want {
			t.Errorf("LimitFor(%q)=%d want %d", p, got, want)
		}
	}
}

func TestSplitNoLimitIsSingle(t *testing.T) {
	segs, warns := Split("a fairly long nostr note that has no length cap at all", 0, Opts{})
	if len(segs) != 1 || len(warns) != 0 {
		t.Fatalf("nostr no-limit should be 1 segment, got %v / warns %v", segs, warns)
	}
}

func TestSplitUnderLimitIsSingle(t *testing.T) {
	segs, _ := Split("short", 300, Opts{})
	if len(segs) != 1 || segs[0] != "short" {
		t.Fatalf("got %v", segs)
	}
}

func TestSplitMarkers(t *testing.T) {
	segs, _ := Split("one\n---\ntwo\n---\nthree", 300, Opts{})
	if len(segs) != 3 || segs[0] != "one" || segs[1] != "two" || segs[2] != "three" {
		t.Fatalf("markers: got %v", segs)
	}
}

func TestSplitMarkersHonoredAtNoLimit(t *testing.T) {
	segs, _ := Split("a\n---\nb", 0, Opts{})
	if len(segs) != 2 {
		t.Fatalf("markers at no-limit: got %v", segs)
	}
}

func TestSplitWrapsAtWordBoundary(t *testing.T) {
	words := strings.Repeat("word ", 100)
	segs, _ := Split(strings.TrimSpace(words), 50, Opts{})
	if len(segs) < 2 {
		t.Fatalf("expected multiple segments, got %d", len(segs))
	}
	for i, s := range segs {
		if glen(s) > 50 {
			t.Errorf("segment %d over limit: %d graphemes", i, glen(s))
		}
		if strings.HasPrefix(s, " ") || strings.HasSuffix(s, " ") {
			t.Errorf("segment %d has edge whitespace: %q", i, s)
		}
	}
}

func TestSplitNeverBreaksMidWord(t *testing.T) {
	segs, _ := Split("alpha bravo charlie delta echo foxtrot", 12, Opts{})
	for _, s := range segs {
		for _, w := range strings.Fields(s) {
			if w == "" {
				t.Fatalf("empty word in %q", s)
			}
		}
	}
}

func TestSplitHardSplitsGiantToken(t *testing.T) {
	url := "https://example.com/" + strings.Repeat("x", 100)
	segs, warns := Split(url, 40, Opts{})
	if len(segs) < 2 {
		t.Fatalf("giant token should hard-split, got %d", len(segs))
	}
	if len(warns) == 0 {
		t.Errorf("expected a hard-split warning")
	}
	for _, s := range segs {
		if glen(s) > 40 {
			t.Errorf("hard-split segment over limit: %d", glen(s))
		}
	}
}

func TestSplitGraphemeAware(t *testing.T) {
	fam := "👨‍👩‍👧‍👦"
	segs, _ := Split(strings.Repeat(fam+" ", 10), 3, Opts{})
	for _, s := range segs {
		if glen(s) > 3 {
			t.Errorf("emoji segment over grapheme limit: %d (%q)", glen(s), s)
		}
	}
}

func TestSplitParagraphPath(t *testing.T) {
	// Two paragraphs that each fit but together exceed the limit → break at the
	// paragraph boundary (each paragraph becomes its own segment).
	p1 := strings.Repeat("a", 40)
	p2 := strings.Repeat("b", 40)
	segs, _ := Split(p1+"\n\n"+p2, 50, Opts{})
	if len(segs) != 2 || segs[0] != p1 || segs[1] != p2 {
		t.Fatalf("paragraph packing: got %v", segs)
	}
}

func TestSplitSentencePath(t *testing.T) {
	// One paragraph, multiple sentences, over limit → break at sentence boundaries.
	segs, _ := Split("First sentence here. Second sentence here. Third sentence here.", 25, Opts{})
	if len(segs) < 2 {
		t.Fatalf("expected sentence splitting, got %v", segs)
	}
	for _, s := range segs {
		if glen(s) > 25 {
			t.Errorf("segment over limit: %q (%d)", s, glen(s))
		}
	}
}

func TestSplitNormalizesCRLF(t *testing.T) {
	segs, _ := Split("one\r\n---\r\ntwo", 300, Opts{})
	if len(segs) != 2 || segs[0] != "one" || segs[1] != "two" {
		t.Fatalf("CRLF markers: got %v", segs)
	}
	p1 := strings.Repeat("a", 40)
	p2 := strings.Repeat("b", 40)
	segs2, _ := Split(p1+"\r\n\r\n"+p2, 50, Opts{})
	if len(segs2) != 2 {
		t.Fatalf("CRLF paragraph break: got %v", segs2)
	}
}

func TestSplitLimitOne(t *testing.T) {
	segs, _ := Split("ab cd", 1, Opts{})
	if len(segs) == 0 {
		t.Fatal("expected segments")
	}
	for _, s := range segs {
		if glen(s) > 1 {
			t.Errorf("limit=1 segment too long: %q", s)
		}
	}
}

func TestNumberingAppendsCounters(t *testing.T) {
	segs, _ := Split("alpha bravo charlie delta echo foxtrot", 12, Opts{Number: true})
	if len(segs) < 2 {
		t.Fatalf("expected a multi-segment chain, got %d", len(segs))
	}
	n := len(segs)
	for i, s := range segs {
		want := " " + itoa(i+1) + "/" + itoa(n)
		if !strings.HasSuffix(s, want) {
			t.Errorf("segment %d %q missing suffix %q", i, s, want)
		}
		if glen(s) > 12 {
			t.Errorf("numbered segment %d over limit: %d (%q)", i, glen(s), s)
		}
	}
}

func TestNumberingSkippedForSingleSegment(t *testing.T) {
	segs, _ := Split("short", 300, Opts{Number: true})
	if len(segs) != 1 || strings.Contains(segs[0], "/") {
		t.Fatalf("single segment must not be numbered: %v", segs)
	}
}

func TestNumberingNeverExceedsLimit(t *testing.T) {
	body := strings.TrimSpace(strings.Repeat("lorem ipsum dolor ", 60))
	segs, _ := Split(body, 40, Opts{Number: true})
	for i, s := range segs {
		if glen(s) > 40 {
			t.Fatalf("numbered segment %d exceeds limit: %d graphemes (%q)", i, glen(s), s)
		}
	}
	last := segs[len(segs)-1]
	if !strings.HasSuffix(last, "/"+itoa(len(segs))) {
		t.Errorf("last counter wrong: %q (n=%d)", last, len(segs))
	}
}

func TestNumberingNoLimitNostr(t *testing.T) {
	// Nostr has no length limit, so a chain only forms via --- markers. Numbering
	// must still apply (the suffix has no budget to reserve — just append it).
	segs, _ := Split("part one\n---\npart two\n---\npart three", 0, Opts{Number: true})
	want := []string{"part one 1/3", "part two 2/3", "part three 3/3"}
	if len(segs) != len(want) {
		t.Fatalf("want %d segments, got %d: %v", len(want), len(segs), segs)
	}
	for i := range want {
		if segs[i] != want[i] {
			t.Errorf("seg %d = %q, want %q", i, segs[i], want[i])
		}
	}
}

func TestNumberingNoLimitSingleSegment(t *testing.T) {
	// A single no-limit segment (no markers) must stay unnumbered.
	segs, _ := Split("just one note, no markers here", 0, Opts{Number: true})
	if len(segs) != 1 || strings.Contains(segs[0], "/") {
		t.Fatalf("single nostr segment must not be numbered: %v", segs)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
