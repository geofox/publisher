package unfurl

import (
	"io"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// pageMeta is the raw metadata extracted from one HTML document.
type pageMeta struct {
	Title          string // og:title → twitter:title → <title>
	Description    string // og:description → twitter:description
	Image          string // og:image → twitter:image, resolved absolute
	DocumentURI    string // <link rel="site.standard.document" href="at://…">
	PublicationURI string // <link rel="site.standard.publication" href="at://…">
}

// parseHTML tokenizes the document and extracts OpenGraph/twitter/title meta
// plus the site.standard link tags. base resolves relative image URLs. The
// scan stops at <body> — preview metadata lives in <head>, and the crawler
// budget (maxHTMLBytes) shouldn't be spent walking article markup.
func parseHTML(r io.Reader, base *url.URL) pageMeta {
	var m pageMeta
	var ogTitle, twTitle, docTitle, ogDesc, twDesc, ogImage, twImage string
	z := html.NewTokenizer(r)
	inTitle := false
loop:
	for {
		switch z.Next() {
		case html.ErrorToken: // EOF or malformed tail — use what we have
			break loop
		case html.TextToken:
			if inTitle && docTitle == "" {
				// z.Text() already unescapes HTML entities; trim the whitespace
				// pretty-printed HTML adds around the title.
				docTitle = strings.TrimSpace(string(z.Text()))
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			attrs := map[string]string{}
			for hasAttr {
				var k, v []byte
				k, v, hasAttr = z.TagAttr()
				attrs[string(k)] = string(v)
			}
			switch string(name) {
			case "title":
				// Also fires for <svg><title> inside <head>; in practice such
				// pages carry og:title, which takes priority over the fallback.
				inTitle = true
			case "meta":
				key := attrs["property"]
				if key == "" {
					key = attrs["name"]
				}
				val := attrs["content"]
				switch strings.ToLower(key) {
				case "og:title":
					ogTitle = val
				case "twitter:title":
					twTitle = val
				case "og:description":
					ogDesc = val
				case "twitter:description":
					twDesc = val
				case "og:image", "og:image:url":
					ogImage = val
				case "twitter:image":
					twImage = val
				}
			case "link":
				// rel is matched whole, not as a space-separated token set;
				// site.standard tags are emitted without additional rel values.
				switch strings.ToLower(attrs["rel"]) {
				case "site.standard.document":
					m.DocumentURI = attrs["href"]
				case "site.standard.publication":
					m.PublicationURI = attrs["href"]
				}
			case "body":
				break loop
			}
		case html.EndTagToken:
			if name, _ := z.TagName(); string(name) == "title" {
				inTitle = false
			}
		}
	}
	m.Title = firstNonEmptyStr(ogTitle, twTitle, docTitle)
	m.Description = firstNonEmptyStr(ogDesc, twDesc)
	if img := firstNonEmptyStr(ogImage, twImage); img != "" {
		if u, err := url.Parse(img); err == nil {
			m.Image = base.ResolveReference(u).String()
		}
	}
	return m
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
