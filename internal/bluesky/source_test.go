package bluesky

import "testing"

func TestExtractImages(t *testing.T) {
	cases := []struct {
		name  string
		embed string
		want  []SourceMedia
	}{
		{
			name: "images view",
			embed: `{"$type":"app.bsky.embed.images#view","images":[
				{"thumb":"https://cdn/t1.jpg","fullsize":"https://cdn/f1.jpg","alt":"one"},
				{"thumb":"https://cdn/t2.jpg","fullsize":"","alt":"two"}]}`,
			want: []SourceMedia{
				{URL: "https://cdn/f1.jpg", Alt: "one"},
				{URL: "https://cdn/t2.jpg", Alt: "two"},
			},
		},
		{
			name: "gallery view",
			embed: `{"$type":"app.bsky.embed.gallery#view","items":[
				{"$type":"app.bsky.embed.gallery#viewImage","thumbnail":"https://cdn/t1.jpg","fullsize":"https://cdn/f1.jpg","alt":"one","aspectRatio":{"width":3,"height":2}},
				{"$type":"app.bsky.embed.gallery#viewImage","thumbnail":"https://cdn/t2.jpg","fullsize":"https://cdn/f2.jpg","alt":"","aspectRatio":{"width":1,"height":1}},
				{"$type":"app.bsky.embed.gallery#viewImage","thumbnail":"https://cdn/t3.jpg","fullsize":"","alt":"three","aspectRatio":{"width":4,"height":3}}]}`,
			want: []SourceMedia{
				{URL: "https://cdn/f1.jpg", Alt: "one"},
				{URL: "https://cdn/f2.jpg", Alt: ""},
				{URL: "https://cdn/t3.jpg", Alt: "three"},
			},
		},
		{name: "empty embed", embed: "", want: nil},
		{name: "no media embed", embed: `{"$type":"app.bsky.embed.external#view","external":{"uri":"https://x"}}`, want: nil},
	}
	for _, c := range cases {
		got := extractImages([]byte(c.embed))
		if len(got) != len(c.want) {
			t.Errorf("%s: got %d media, want %d (%v)", c.name, len(got), len(c.want), got)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s[%d]: got %+v, want %+v", c.name, i, got[i], c.want[i])
			}
		}
	}
}

func TestParsePostURL(t *testing.T) {
	cases := []struct{ url, wantActor, wantRkey string }{
		{"https://bsky.app/profile/alice.bsky.social/post/3kabc", "alice.bsky.social", "3kabc"},
		{"https://bsky.app/profile/did:plc:xyz/post/3kdef", "did:plc:xyz", "3kdef"},
	}
	for _, c := range cases {
		actor, rkey, err := parsePostURL(c.url)
		if err != nil || actor != c.wantActor || rkey != c.wantRkey {
			t.Errorf("parsePostURL(%q) = %q,%q,%v", c.url, actor, rkey, err)
		}
	}
	if _, _, err := parsePostURL("https://bsky.app/profile/alice"); err == nil {
		t.Error("expected error for non-post URL")
	}
}
