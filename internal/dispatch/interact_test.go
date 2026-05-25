package dispatch

import (
	"context"
	"strings"
	"testing"

	gonostr "fiatjaf.com/nostr"
	pubnostr "github.com/geofox/publisher/internal/nostr"
)

// fakeNostrActor records the ReplyRef forwarded to PublishText.
type fakeNostrActor struct {
	lastReply *ReplyRef
}

func (f *fakeNostrActor) PublishText(_ context.Context, _ string, _ *int, _ []gonostr.Tag, replyTo *ReplyRef) (TargetResult, error) {
	f.lastReply = replyTo
	return TargetResult{Platform: "nostr", Status: "success", RemoteID: "ev1"}, nil
}
func (f *fakeNostrActor) RebroadcastToRelay(context.Context, string, string) (bool, string) { return true, "" }
func (f *fakeNostrActor) Repost(context.Context, string, string, int, string) (TargetResult, error) {
	return TargetResult{Platform: "nostr", Status: "success"}, nil
}
func (f *fakeNostrActor) Quote(context.Context, string, string, string, string) (TargetResult, error) {
	return TargetResult{Platform: "nostr", Status: "success"}, nil
}

func TestReplyRefCarriesAuthorToNostr(t *testing.T) {
	f := &fakeNostrActor{}
	d := &Dispatcher{Nostr: f}
	ref := &ReplyRef{RootID: "r", ParentID: "r", AuthorPubkey: "extpub"}
	d.runPlatform(context.Background(), "nostr", "hi", Overrides{}, nil, nil, ref)
	if f.lastReply == nil || f.lastReply.AuthorPubkey != "extpub" {
		t.Fatalf("AuthorPubkey not forwarded to nostr poster: %+v", f.lastReply)
	}
}

var _ = pubnostr.NostrReply{} // keep import while B2 builds out

type fakeBskyActor struct {
	reposted [2]string // uri,cid
	quoted   string    // text
	quoteRef [2]string // uri,cid
}

func (f *fakeBskyActor) PostBsky(_ context.Context, text string, _ Overrides, _ []Img, _ *ReplyRef) (TargetResult, error) {
	return TargetResult{Platform: "bluesky", Status: "success", RemoteID: "p1"}, nil
}
func (f *fakeBskyActor) RepostBsky(_ context.Context, uri, cid string) (TargetResult, error) {
	f.reposted = [2]string{uri, cid}
	return TargetResult{Platform: "bluesky", Status: "success", RemoteID: "rp1"}, nil
}
func (f *fakeBskyActor) QuoteBsky(_ context.Context, text string, _ Overrides, _ []Img, uri, cid string) (TargetResult, error) {
	f.quoted, f.quoteRef = text, [2]string{uri, cid}
	return TargetResult{Platform: "bluesky", Status: "success", RemoteID: "q1"}, nil
}

func TestRunActionRepostBsky(t *testing.T) {
	f := &fakeBskyActor{}
	d := &Dispatcher{Bluesky: f}
	r := d.runAction(context.Background(), actionRepost, "bluesky", "", Overrides{}, nil,
		InteractRef{URI: "at://x", CID: "cidx"})
	if r.Status != "success" || f.reposted != [2]string{"at://x", "cidx"} {
		t.Fatalf("repost not wired: %+v / %v", r, f.reposted)
	}
}

func TestRunActionQuoteBsky(t *testing.T) {
	f := &fakeBskyActor{}
	d := &Dispatcher{Bluesky: f}
	r := d.runAction(context.Background(), actionQuote, "bluesky", "my take", Overrides{}, nil,
		InteractRef{URI: "at://x", CID: "cidx"})
	if r.Status != "success" || f.quoted != "my take" || f.quoteRef != [2]string{"at://x", "cidx"} {
		t.Fatalf("quote not wired: %+v / %q / %v", r, f.quoted, f.quoteRef)
	}
}

// strHolder + fakeMastoActor capture link-quote / quote text for assertions.
type strHolder struct{ s string }

func (h strHolder) contains(sub string) bool { return strings.Contains(h.s, sub) }

type fakeMastoActor struct{ lastPostText strHolder }

func (f *fakeMastoActor) PostText(_ context.Context, text string, _ Overrides, _ []Img, _ *ReplyRef) (TargetResult, error) {
	f.lastPostText = strHolder{s: text}
	return TargetResult{Platform: "mastodon", Status: "success", RemoteID: "m1"}, nil
}
func (f *fakeMastoActor) Reblog(context.Context, string) (TargetResult, error) {
	return TargetResult{Platform: "mastodon", Status: "success"}, nil
}
func (f *fakeMastoActor) QuoteStatus(_ context.Context, text, _ string) (TargetResult, error) {
	f.lastPostText = strHolder{s: text}
	return TargetResult{Platform: "mastodon", Status: "success", RemoteID: "mq1"}, nil
}

func TestInteractQuoteFansOut(t *testing.T) {
	bsky := &fakeBskyActor{}
	masto := &fakeMastoActor{}
	d := &Dispatcher{Bluesky: bsky, Mastodon: masto}
	post := d.Interact(context.Background(), InteractSpec{
		Action: actionQuote, SourcePlatform: "bluesky",
		Ref:       InteractRef{URI: "at://x", CID: "cidx"},
		SourceURL: "https://bsky.app/x", SourceAuthor: "@alice",
		Text:      "great point",
		Fanout:    []string{"mastodon"},
	})
	if post.Interaction == nil || post.Interaction.Action != "quote" || post.Interaction.SourceAuthor != "@alice" {
		t.Fatalf("missing/incorrect interaction descriptor: %+v", post.Interaction)
	}
	if len(post.Targets) != 2 {
		t.Fatalf("quote+fanout should make 2 targets, got %d", len(post.Targets))
	}
	if bsky.quoted != "great point" {
		t.Errorf("bluesky native quote text wrong: %q", bsky.quoted)
	}
	if !masto.lastPostText.contains("https://bsky.app/x") {
		t.Errorf("mastodon link-quote should include the source URL: %q", masto.lastPostText.s)
	}
}

func TestInteractReplySingleTarget(t *testing.T) {
	bsky := &fakeBskyActor{}
	d := &Dispatcher{Bluesky: bsky}
	post := d.Interact(context.Background(), InteractSpec{
		Action: actionReply, SourcePlatform: "bluesky",
		Ref:  InteractRef{URI: "at://x", CID: "cidx", ReplyRootURI: "at://root", ReplyRootCID: "cidroot"},
		Text: "agreed",
	})
	if len(post.Targets) != 1 || post.Targets[0].Platform != "bluesky" {
		t.Fatalf("reply should make 1 source-platform target: %+v", post.Targets)
	}
	if post.Interaction.Action != "reply" {
		t.Errorf("interaction action wrong: %+v", post.Interaction)
	}
}

func TestInteractRepostSingleTarget(t *testing.T) {
	bsky := &fakeBskyActor{}
	d := &Dispatcher{Bluesky: bsky}
	post := d.Interact(context.Background(), InteractSpec{
		Action: actionRepost, SourcePlatform: "bluesky",
		Ref: InteractRef{URI: "at://x", CID: "cidx"},
	})
	if len(post.Targets) != 1 || post.Targets[0].Status != "success" {
		t.Fatalf("repost target wrong: %+v", post.Targets)
	}
	if bsky.reposted != [2]string{"at://x", "cidx"} {
		t.Errorf("repost not performed: %v", bsky.reposted)
	}
}
