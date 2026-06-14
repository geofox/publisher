package bluesky

import (
	"regexp"
	"sort"
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

// linkURI returns the facet's #link target, or "" if it carries no link feature.
func (f facet) linkURI() string {
	for _, ft := range f.Features {
		if ft.Type == "app.bsky.richtext.facet#link" && ft.URI != "" {
			return ft.URI
		}
	}
	return ""
}

// expandLinkFacets rebuilds post text with each link facet's shortened display
// span replaced by its full URI. Bluesky's composer shortens a long link's
// VISIBLE text in record.text (e.g. "njump.me/nevent1qqsq…") and keeps the full
// URL only in the richtext facet. A native quote re-embeds the record so the
// facet survives; a fan-out reproduction copies record.text verbatim, so without
// this expansion the copied link is truncated and dead on every other platform.
// Offsets are UTF-8 byte indices (Go strings are byte-indexed). Facets are
// applied left-to-right; out-of-range or overlapping spans are skipped.
func expandLinkFacets(text string, facets []facet) string {
	type repl struct {
		start, end int
		uri        string
	}
	var repls []repl
	for _, f := range facets {
		uri := f.linkURI()
		if uri == "" {
			continue // tags/mentions keep their display text
		}
		s, e := f.Index.ByteStart, f.Index.ByteEnd
		if s < 0 || e > len(text) || s >= e {
			continue // malformed/out-of-range span: leave text as-is
		}
		repls = append(repls, repl{s, e, uri})
	}
	if len(repls) == 0 {
		return text
	}
	sort.Slice(repls, func(i, j int) bool { return repls[i].start < repls[j].start })
	var b strings.Builder
	last := 0
	for _, r := range repls {
		if r.start < last {
			continue // overlaps a span already applied
		}
		b.WriteString(text[last:r.start])
		b.WriteString(r.uri)
		last = r.end
	}
	b.WriteString(text[last:])
	return b.String()
}

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
