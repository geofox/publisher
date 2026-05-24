package bluesky

import "testing"

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
