package bluesky

import (
	"strings"
	"testing"
)

// link makes a #link facet spanning display within text (byte offsets), pointing
// at the full uri — the shape getPosts returns when Bluesky shortens a long link.
func linkFacet(text, display, uri string) facet {
	i := strings.Index(text, display)
	if i < 0 {
		panic("display not in text: " + display)
	}
	return facet{
		Index:    byteSlice{ByteStart: i, ByteEnd: i + len(display)},
		Features: []facetFeature{{Type: "app.bsky.richtext.facet#link", URI: uri}},
	}
}

func TestExpandLinkFacets(t *testing.T) {
	// The real nevent from the bug report: Bluesky shows a shortened display span
	// while the facet carries the full URL. Reproducing record.text without
	// expanding facets truncates the link on every fan-out platform.
	const full = "https://njump.me/nevent1qqsqqqq8knf94zg0vp2u0d6zkut5l0xujd6s7qkmzr0cpxyqgej3nlqzyzzu7szw34qvqaryvdtamnfnerww3r29auyd52nfd4x420xzfxtdcqcyqqqqqqghfupkj"

	t.Run("expands a shortened link to its full uri", func(t *testing.T) {
		text := "great post njump.me/nevent1qqsq…"
		got := expandLinkFacets(text, []facet{linkFacet(text, "njump.me/nevent1qqsq…", full)})
		want := "great post " + full
		if got != want {
			t.Errorf("got  %q\nwant %q", got, want)
		}
	})

	t.Run("leaves tag facets untouched and expands only links", func(t *testing.T) {
		text := "#nostr see njump.me/x…"
		facets := []facet{
			{Index: byteSlice{ByteStart: 0, ByteEnd: 6}, Features: []facetFeature{{Type: "app.bsky.richtext.facet#tag", Tag: "nostr"}}},
			linkFacet(text, "njump.me/x…", full),
		}
		got := expandLinkFacets(text, facets)
		want := "#nostr see " + full
		if got != want {
			t.Errorf("got  %q\nwant %q", got, want)
		}
	})

	t.Run("expands multiple links preserving order and gaps", func(t *testing.T) {
		text := "a one.com/aaa… b two.com/bbb… c"
		facets := []facet{
			linkFacet(text, "one.com/aaa…", "https://one.com/aaaaaaaaaaaaaaaaaa"),
			linkFacet(text, "two.com/bbb…", "https://two.com/bbbbbbbbbbbbbbbbbb"),
		}
		got := expandLinkFacets(text, facets)
		want := "a https://one.com/aaaaaaaaaaaaaaaaaa b https://two.com/bbbbbbbbbbbbbbbbbb c"
		if got != want {
			t.Errorf("got  %q\nwant %q", got, want)
		}
	})

	t.Run("no facets returns text unchanged", func(t *testing.T) {
		text := "plain text, no links"
		if got := expandLinkFacets(text, nil); got != text {
			t.Errorf("got %q, want unchanged", got)
		}
	})

	t.Run("skips out-of-range facet spans defensively", func(t *testing.T) {
		text := "short"
		facets := []facet{{Index: byteSlice{ByteStart: 2, ByteEnd: 999}, Features: []facetFeature{{Type: "app.bsky.richtext.facet#link", URI: full}}}}
		if got := expandLinkFacets(text, facets); got != text {
			t.Errorf("got %q, want unchanged %q", got, text)
		}
	})
}

func TestParseFacets(t *testing.T) {
	text := "Check out #golang at https://go.dev"
	fs := parseFacets(text)
	if len(fs) != 2 {
		t.Fatalf("want 2 facets, got %d: %+v", len(fs), fs)
	}
	var tag, link *facet
	for i := range fs {
		switch fs[i].Features[0].Type {
		case "app.bsky.richtext.facet#tag":
			tag = &fs[i]
		case "app.bsky.richtext.facet#link":
			link = &fs[i]
		}
	}
	if tag == nil || link == nil {
		t.Fatalf("missing tag or link facet: %+v", fs)
	}
	if tag.Index.ByteStart != 10 || tag.Index.ByteEnd != 17 || tag.Features[0].Tag != "golang" {
		t.Errorf("tag wrong: %+v", *tag)
	}
	if text[link.Index.ByteStart:link.Index.ByteEnd] != "https://go.dev" || link.Features[0].URI != "https://go.dev" {
		t.Errorf("link wrong: %+v", *link)
	}
}

func TestParseFacetsTrimsTrailingPunct(t *testing.T) {
	fs := parseFacets("see https://go.dev.")
	if len(fs) != 1 || fs[0].Features[0].URI != "https://go.dev" {
		t.Errorf("want trimmed url, got %+v", fs)
	}
}

func TestParseFacetsSkipsFragmentInURL(t *testing.T) {
	fs := parseFacets("see https://docs.rs/foo#section and #rust")
	links, tags := 0, 0
	for _, f := range fs {
		switch f.Features[0].Type {
		case "app.bsky.richtext.facet#link":
			links++
		case "app.bsky.richtext.facet#tag":
			tags++
			if f.Features[0].Tag != "rust" {
				t.Errorf("unexpected tag %q (should not pick up #section)", f.Features[0].Tag)
			}
		}
	}
	if links != 1 || tags != 1 {
		t.Errorf("want 1 link + 1 tag, got %d links %d tags: %+v", links, tags, fs)
	}
}
