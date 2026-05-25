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
		Ref:           InteractRef{URI: "at://x", CID: "cidx"},
		SourceURL:     "https://bsky.app/x", SourceAuthor: "@alice",
		SourcePreview: SourcePreview{Author: "@alice", Text: "the original"},
		Text:          "great point",
		Fanout:        []string{"mastodon"},
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
	// Fan-out now reproduces commentary + the original's text + the source URL,
	// not just the URL.
	if !masto.lastPostText.contains("great point") || !masto.lastPostText.contains("the original") ||
		!masto.lastPostText.contains("https://bsky.app/x") {
		t.Errorf("mastodon fan-out should reproduce commentary + original text + url: %q", masto.lastPostText.s)
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

func TestInteractReplyThreads(t *testing.T) {
	f := &fakeBsky{failAt: -1}
	d := &Dispatcher{Bluesky: f}
	post := d.Interact(context.Background(), InteractSpec{
		Action: actionReply, SourcePlatform: "bluesky",
		Ref:  InteractRef{URI: "at://src", CID: "csrc", ReplyRootURI: "at://src", ReplyRootCID: "csrc"},
		Text: "aaa\n---\nbbb",
	})
	if len(post.Targets) != 1 {
		t.Fatalf("reply → 1 target, got %d", len(post.Targets))
	}
	if len(post.Targets[0].Segments) != 2 {
		t.Fatalf("long reply should thread: %+v", post.Targets[0].Segments)
	}
	if f.calls[0].replyTo == nil || f.calls[0].replyTo.ParentID != "at://src" {
		t.Errorf("head must reply to the source: %+v", f.calls[0].replyTo)
	}
	if post.Interaction == nil || post.Interaction.Action != "reply" {
		t.Errorf("interaction descriptor wrong: %+v", post.Interaction)
	}
}

func TestInteractQuoteFanoutReproduces(t *testing.T) {
	bsky := &fakeBsky{failAt: -1}
	masto := &fakeMastoActor{}
	d := &Dispatcher{Bluesky: bsky, Mastodon: masto}
	post := d.Interact(context.Background(), InteractSpec{
		Action: actionQuote, SourcePlatform: "bluesky",
		Ref:           InteractRef{URI: "at://src", CID: "csrc"},
		SourceURL:     "https://bsky/9", SourceAuthor: "@bird",
		SourcePreview: SourcePreview{Author: "@bird", Text: "tweet text"},
		Text:          "look", Fanout: []string{"mastodon"},
	})
	if len(post.Targets) != 2 {
		t.Fatalf("quote+fanout → 2 targets, got %d", len(post.Targets))
	}
	body := masto.lastPostText.s
	if !strings.Contains(body, "look") || !strings.Contains(body, "tweet text") || !strings.Contains(body, "https://bsky/9") {
		t.Errorf("fan-out should reproduce commentary + original text + url: %q", body)
	}
}

func TestInteractRepostUnchanged(t *testing.T) {
	f := &fakeBsky{failAt: -1}
	d := &Dispatcher{Bluesky: f}
	post := d.Interact(context.Background(), InteractSpec{
		Action: actionRepost, SourcePlatform: "bluesky", Ref: InteractRef{URI: "at://src", CID: "csrc"},
	})
	if len(post.Targets) != 1 || post.Targets[0].Status != "success" {
		t.Fatalf("repost target wrong: %+v", post.Targets)
	}
}

func TestRunChainReplyHead(t *testing.T) {
	f := &fakeBsky{failAt: -1}
	d := &Dispatcher{Bluesky: f}
	head := &headSpec{reply: &ReplyRef{RootID: "at://src", RootCID: "csrc", ParentID: "at://src", ParentCID: "csrc"}}
	out := d.runChain(context.Background(), "bluesky", "aaa\n---\nbbb", Overrides{}, nil, nil, false, head)
	if out.Status != "success" || len(out.Segments) != 2 {
		t.Fatalf("want 2-seg success, got %s %+v", out.Status, out.Segments)
	}
	if f.calls[0].replyTo == nil || f.calls[0].replyTo.ParentID != "at://src" {
		t.Errorf("head must reply to source: %+v", f.calls[0].replyTo)
	}
	if f.calls[1].replyTo == nil || f.calls[1].replyTo.ParentID != "at://post0" {
		t.Errorf("seg2 must reply to the head: %+v", f.calls[1].replyTo)
	}
}

func TestRunChainPlainHeadUnchanged(t *testing.T) {
	f := &fakeBsky{failAt: -1}
	d := &Dispatcher{Bluesky: f}
	out := d.runChain(context.Background(), "bluesky", "solo", Overrides{}, nil, nil, false, nil)
	if out.Status != "success" || len(out.Segments) != 0 {
		t.Fatalf("plain single post changed: %s %+v", out.Status, out.Segments)
	}
	if f.calls[0].replyTo != nil {
		t.Errorf("plain head must not reply: %+v", f.calls[0].replyTo)
	}
}

func TestAssembleReproduction(t *testing.T) {
	sp := SourcePreview{Author: "@bird", Text: "the original"}
	got := assembleReproduction("my take", sp, "https://x/9")
	want := "my take\n\n— @bird:\nthe original\n\nhttps://x/9"
	if got != want {
		t.Fatalf("assembleReproduction:\n got %q\nwant %q", got, want)
	}
	if g := assembleReproduction("", sp, "https://x/9"); g != "— @bird:\nthe original\n\nhttps://x/9" {
		t.Errorf("empty commentary wrong: %q", g)
	}
}

func TestCapMedia(t *testing.T) {
	user := []Img{{Alt: "u1"}, {Alt: "u2"}}
	src := []Img{{Alt: "s1"}, {Alt: "s2"}, {Alt: "s3"}}
	out := capMedia(user, src, 4)
	if len(out) != 4 || out[0].Alt != "u1" || out[1].Alt != "u2" || out[2].Alt != "s1" || out[3].Alt != "s2" {
		t.Fatalf("cap should keep user first then fill from source up to max: %+v", out)
	}
	// max<=0 → no cap (nostr)
	if all := capMedia(user, src, 0); len(all) != 5 {
		t.Errorf("max<=0 means no cap, got %d", len(all))
	}
}

func TestMediaMax(t *testing.T) {
	for p, want := range map[string]int{"bluesky": 4, "mastodon": 4, "threads": 4, "nostr": 0} {
		if mediaMax(p) != want {
			t.Errorf("mediaMax(%q)=%d want %d", p, mediaMax(p), want)
		}
	}
}

func TestInteractHonorsPerPlatformTextOverride(t *testing.T) {
	f := &fakeBsky{failAt: -1}
	d := &Dispatcher{Bluesky: f}
	d.Interact(context.Background(), InteractSpec{
		Action: actionReply, SourcePlatform: "bluesky",
		Ref:       InteractRef{URI: "at://s", CID: "cs"},
		Text:      "master text",
		Overrides: map[string]Overrides{"bluesky": {Text: "edited per-platform"}},
	})
	if f.calls[0].text != "edited per-platform" {
		t.Errorf("source chain should use the per-platform override text, got %q", f.calls[0].text)
	}
}

func TestInteractMastodonQuoteWithMediaDegrades(t *testing.T) {
	masto := &fakeMastoActor{}
	d := &Dispatcher{Mastodon: masto}
	post := d.Interact(context.Background(), InteractSpec{
		Action: actionQuote, SourcePlatform: "mastodon",
		Ref: InteractRef{LocalID: "9"}, SourceURL: "https://m/9", SourceAuthor: "@a",
		SourcePreview: SourcePreview{Author: "@a", Text: "tweet text"},
		Text:          "look", Images: []Img{{Alt: "x"}}, // native quote can't carry media → degrade
	})
	if len(post.Targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(post.Targets))
	}
	// Degrade path posts a reproduction (commentary + original text + url) via PostText;
	// a native QuoteStatus would only carry "look".
	body := masto.lastPostText.s
	if !strings.Contains(body, "tweet text") || !strings.Contains(body, "https://m/9") {
		t.Errorf("mastodon quote+media should degrade to a reproduction post: %q", body)
	}
}
