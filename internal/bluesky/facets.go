package bluesky

import (
	"regexp"
	"strings"
)

type byteSlice struct {
	ByteStart int `json:"byteStart"`
	ByteEnd   int `json:"byteEnd"`
}

type facetFeature struct {
	Type string `json:"$type"`
	Tag  string `json:"tag,omitempty"`
	URI  string `json:"uri,omitempty"`
}

type facet struct {
	Index    byteSlice      `json:"index"`
	Features []facetFeature `json:"features"`
}

// Tags: a leading '#' followed by letters/numbers/underscore (unicode-aware).
// The facet span includes the '#'; the tag value excludes it.
var tagRe = regexp.MustCompile(`#([\p{L}\p{N}_]+)`)

// Links: http(s) URLs up to the next whitespace.
var urlRe = regexp.MustCompile(`https?://[^\s]+`)

// parseFacets scans text and returns richtext facets for inline #hashtags and
// links, with UTF-8 byte offsets (Go string indices are already UTF-8 bytes).
func parseFacets(text string) []facet {
	var out []facet

	// Collect link facets first, recording their byte spans.
	type span struct{ start, end int }
	var linkSpans []span
	for _, loc := range urlRe.FindAllStringIndex(text, -1) {
		uri := strings.TrimRight(text[loc[0]:loc[1]], ".,!?);:")
		end := loc[0] + len(uri)
		linkSpans = append(linkSpans, span{loc[0], end})
		out = append(out, facet{
			Index:    byteSlice{ByteStart: loc[0], ByteEnd: end},
			Features: []facetFeature{{Type: "app.bsky.richtext.facet#link", URI: uri}},
		})
	}

	insideLink := func(start, end int) bool {
		for _, ls := range linkSpans {
			if start >= ls.start && end <= ls.end {
				return true
			}
		}
		return false
	}

	// Tags, skipping any '#fragment' that lives inside a link span.
	for _, m := range tagRe.FindAllStringSubmatchIndex(text, -1) {
		if insideLink(m[0], m[1]) {
			continue
		}
		out = append(out, facet{
			Index:    byteSlice{ByteStart: m[0], ByteEnd: m[1]},
			Features: []facetFeature{{Type: "app.bsky.richtext.facet#tag", Tag: text[m[2]:m[3]]}},
		})
	}
	return out
}
