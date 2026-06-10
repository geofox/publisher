package unfurl

import (
	"net/url"
	"strings"
	"testing"
)

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestParseHTMLOpenGraph(t *testing.T) {
	doc := `<html><head>
		<title>Doc Title</title>
		<meta property="og:title" content="OG Title"/>
		<meta property="og:description" content="OG Desc"/>
		<meta property="og:image" content="/img/t.jpg"/>
		<link rel="site.standard.document" href="at://did:plc:abc/site.standard.document/3k"/>
		<link rel="site.standard.publication" href="at://did:plc:abc/site.standard.publication/self"/>
	</head><body><p>hi</p></body></html>`
	m := parseHTML(strings.NewReader(doc), mustURL(t, "https://example.com/post/1"))
	if m.Title != "OG Title" || m.Description != "OG Desc" {
		t.Fatalf("title/desc: %+v", m)
	}
	if m.Image != "https://example.com/img/t.jpg" {
		t.Fatalf("relative og:image not resolved: %q", m.Image)
	}
	if m.DocumentURI != "at://did:plc:abc/site.standard.document/3k" ||
		m.PublicationURI != "at://did:plc:abc/site.standard.publication/self" {
		t.Fatalf("site.standard tags: %+v", m)
	}
}

func TestParseHTMLFallbacks(t *testing.T) {
	doc := `<html><head>
		<title>Plain &amp; Simple</title>
		<meta name="twitter:description" content="TW Desc">
		<meta name="twitter:image" content="https://cdn.example.com/x.png">
	</head><body></body></html>`
	m := parseHTML(strings.NewReader(doc), mustURL(t, "https://example.com/"))
	if m.Title != "Plain & Simple" {
		t.Fatalf("title fallback with entity: %q", m.Title)
	}
	if m.Description != "TW Desc" || m.Image != "https://cdn.example.com/x.png" {
		t.Fatalf("twitter fallbacks: %+v", m)
	}
}

func TestParseHTMLNoMetadata(t *testing.T) {
	m := parseHTML(strings.NewReader(`<html><head></head><body>x</body></html>`), mustURL(t, "https://e.com/"))
	if m.Title != "" || m.Image != "" {
		t.Fatalf("expected empty meta, got %+v", m)
	}
}
