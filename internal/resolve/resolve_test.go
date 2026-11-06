package resolve

import (
	"testing"
)

func TestDetect(t *testing.T) {
	cases := map[string]string{
		"https://bsky.app/profile/alice.bsky.social/post/3kabc":            "bluesky",
		"https://mastodon.social/@alice/112233445566":                      "mastodon",
		"https://example.town/users/bob/statuses/9":                        "mastodon",
		"https://www.threads.net/@alice/post/Cabc":                         "threads",
		"https://threads.com/@alice/post/Cabc":                             "threads",
		"nostr:nevent1qqsxyz":                                              "nostr",
		"nevent1QQSXYZ":                                                    "nostr",   // bech32 is case-insensitive
		"https://bsky.app:443/profile/a/post/x":                            "bluesky", // host carries a port
		"https://www.threads.net:443/@a/post/x":                            "threads",
		"nevent1qqsxyz":                                                    "nostr",
		"note1qqsxyz":                                                      "nostr",
		"naddr1qqsxyz":                                                     "nostr",
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef": "nostr",
		"not a url or id":                                                  "",
		"  note1qqsxyz\n":                                                  "nostr", // pasted with space around it
	}
	for in, want := range cases {
		if got := Detect(in); got != want {
			t.Errorf("Detect(%q) = %q, want %q", in, got, want)
		}
	}
}
