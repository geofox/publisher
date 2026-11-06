package destination

import (
	"encoding/json"
	"os"
	"testing"
)

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

// TestCountSharedCases runs the cases the web app's counter is checked
// against too, so the two can't drift apart.
func TestCountSharedCases(t *testing.T) {
	raw, err := os.ReadFile("testdata/counting.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Cases []struct {
			Name     string   `json:"name"`
			Counting Counting `json:"counting"`
			Text     string   `json:"text"`
			Want     int      `json:"want"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Cases) < 10 {
		t.Fatalf("only %d cases", len(f.Cases))
	}
	for _, tc := range f.Cases {
		if got := tc.Counting.Count(tc.Text); got != tc.Want {
			t.Errorf("%s: Count(%q) = %d, want %d", tc.Name, tc.Text, got, tc.Want)
		}
	}
}

func TestCountURLs(t *testing.T) {
	if n := CountURLs("a https://x.io b https://x.io c http://y.io/z"); n != 2 {
		t.Errorf("CountURLs = %d, want 2 distinct", n)
	}
}
