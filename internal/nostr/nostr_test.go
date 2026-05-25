package nostr

import (
	"context"
	"strings"
	"testing"
	"time"

	gonostr "fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"
)

// Guards the NIP-19 nevent encoding used to build the njump viewer link
// (id + relay hints + author) — the API usage Publish relies on.
func TestNeventEncoding(t *testing.T) {
	sk := gonostr.Generate()
	ev := gonostr.Event{
		PubKey:    gonostr.GetPublicKey(sk),
		CreatedAt: gonostr.Timestamp(time.Now().Unix()),
		Kind:      1,
		Content:   "hi",
	}
	if err := ev.Sign(sk); err != nil {
		t.Fatal(err)
	}
	nevent := nip19.EncodeNevent(ev.ID, []string{"wss://relay.geoffrey.one"}, ev.PubKey)
	if !strings.HasPrefix(nevent, "nevent1") {
		t.Fatalf("nevent = %q, want nevent1… prefix", nevent)
	}
	url := "https://njump.me/" + nevent
	if !strings.HasPrefix(url, "https://njump.me/nevent1") {
		t.Errorf("url = %q", url)
	}
}

func TestIsOverlayRelay(t *testing.T) {
	cases := map[string]bool{
		"ws://abc.onion":                     true,
		"wss://relay.example.i2p":            true,
		"ws://abc.onion:8080":                true,
		"wss://relay.geoffrey.one":           false,
		"wss://nos.lol/":                     false,
		"https://not-a-relay.onion.evil.com": false,
	}
	for in, want := range cases {
		if got := IsOverlayRelay(in); got != want {
			t.Errorf("IsOverlayRelay(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestRebroadcastToRelaySkipsOverlay(t *testing.T) {
	p := New(Config{})
	r := p.RebroadcastToRelay(context.Background(), `{}`, "ws://abc.onion")
	if !r.Skipped || r.OK {
		t.Errorf("overlay relay should be skipped, got %+v", r)
	}
}

func TestRebroadcastToRelayBadJSON(t *testing.T) {
	p := New(Config{})
	r := p.RebroadcastToRelay(context.Background(), "not json", "wss://relay.geoffrey.one")
	if r.OK || r.Message == "" {
		t.Errorf("bad event JSON should fail with a message, got %+v", r)
	}
}

func TestRequiresText(t *testing.T) {
	// NIP-18 reposts (kind 6/16) may have empty content; everything else requires text.
	for k, want := range map[int]bool{0: true, 1: true, 6: false, 16: false, 30023: true} {
		if got := requiresText(k); got != want {
			t.Errorf("requiresText(%d) = %v, want %v", k, got, want)
		}
	}
}

func TestReplyTagsCarriesAuthorOnDistinctParent(t *testing.T) {
	// Distinct root and parent: both e-tags carry the author 5th element.
	r := &NostrReply{RootID: "root", ParentID: "parent", RelayHint: "wss://r", AuthorPubkey: "auth"}
	tags := replyTags(r, "owner")
	count := 0
	for _, tg := range tags {
		if tg[0] == "e" {
			if len(tg) < 5 || tg[4] != "auth" {
				t.Errorf("e-tag missing author 5th elem: %v", tg)
			}
			count++
		}
	}
	if count != 2 {
		t.Fatalf("want 2 e-tags for distinct root/parent, got %d", count)
	}
}
