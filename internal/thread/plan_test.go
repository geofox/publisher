package thread

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/geofox/publisher/internal/destination"
)

var mastodonCounting = destination.Counting{URLLength: 23, MentionLocalPartOnly: true}

// checkRanges holds the planner's invariants: posts in order, no overlap,
// every non-space character carried exactly once, each post's text its
// range plus its counter, and within the limit.
func checkRanges(t *testing.T, text string, items []Item, r Rules) {
	t.Helper()
	rs := []rune(text)
	covered := make([]bool, len(rs))
	prevTo := 0
	for i, it := range items {
		if it.From < prevTo || it.To < it.From || it.To > len(rs) {
			t.Fatalf("item %d range [%d,%d) out of order (prev ended %d)", i, it.From, it.To, prevTo)
		}
		prevTo = it.To
		body := string(rs[it.From:it.To])
		if !strings.HasPrefix(it.Text, body) {
			t.Fatalf("item %d text %q doesn't start with its range %q", i, it.Text, body)
		}
		if strings.TrimSpace(body) != body {
			t.Fatalf("item %d range has edge whitespace: %q", i, body)
		}
		for k := it.From; k < it.To; k++ {
			covered[k] = true
		}
		if r.Limit > 0 && it.Length > r.Limit {
			t.Fatalf("item %d counts %d, over the limit %d: %q", i, it.Length, r.Limit, it.Text)
		}
		if it.Length != r.Counting.Count(it.Text) {
			t.Fatalf("item %d Length %d, want %d", i, it.Length, r.Counting.Count(it.Text))
		}
	}
	for k, c := range covered {
		if !c && !unicode.IsSpace(rs[k]) {
			t.Fatalf("character %d (%q) is in no post", k, rs[k])
		}
	}
}

func TestPlanRangesProperty(t *testing.T) {
	words := []string{"a", "word", "longer-word", "https://example.com/some/path", "#tag", "@ann@example.social",
		"end.", "Why?", "ok!", "\n\n", "\n", "😀", "👨‍👩‍👧", "日本語", "é"}
	rng := rand.New(rand.NewSource(1))
	for n := 0; n < 400; n++ {
		var b strings.Builder
		for k := rng.Intn(120); k > 0; k-- {
			b.WriteString(words[rng.Intn(len(words))])
			b.WriteString(" ")
		}
		text := b.String()
		nr := len([]rune(text))
		var breaks []int
		for k := rng.Intn(4); k > 0 && nr > 0; k-- {
			breaks = append(breaks, rng.Intn(nr))
		}
		media := make([]int, rng.Intn(6))
		for k := range media {
			media[k] = rng.Intn(nr + 1)
		}
		counting := destination.Counting{}
		if n%3 == 1 {
			counting = mastodonCounting
		} else if n%3 == 2 {
			counting = destination.Counting{EmojiUTF8Bytes: true}
		}
		r := Rules{Limit: []int{0, 30, 60, 300}[n%4], Counting: counting, ImgCap: n % 5, Number: n%2 == 0}
		items, _ := Plan(text, Layout{Breaks: breaks, MediaAt: media}, r)
		checkRanges(t, text, items, r)
		seen := map[int]bool{}
		for _, it := range items {
			if r.ImgCap > 0 && len(it.Media) > r.ImgCap {
				t.Fatalf("post carries %d media, cap %d", len(it.Media), r.ImgCap)
			}
			for _, m := range it.Media {
				if seen[m] {
					t.Fatalf("media %d on two posts", m)
				}
				seen[m] = true
			}
		}
		if len(seen) != len(media) {
			t.Fatalf("%d of %d media placed", len(seen), len(media))
		}
		again, _ := Plan(text, Layout{Breaks: breaks, MediaAt: media}, r)
		if !reflect.DeepEqual(items, again) {
			t.Fatal("planning twice gave different plans")
		}
	}
}

func TestPlanParagraphsAsTheServerAlwaysDid(t *testing.T) {
	// Three 159-character paragraphs at Bluesky's 300: each paragraph is a
	// post (the composer's old word packer said two).
	para := strings.TrimSpace(strings.Repeat("word ", 32))
	text := para + "\n\n" + para + "\n\n" + para
	items, _ := Plan(text, Layout{}, Rules{Limit: 300, Number: true})
	if len(items) != 3 {
		t.Fatalf("want 3 posts, got %d", len(items))
	}
}

func TestPlanBreaksArePerPlan(t *testing.T) {
	text := "First thought.\n\nSecond thought."
	at := strings.Index(text, "Second")
	// With a break: two posts, the second made by hand.
	items, _ := Plan(text, Layout{Breaks: []int{at}}, Rules{})
	if len(items) != 2 || items[0].Text != "First thought." || items[1].Text != "Second thought." {
		t.Fatalf("got %+v", items)
	}
	if items[0].Manual || !items[1].Manual {
		t.Errorf("manual flags: %v %v", items[0].Manual, items[1].Manual)
	}
	// Without (an account with breaks: []): one long note.
	items, _ = Plan(text, Layout{Breaks: []int{}}, Rules{})
	if len(items) != 1 || items[0].Text != text {
		t.Fatalf("got %+v", items)
	}
}

func TestPlanAutoBreaksAreNotManual(t *testing.T) {
	text := strings.TrimSpace(strings.Repeat("Sentence number one. ", 10))
	items, _ := Plan(text, Layout{}, Rules{Limit: 50})
	if len(items) < 2 {
		t.Fatalf("want a thread, got %d", len(items))
	}
	for i, it := range items {
		if it.Manual {
			t.Errorf("item %d marked manual", i)
		}
	}
}

func TestPlanIgnoresBadBreaks(t *testing.T) {
	items, _ := Plan("abc def", Layout{Breaks: []int{-1, 0, 7, 99, 4, 4}}, Rules{})
	if len(items) != 2 || items[0].Text != "abc" || items[1].Text != "def" {
		t.Fatalf("got %+v", items)
	}
}

func TestPlanBreakInWhitespaceOnly(t *testing.T) {
	// A break that leaves an empty piece makes no empty post.
	items, _ := Plan("abc   ", Layout{Breaks: []int{4}}, Rules{})
	if len(items) != 1 || items[0].Text != "abc" {
		t.Fatalf("got %+v", items)
	}
}

func TestPlanMediaFollowTheirPosition(t *testing.T) {
	text := "one\n\ntwo\n\nthree"
	b1, b2 := strings.Index(text, "two"), strings.Index(text, "three")
	lay := Layout{Breaks: []int{b1, b2}, MediaAt: []int{0, b2 + 2, b1}}
	items, _ := Plan(text, lay, Rules{ImgCap: 4})
	want := [][]int{{0}, {2}, {1}}
	for i, it := range items {
		if !reflect.DeepEqual(it.Media, want[i]) {
			t.Errorf("post %d media %v, want %v", i, it.Media, want[i])
		}
	}
	// The same positions on a plan with no breaks: all on the one post.
	items, _ = Plan(text, Layout{Breaks: []int{}, MediaAt: lay.MediaAt}, Rules{ImgCap: 4})
	if len(items) != 1 || !reflect.DeepEqual(items[0].Media, []int{0, 1, 2}) {
		t.Fatalf("got %+v", items)
	}
}

func TestPlanMediaSpillAndMediaOnlyPosts(t *testing.T) {
	// Six images, four per post, one short text: a media-only post is added,
	// numbered.
	items, _ := Plan("hi", Layout{MediaAt: make([]int, 6)}, Rules{Limit: 500, ImgCap: 4, Number: true})
	if len(items) != 2 {
		t.Fatalf("want 2 posts, got %d", len(items))
	}
	if items[0].Text != "hi 1/2" || items[1].Text != "2/2" {
		t.Errorf("texts %q %q", items[0].Text, items[1].Text)
	}
	if items[1].From != 2 || items[1].To != 2 {
		t.Errorf("media-only post range [%d,%d)", items[1].From, items[1].To)
	}
	if len(items[0].Media) != 4 || len(items[1].Media) != 2 {
		t.Errorf("media %v %v", items[0].Media, items[1].Media)
	}
}

func TestPlanMastodonCountsURLsAs23(t *testing.T) {
	long := "https://example.com/" + strings.Repeat("x", 600)
	text := "Read this: " + long
	items, _ := Plan(text, Layout{}, Rules{Limit: 500, Counting: mastodonCounting})
	if len(items) != 1 {
		t.Fatalf("a long link fits a Mastodon post; got %d posts", len(items))
	}
	if items[0].Length != len("Read this: ")+23 {
		t.Errorf("length %d", items[0].Length)
	}
	// Counted literally, it doesn't.
	items, _ = Plan(text, Layout{}, Rules{Limit: 500})
	if len(items) < 2 {
		t.Fatal("counted literally, the link should split")
	}
}

func TestPlanThreadsEmojiBytes(t *testing.T) {
	text := strings.Repeat("😀", 130) // 130 graphemes, 520 bytes
	items, _ := Plan(text, Layout{}, Rules{Limit: 500, Counting: destination.Counting{EmojiUTF8Bytes: true}})
	if len(items) != 2 {
		t.Fatalf("want 2 posts, got %d", len(items))
	}
	for _, it := range items {
		if it.Length > 500 {
			t.Errorf("post counts %d", it.Length)
		}
	}
}

func TestPlanMaxLinksProblem(t *testing.T) {
	text := "a https://1.io b https://2.io c https://3.io"
	items, _ := Plan(text, Layout{}, Rules{Limit: 500, MaxLinks: 2})
	if len(items) != 1 || len(items[0].Problems) != 1 {
		t.Fatalf("got %+v", items)
	}
	items, _ = Plan(text, Layout{}, Rules{Limit: 500, MaxLinks: 3})
	if len(items[0].Problems) != 0 {
		t.Fatalf("got %+v", items[0].Problems)
	}
}

func TestPlanLoneLinkNotLeftAlone(t *testing.T) {
	// The link would pack alone into the last post; it takes the sentence
	// before it.
	body := strings.TrimSpace(strings.Repeat("Some words here. ", 5)) // 84
	text := body + " Read it: https://example.com/a-fairly-long-article-path"
	items, _ := Plan(text, Layout{}, Rules{Limit: 90})
	last := items[len(items)-1].Text
	if strings.HasPrefix(last, "https://") {
		t.Fatalf("link alone in the last post: %q", last)
	}
	checkRanges(t, text, items, Rules{Limit: 90})
}

func TestPlanShortTailIsEvenedOut(t *testing.T) {
	// 8 sentences of 20 at 150: greedy gives 7 + 1 (a 20-character tail);
	// the last sentence of the first post moves down.
	s := "Twenty chars here!! "
	text := strings.TrimSpace(strings.Repeat(s, 8))
	items, _ := Plan(text, Layout{}, Rules{Limit: 150})
	if len(items) != 2 {
		t.Fatalf("want 2 posts, got %d", len(items))
	}
	if n := len([]rune(items[1].Text)); n < 150/4 {
		t.Errorf("tail still short: %d (%q)", n, items[1].Text)
	}
	checkRanges(t, text, items, Rules{Limit: 150})
}

func TestPlanEmptyText(t *testing.T) {
	items, _ := Plan("", Layout{MediaAt: []int{0}}, Rules{Limit: 300})
	if len(items) != 1 || items[0].Text != "" || !reflect.DeepEqual(items[0].Media, []int{0}) {
		t.Fatalf("got %+v", items)
	}
}

func TestPlanKeepsInnerWhitespace(t *testing.T) {
	text := "line one\nline  two"
	items, _ := Plan(text, Layout{}, Rules{Limit: 300})
	if items[0].Text != text {
		t.Fatalf("got %q", items[0].Text)
	}
}

func TestNormalizeNewlinesMovesPositions(t *testing.T) {
	breaks := []int{5, 10} // "ab\r\ncd\r\n|ef" … positions after each CRLF
	text := NormalizeNewlines("ab\r\ncd\r\nef\rgh", breaks)
	if text != "ab\ncd\nef\ngh" {
		t.Fatalf("text %q", text)
	}
	if !reflect.DeepEqual(breaks, []int{4, 8}) {
		t.Fatalf("breaks %v", breaks)
	}
}

func TestFromMarkers(t *testing.T) {
	text, breaks, parts := FromMarkers("one\n---\n\n---\ntwo\r\n---\nthree")
	if text != "one\n\ntwo\nthree" {
		t.Fatalf("text %q", text)
	}
	items, _ := Plan(text, Layout{Breaks: breaks}, Rules{})
	var got []string
	for _, it := range items {
		got = append(got, it.Text)
	}
	if !reflect.DeepEqual(got, []string{"one", "two", "three"}) {
		t.Fatalf("posts %q (breaks %v)", got, breaks)
	}
	// Parts skip the empty one, as the old splitter did.
	if len(parts) != 3 {
		t.Fatalf("parts %v", parts)
	}
	ranges := make([][2]int, len(items))
	for i, it := range items {
		ranges[i] = [2]int{it.From, it.To}
	}
	for i, at := range parts {
		if j := postAt(ranges, at); j != i {
			t.Errorf("part %d starts at %d, in post %d", i, at, j)
		}
	}
	// Markers that break nothing (first, last, doubled) are dropped.
	if text, breaks, _ := FromMarkers("---\none\n---\n---\ntwo\n---"); text != "one\ntwo\n" || !reflect.DeepEqual(breaks, []int{4}) {
		t.Fatalf("edges: %q %v", text, breaks)
	}
	if text, breaks, parts := FromMarkers("no markers"); text != "no markers" || breaks != nil || !reflect.DeepEqual(parts, []int{0}) {
		t.Fatalf("got %q %v %v", text, breaks, parts)
	}
}

func TestPlanShortTailOfWordsIsEvenedOut(t *testing.T) {
	text := strings.TrimSpace(strings.Repeat("word ", 101)) // 504 at 500: "word" alone
	items, _ := Plan(text, Layout{}, Rules{Limit: 500})
	if len(items) != 2 {
		t.Fatalf("want 2 posts, got %d", len(items))
	}
	if n := len([]rune(items[1].Text)); n < 125 {
		t.Errorf("tail %d characters", n)
	}
	checkRanges(t, text, items, Rules{Limit: 500})
}

// A media at a hand-made break goes with the post after it, even when
// whitespace follows the break (blank lines around an old "---").
func TestPlanMediaAtBreakBeforeWhitespace(t *testing.T) {
	text, breaks, parts := FromMarkers("para1\n\n---\n\npara2")
	items, _ := Plan(text, Layout{Breaks: breaks, MediaAt: []int{parts[1]}}, Rules{})
	if len(items) != 2 || len(items[0].Media) != 0 || !reflect.DeepEqual(items[1].Media, []int{0}) {
		t.Fatalf("items %+v", items)
	}
}

// Media over a post's cap spill to later posts, never earlier ones.
func TestPlanMediaSpillsForward(t *testing.T) {
	text := "First post.\n\nSecond post."
	at := strings.Index(text, "Second")
	items, _ := Plan(text, Layout{Breaks: []int{at}, MediaAt: []int{at, at, at, at, at, at}}, Rules{ImgCap: 4})
	if len(items) != 3 {
		t.Fatalf("want 3 posts (a media-only one after), got %d: %+v", len(items), items)
	}
	if len(items[0].Media) != 0 || !reflect.DeepEqual(items[1].Media, []int{0, 1, 2, 3}) || !reflect.DeepEqual(items[2].Media, []int{4, 5}) {
		t.Fatalf("media %v %v %v", items[0].Media, items[1].Media, items[2].Media)
	}
}

// A long run with nowhere to break is cut without recounting the post for
// every character (that was O(n × limit): seconds for a 50 000-character
// request with Mastodon's rules).
func TestPlanHardSplitIsFast(t *testing.T) {
	text := strings.Repeat("a", 50000)
	r := Rules{Limit: 500, Number: true, Counting: destination.Counting{URLLength: 23, MentionLocalPartOnly: true}}
	limit := 1500 * time.Millisecond
	if raceEnabled {
		limit *= 10
	}
	start := time.Now()
	items, _ := Plan(text, Layout{}, r)
	if d := time.Since(start); d > limit {
		t.Errorf("took %v", d)
	}
	for i, it := range items {
		if it.Length > 500 {
			t.Fatalf("post %d counts %d", i, it.Length)
		}
	}
	if len(items) != 102 { // 50 000 / (500 - " 102/102")
		t.Errorf("%d posts", len(items))
	}
}
