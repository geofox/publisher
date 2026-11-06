package destination

import (
	"regexp"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/rivo/uniseg"
)

// Counting says how a text length is measured. It is declarative so the
// splitter, the preview and the browser can all count the same way, and so an
// out-of-process plug-in can describe its rule without shipping code.
type Counting struct {
	// Unit is "graphemes" (user-perceived characters, Bluesky), "codepoints",
	// "utf16" or "bytes". Empty means graphemes.
	Unit string `json:"unit"`
	// URLLength, when > 0, is the fixed cost of every http(s) URL whatever
	// its length (Mastodon: 23).
	URLLength int `json:"url_length,omitempty"`
	// MentionLocalPartOnly counts a "@user@host" mention as "@user"
	// (Mastodon).
	MentionLocalPartOnly bool `json:"mention_local_part_only,omitempty"`
	// EmojiUTF8Bytes counts an emoji as its UTF-8 length rather than as
	// one character (Threads). Only with the graphemes unit.
	EmojiUTF8Bytes bool `json:"emoji_utf8_bytes,omitempty"`
	// ContentWarningCounts takes the content warning's length off the
	// limit of every post it goes with (Mastodon counts it in).
	ContentWarningCounts bool `json:"content_warning_counts,omitempty"`
}

// Counting units.
const (
	UnitGraphemes  = "graphemes"
	UnitCodepoints = "codepoints"
	UnitUTF16      = "utf16"
	UnitBytes      = "bytes"
)

var (
	countURL     = regexp.MustCompile(`https?://[^\s<>"]+`)
	countMention = regexp.MustCompile(`(^|[^\w@])(@[\w.]+)@[\w.-]+\.[a-z]{2,}`)
)

// Count measures s.
func (c Counting) Count(s string) int {
	n := 0
	if c.URLLength > 0 {
		// A URL costs URLLength; punctuation that ends a sentence after it
		// isn't part of it and counts as text (as Mastodon's extractor).
		s = countURL.ReplaceAllStringFunc(s, func(u string) string {
			n += c.URLLength
			return u[len(trimURL(u)):]
		})
	}
	if c.MentionLocalPartOnly {
		s = countMention.ReplaceAllString(s, "$1$2")
	}
	return n + c.unitLen(s)
}

func (c Counting) unitLen(s string) int {
	switch c.Unit {
	case UnitCodepoints:
		return utf8.RuneCountInString(s)
	case UnitUTF16:
		return len(utf16.Encode([]rune(s)))
	case UnitBytes:
		return len(s)
	default:
		if !c.EmojiUTF8Bytes {
			return uniseg.GraphemeClusterCount(s)
		}
		n := 0
		g := uniseg.NewGraphemes(s)
		for g.Next() {
			if cl := g.Str(); isEmoji(cl) {
				n += len(cl)
			} else {
				n++
			}
		}
		return n
	}
}

// isEmoji reports whether a grapheme cluster is an emoji: it holds a
// pictograph, a regional indicator (flags), the emoji presentation selector
// or a keycap. The web app's counter (common.js) uses the same ranges.
func isEmoji(cluster string) bool {
	for _, r := range cluster {
		switch {
		case r == 0xFE0F, r == 0x20E3:
			return true
		case r >= 0x1F000 && r <= 0x1FAFF, // pictographs, emoticons, flags' regional indicators, symbols
			r >= 0x2600 && r <= 0x27BF, // miscellaneous symbols, dingbats
			r >= 0x2300 && r <= 0x23FF, // technical (⌚, ⏰)
			r >= 0x2B00 && r <= 0x2BFF: // arrows, ⭐
			return true
		}
	}
	return false
}

// trimURL takes trailing sentence punctuation off a matched URL, and a
// closing parenthesis it doesn't open itself.
func trimURL(u string) string {
	for len(u) > 0 {
		last := u[len(u)-1]
		switch {
		case strings.IndexByte(".,:;!?'\"", last) >= 0:
			u = u[:len(u)-1]
		case last == ')' && strings.Count(u, "(") < strings.Count(u, ")"):
			u = u[:len(u)-1]
		default:
			return u
		}
	}
	return u
}

// CountURLs is the number of distinct http(s) URLs in s, as a platform that
// caps links per post counts them.
func CountURLs(s string) int {
	seen := map[string]bool{}
	for _, u := range countURL.FindAllString(s, -1) {
		seen[u] = true
	}
	return len(seen)
}

// Fits reports whether s is within limit (0 = unlimited).
func (c Counting) Fits(s string, limit int) bool {
	return limit <= 0 || c.Count(strings.ReplaceAll(s, "\r\n", "\n")) <= limit
}
