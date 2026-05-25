package resolve

import (
	"context"
	"testing"
	"time"

	bsky "github.com/geofox/publisher/internal/bluesky"
	mast "github.com/geofox/publisher/internal/mastodon"
	pubnostr "github.com/geofox/publisher/internal/nostr"
)

type fakeNostr struct{ ev *pubnostr.SourceEvent }

func (f fakeNostr) ResolveSource(context.Context, string) (*pubnostr.SourceEvent, error) {
	return f.ev, nil
}

func TestNostrAdapterAllowsEverything(t *testing.T) {
	ev := &pubnostr.SourceEvent{
		IDHex: "ab12cd", Author: "f00dbabef00dbabef00dbabef00dbabef00dbabef00dbabef00dbabef00dbabe",
		Kind: 1, Content: "hello", CreatedAt: time.Now(),
	}
	a := NostrAdapter{P: fakeNostr{ev: ev}}
	ref, err := a.ResolveSource(context.Background(), "nevent1x")
	if err != nil {
		t.Fatal(err)
	}
	if !ref.Caps.Reply.Allowed || !ref.Caps.Quote.Allowed || !ref.Caps.Repost.Allowed {
		t.Errorf("nostr caps should all be allowed: %+v", ref.Caps)
	}
	if ref.Preview.WebURL == "" || ref.Ref.EventID != "ab12cd" {
		t.Errorf("ref/web url wrong: %+v", ref)
	}
}

func TestNostrAdapterProtectedRepostReason(t *testing.T) {
	ev := &pubnostr.SourceEvent{IDHex: "ab12cd", Author: "f00d", Kind: 1, Protected: true}
	a := NostrAdapter{P: fakeNostr{ev: ev}}
	ref, _ := a.ResolveSource(context.Background(), "x")
	if ref.Caps.Repost.Reason == "" {
		t.Error("protected event should annotate the repost reason")
	}
}

type fakeBskySource struct{ p *bsky.SourcePost }

func (f fakeBskySource) GetPost(context.Context, string) (*bsky.SourcePost, error) {
	return f.p, nil
}

func TestBlueskyAdapterMapsViewerFlags(t *testing.T) {
	a := BlueskyAdapter{C: fakeBskySource{p: &bsky.SourcePost{
		URI: "at://x", CID: "cid1", AuthorHandle: "alice.bsky.social",
		Text: "hi", EmbeddingDisabled: true, ReplyDisabled: false,
	}}}
	ref, err := a.ResolveSource(context.Background(), "https://bsky.app/...")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Caps.Quote.Allowed || ref.Caps.Quote.Reason == "" {
		t.Errorf("embeddingDisabled should block quote with a reason: %+v", ref.Caps.Quote)
	}
	if !ref.Caps.Reply.Allowed || !ref.Caps.Repost.Allowed {
		t.Errorf("reply/repost should be allowed: %+v", ref.Caps)
	}
	if ref.Ref.URI != "at://x" || ref.Ref.CID != "cid1" {
		t.Errorf("ref not carried: %+v", ref.Ref)
	}
}

func TestBlueskyAdapterCarriesReplyRoot(t *testing.T) {
	a := BlueskyAdapter{C: fakeBskySource{p: &bsky.SourcePost{
		URI: "at://x", CID: "cidx", AuthorHandle: "a",
		ReplyRootURI: "at://root", ReplyRootCID: "cidroot",
	}}}
	ref, err := a.ResolveSource(context.Background(), "https://bsky.app/...")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Ref.ReplyRootURI != "at://root" || ref.Ref.ReplyRootCID != "cidroot" {
		t.Fatalf("reply root not carried into ref: %+v", ref.Ref)
	}
}

type fakeMastSource struct{ st *mast.SourceStatus }

func (f fakeMastSource) ResolveStatus(context.Context, string) (*mast.SourceStatus, error) {
	return f.st, nil
}

func TestMastodonAdapterCaps(t *testing.T) {
	a := MastodonAdapter{C: fakeMastSource{st: &mast.SourceStatus{
		LocalID: "9", AuthorAcct: "a@x", Visibility: "private", QuoteCurrentUser: "denied",
	}}}
	ref, _ := a.ResolveSource(context.Background(), "https://x/@a/9")
	if ref.Caps.Repost.Allowed || ref.Caps.Quote.Allowed {
		t.Errorf("private+denied should block boost and native quote: %+v", ref.Caps)
	}
	if !ref.Caps.Reply.Allowed {
		t.Error("reply should be allowed")
	}

	a2 := MastodonAdapter{C: fakeMastSource{st: &mast.SourceStatus{
		LocalID: "9", Visibility: "public", QuoteCurrentUser: "automatic",
	}}}
	ref2, _ := a2.ResolveSource(context.Background(), "x")
	if !ref2.Caps.Quote.Allowed || !ref2.Caps.Repost.Allowed {
		t.Errorf("public+automatic should allow quote+boost: %+v", ref2.Caps)
	}
}
