package bluesky

import "testing"

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
