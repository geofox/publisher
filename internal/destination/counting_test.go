package destination

import "testing"

func TestCount(t *testing.T) {
	mastodon := Counting{URLLength: 23, MentionLocalPartOnly: true}
	cases := []struct {
		name string
		c    Counting
		in   string
		want int
	}{
		{"ascii graphemes", Counting{}, "hello", 5},
		{"zwj family is one grapheme", Counting{}, "👨‍👩‍👧", 1},
		{"zwj family codepoints", Counting{Unit: UnitCodepoints}, "👨‍👩‍👧", 5},
		{"emoji utf16", Counting{Unit: UnitUTF16}, "😀", 2},
		{"bytes", Counting{Unit: UnitBytes}, "é", 2},
		{"cjk graphemes", Counting{}, "日本語", 3},
		{"url weighted", mastodon, "see https://example.com/a/very/long/path?q=1 now", len("see ") + 23 + len(" now")},
		{"two urls", mastodon, "http://a.io https://b.io/x", 23 + 1 + 23},
		{"remote mention", mastodon, "hi @alice@example.social!", len("hi @alice!")},
		{"local mention untouched", mastodon, "hi @alice", len("hi @alice")},
		{"email is not a mention", mastodon, "mail bob@example.com", len("mail bob@example.com")},
	}
	for _, tc := range cases {
		if got := tc.c.Count(tc.in); got != tc.want {
			t.Errorf("%s: Count(%q) = %d, want %d", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestFits(t *testing.T) {
	c := Counting{}
	if !c.Fits("abc", 0) {
		t.Error("limit 0 must mean unlimited")
	}
	if !c.Fits("abc", 3) || c.Fits("abcd", 3) {
		t.Error("boundary wrong")
	}
}
