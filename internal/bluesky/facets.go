package bluesky

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/geofox/publisher/internal/destination"
)

type byteSlice struct {
	ByteStart int `json:"byteStart"`
	ByteEnd   int `json:"byteEnd"`
}

type facetFeature struct {
	Type string `json:"$type"`
	Tag  string `json:"tag,omitempty"`
	URI  string `json:"uri,omitempty"`
	DID  string `json:"did,omitempty"`
}

type facet struct {
	Index    byteSlice      `json:"index"`
	Features []facetFeature `json:"features"`
}

// Tags follow Bluesky's own rich-text parser
// (https://docs.bsky.app/docs/advanced-guides/post-richtext): a '#' at the
// start or after whitespace, then a tag that doesn't start with a digit, up
// to the next whitespace; trailing punctuation is not part of it. Group 1 is
// the facet span (with the '#'), group 2 the tag.
var tagRe = regexp.MustCompile(`(?:^|\s)([#＃]([^\d\s\p{P}][^\s]*))`)

// maxTagLen is the longest tag Bluesky's parser accepts, in characters.
const maxTagLen = 64

// Mentions: an @handle at the start, after whitespace or '('. A handle is a
// domain name, so it has at least one dot and ends in a letter. Group 1 is
// the facet span (with the '@'), group 2 the handle.
var mentionRe = regexp.MustCompile(`(?:^|\s|\()(@([a-zA-Z0-9-]+(?:\.[a-zA-Z0-9-]+)*\.[a-zA-Z][a-zA-Z0-9-]*))`)

// maxMentions caps how many handles one post resolves (each one is a lookup).
const maxMentions = 20

// mentionHandles lists the distinct @handles in text, in order, lower-cased.
func mentionHandles(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range mentionRe.FindAllStringSubmatch(text, -1) {
		h := strings.ToLower(m[2])
		if !seen[h] && len(out) < maxMentions {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

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

// parseFacets scans text and returns richtext facets for inline #hashtags,
// links and @mentions, with UTF-8 byte offsets (Go string indices are already
// UTF-8 bytes). dids maps a lower-cased handle to its DID; a handle missing
// from it (not resolved) stays plain text.
func parseFacets(text string, dids map[string]string) []facet {
	var out []facet

	// Collect link facets first, recording their byte spans.
	type span struct{ start, end int }
	var linkSpans []span
	for _, loc := range urlRe.FindAllStringIndex(text, -1) {
		// The same trimming the length counter uses: trailing punctuation
		// and a ')' the URL doesn't open itself are not part of it.
		uri := destination.TrimURL(text[loc[0]:loc[1]])
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
		start, end := m[2], m[5]
		tag := strings.TrimRightFunc(text[m[4]:end], unicode.IsPunct)
		end = m[4] + len(tag)
		if tag == "" || utf8.RuneCountInString(tag) > maxTagLen || insideLink(start, end) {
			continue
		}
		out = append(out, facet{
			Index:    byteSlice{ByteStart: start, ByteEnd: end},
			Features: []facetFeature{{Type: "app.bsky.richtext.facet#tag", Tag: tag}},
		})
	}

	for _, m := range mentionRe.FindAllStringSubmatchIndex(text, -1) {
		did := dids[strings.ToLower(text[m[4]:m[5]])]
		if did == "" || insideLink(m[2], m[3]) {
			continue
		}
		out = append(out, facet{
			Index:    byteSlice{ByteStart: m[2], ByteEnd: m[3]},
			Features: []facetFeature{{Type: "app.bsky.richtext.facet#mention", DID: did}},
		})
	}
	return out
}
