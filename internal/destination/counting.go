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
		urls := countURL.FindAllString(s, -1)
		n += len(urls) * c.URLLength
		s = countURL.ReplaceAllString(s, "")
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
		return uniseg.GraphemeClusterCount(s)
	}
}

// Fits reports whether s is within limit (0 = unlimited).
func (c Counting) Fits(s string, limit int) bool {
	return limit <= 0 || c.Count(strings.ReplaceAll(s, "\r\n", "\n")) <= limit
}
