// Package unfurl builds Bluesky link cards: it fetches a page's OpenGraph
// metadata and atproto site.standard references so dispatch and the
// thread-preview endpoint share one (cached) source of card truth.
package unfurl

import (
	"regexp"
	"strings"
)

// urlRe and the punctuation trim mirror bluesky/facets.go, so the card URL is
// byte-identical to the faceted link.
var urlRe = regexp.MustCompile(`https?://[^\s]+`)

func trimURL(raw string) string { return strings.TrimRight(raw, ".,!?);:") }

// CardURL picks the URL that should carry the link card: the trailing URL when
// the text ends with one (only whitespace after it), else the first URL.
// ok=false when the text contains no URL.
func CardURL(text string) (url string, trailing bool, ok bool) {
	locs := urlRe.FindAllStringIndex(text, -1)
	if len(locs) == 0 {
		return "", false, false
	}
	last := locs[len(locs)-1]
	lastURL := trimURL(text[last[0]:last[1]])
	if strings.TrimSpace(text[last[0]+len(lastURL):]) == "" {
		return lastURL, true, true
	}
	first := locs[0]
	return trimURL(text[first[0]:first[1]]), false, true
}

// StripTrailing removes a trailing URL (and the whitespace before it) from
// text. Text that doesn't end with the URL is returned unchanged.
func StripTrailing(text, url string) string {
	t := strings.TrimRight(text, " \t\n")
	if !strings.HasSuffix(t, url) {
		return text
	}
	return strings.TrimRight(strings.TrimSuffix(t, url), " \t\n")
}
