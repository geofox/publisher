package unfurl

import "testing"

func TestCardURL(t *testing.T) {
	cases := []struct {
		name     string
		text     string
		wantURL  string
		trailing bool
		ok       bool
	}{
		{"no url", "hello world", "", false, false},
		{"trailing url", "read this https://example.com/a", "https://example.com/a", true, true},
		{"url only", "https://example.com/a", "https://example.com/a", true, true},
		{"mid-text url", "see https://example.com/a for more", "https://example.com/a", false, true},
		{"two urls, none trailing", "https://a.com/1 and https://b.com/2 end", "https://a.com/1", false, true},
		{"two urls, last trailing wins", "https://a.com/1 then https://b.com/2", "https://b.com/2", true, true},
		{"trailing whitespace ok", "link https://example.com/a \n", "https://example.com/a", true, true},
		{"trailing punctuation is not trailing", "link https://example.com/a.", "https://example.com/a", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u, trailing, ok := CardURL(c.text)
			if ok != c.ok || u != c.wantURL || trailing != c.trailing {
				t.Fatalf("CardURL(%q) = (%q, %v, %v), want (%q, %v, %v)",
					c.text, u, trailing, ok, c.wantURL, c.trailing, c.ok)
			}
		})
	}
}

func TestStripTrailing(t *testing.T) {
	cases := []struct{ name, text, url, want string }{
		{"strips url and space", "read this https://x.com/a", "https://x.com/a", "read this"},
		{"strips trailing newline too", "read this\nhttps://x.com/a\n", "https://x.com/a", "read this"},
		{"not a suffix → unchanged", "https://x.com/a then text", "https://x.com/a", "https://x.com/a then text"},
		{"url-only → empty", "https://x.com/a", "https://x.com/a", ""},
		{"strips crlf", "read this https://x.com/a\r\n", "https://x.com/a", "read this"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := StripTrailing(c.text, c.url); got != c.want {
				t.Fatalf("StripTrailing(%q, %q) = %q, want %q", c.text, c.url, got, c.want)
			}
		})
	}
}
