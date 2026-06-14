package verify

import (
	"context"
	"os"
	"testing"
	"time"

	cbornode "github.com/ipfs/go-ipld-cbor"
)

func TestParseBlueskyRef(t *testing.T) {
	cases := []struct {
		raw                              string
		authority, collection, recordKey string
		wantErr                          bool
	}{
		{"at://did:plc:ewvi7nxzyoun6zhxrhs64oiz/app.bsky.feed.post/3kabc", "did:plc:ewvi7nxzyoun6zhxrhs64oiz", "app.bsky.feed.post", "3kabc", false},
		{"https://bsky.app/profile/alice.bsky.social/post/3kabc", "alice.bsky.social", "app.bsky.feed.post", "3kabc", false},
		{"https://bsky.app/profile/did:plc:ewvi7nxzyoun6zhxrhs64oiz/post/3kxyz", "did:plc:ewvi7nxzyoun6zhxrhs64oiz", "app.bsky.feed.post", "3kxyz", false},
		{"https://bsky.app/profile/alice", "", "", "", true},
		{"https://example.com/foo", "", "", "", true},
	}
	for _, c := range cases {
		a, coll, rk, err := parseBlueskyRef(c.raw)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseBlueskyRef(%q) expected error", c.raw)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseBlueskyRef(%q) error: %v", c.raw, err)
			continue
		}
		if a != c.authority || coll != c.collection || rk != c.recordKey {
			t.Errorf("parseBlueskyRef(%q) = (%q,%q,%q), want (%q,%q,%q)",
				c.raw, a, coll, rk, c.authority, c.collection, c.recordKey)
		}
	}
}

// TestBlueskyIntegration verifies a real public post end-to-end. Network-gated:
// set VERIFY_BSKY_URL to a real bsky.app post URL (or at:// URI) to run it.
func TestBlueskyIntegration(t *testing.T) {
	raw := os.Getenv("VERIFY_BSKY_URL")
	if raw == "" {
		t.Skip("set VERIFY_BSKY_URL to run the Bluesky integration test")
	}
	bv := NewBlueskyVerifier("https://plc.directory", 15*time.Second)
	v := bv.Verify(context.Background(), Input{Raw: raw})
	if v.Status != StatusVerified {
		t.Fatalf("status = %s (err=%s) checks=%+v", v.Status, v.Error, v.Checks)
	}
	if v.Assurance != "cryptographic" {
		t.Errorf("assurance = %q", v.Assurance)
	}
	if !hasCheck(v, "commit_signature", "pass") || !hasCheck(v, "mst_inclusion", "pass") {
		t.Errorf("missing crypto checks: %+v", v.Checks)
	}
	if v.Signer == nil || v.Signer.DID == "" {
		t.Errorf("signer not populated: %+v", v.Signer)
	}
}

func TestExtractBskyText(t *testing.T) {
	// 1. A record with ONLY an alt description containing the word "text", but NO top-level text.
	m1 := map[string]any{
		"embed": map[string]any{
			"images": []any{
				map[string]any{
					"alt": "this is some text description",
				},
			},
		},
	}
	b1, err := cbornode.DumpObject(m1)
	if err != nil {
		t.Fatalf("failed to dump cbor: %v", err)
	}
	if got := extractBskyText(b1); got != "" {
		t.Errorf("expected empty text, got %q", got)
	}

	// 2. A record with a top-level text field AND a nested alt description containing "text".
	m2 := map[string]any{
		"text": "hello world",
		"embed": map[string]any{
			"images": []any{
				map[string]any{
					"alt": "some text",
				},
			},
		},
	}
	b2, err := cbornode.DumpObject(m2)
	if err != nil {
		t.Fatalf("failed to dump cbor: %v", err)
	}
	if got := extractBskyText(b2); got != "hello world" {
		t.Errorf("expected %q, got %q", "hello world", got)
	}
}
