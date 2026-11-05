package dispatch

import (
	"context"
	"strings"
	"testing"
	"time"

	gonostr "fiatjaf.com/nostr"
	pubnostr "github.com/geofox/publisher/internal/nostr"
	"github.com/geofox/publisher/internal/store"
)

// fakeNostrActor records the ReplyRef forwarded to PublishText.
type fakeNostrActor struct {
	lastReply *ReplyRef
	gotImetas []gonostr.Tag
}

func (f *fakeNostrActor) PublishText(_ context.Context, _ string, _ *int, _ []gonostr.Tag, replyTo *ReplyRef) (legacyResult, error) {
	f.lastReply = replyTo
	return legacyResult{Platform: "nostr", Status: "success", RemoteID: "ev1"}, nil
}
func (f *fakeNostrActor) RebroadcastToRelay(context.Context, string, string) (bool, string) {
	return true, ""
}
func (f *fakeNostrActor) Repost(context.Context, string, string, int, string) (legacyResult, error) {
	return legacyResult{Platform: "nostr", Status: "success"}, nil
}
func (f *fakeNostrActor) Quote(_ context.Context, _ string, _ string, _ string, _ string, imetas []gonostr.Tag) (legacyResult, error) {
	f.gotImetas = imetas
	return legacyResult{Platform: "nostr", Status: "success", RemoteID: "nq"}, nil
}

func TestReplyRefCarriesAuthorToNostr(t *testing.T) {
	f := &fakeNostrActor{}
	d := &Dispatcher{Store: openDispatchStore(t), Accounts: legacyAccounts(legacy{Nostr: f})}
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

func (f *fakeBskyActor) PostBsky(_ context.Context, text string, _ Overrides, _ []Img, _ *ReplyRef) (legacyResult, error) {
	return legacyResult{Platform: "bluesky", Status: "success", RemoteID: "p1"}, nil
}
func (f *fakeBskyActor) RepostBsky(_ context.Context, uri, cid string) (legacyResult, error) {
	f.reposted = [2]string{uri, cid}
	return legacyResult{Platform: "bluesky", Status: "success", RemoteID: "rp1"}, nil
}
func (f *fakeBskyActor) QuoteBsky(_ context.Context, text string, _ Overrides, _ []Img, uri, cid string) (legacyResult, error) {
	f.quoted, f.quoteRef = text, [2]string{uri, cid}
	return legacyResult{Platform: "bluesky", Status: "success", RemoteID: "q1"}, nil
}

func TestRunActionRepostBsky(t *testing.T) {
	f := &fakeBskyActor{}
	d := &Dispatcher{Store: openDispatchStore(t), Accounts: legacyAccounts(legacy{Bluesky: f})}
	r := d.runAction(context.Background(), actionRepost, "bluesky", "", Overrides{}, nil, nil,
		InteractRef{URI: "at://x", CID: "cidx"})
	if r.Status != "success" || f.reposted != [2]string{"at://x", "cidx"} {
		t.Fatalf("repost not wired: %+v / %v", r, f.reposted)
	}
}

func TestRunActionQuoteBsky(t *testing.T) {
	f := &fakeBskyActor{}
	d := &Dispatcher{Store: openDispatchStore(t), Accounts: legacyAccounts(legacy{Bluesky: f})}
	r := d.runAction(context.Background(), actionQuote, "bluesky", "my take", Overrides{}, nil, nil,
		InteractRef{URI: "at://x", CID: "cidx"})
	if r.Status != "success" || f.quoted != "my take" || f.quoteRef != [2]string{"at://x", "cidx"} {
		t.Fatalf("quote not wired: %+v / %q / %v", r, f.quoted, f.quoteRef)
	}
}

// strHolder + fakeMastoActor capture link-quote / quote text for assertions.
type strHolder struct{ s string }

func (h strHolder) contains(sub string) bool { return strings.Contains(h.s, sub) }

type fakeMastoActor struct {
	lastPostText strHolder
	quoteImgs    []Img
}

func (f *fakeMastoActor) PostText(_ context.Context, text string, _ Overrides, _ []Img, _ *ReplyRef) (legacyResult, error) {
	f.lastPostText = strHolder{s: text}
	return legacyResult{Platform: "mastodon", Status: "success", RemoteID: "m1"}, nil
}
func (f *fakeMastoActor) Reblog(context.Context, string) (legacyResult, error) {
	return legacyResult{Platform: "mastodon", Status: "success"}, nil
}
func (f *fakeMastoActor) QuoteStatus(_ context.Context, text, _ string, imgs []Img) (legacyResult, error) {
	f.lastPostText = strHolder{s: text}
	f.quoteImgs = imgs
	return legacyResult{Platform: "mastodon", Status: "success", RemoteID: "mq1"}, nil
}

func TestInteractQuoteFansOut(t *testing.T) {
	bsky := &fakeBskyActor{}
	masto := &fakeMastoActor{}
	d := &Dispatcher{Store: openDispatchStore(t), Accounts: legacyAccounts(legacy{Bluesky: bsky, Mastodon: masto})}
	post := d.Interact(context.Background(), InteractSpec{
		Action: actionQuote, SourcePlatform: "bluesky",
		Ref:       InteractRef{URI: "at://x", CID: "cidx"},
		SourceURL: "https://bsky.app/x", SourceAuthor: "@alice",
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
	d := &Dispatcher{Store: openDispatchStore(t), Accounts: legacyAccounts(legacy{Bluesky: bsky})}
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
	d := &Dispatcher{Store: openDispatchStore(t), Accounts: legacyAccounts(legacy{Bluesky: bsky})}
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
	d := &Dispatcher{Store: openDispatchStore(t), Accounts: legacyAccounts(legacy{Bluesky: f})}
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
	d := &Dispatcher{Store: openDispatchStore(t), Accounts: legacyAccounts(legacy{Bluesky: bsky, Mastodon: masto})}
	post := d.Interact(context.Background(), InteractSpec{
		Action: actionQuote, SourcePlatform: "bluesky",
		Ref:       InteractRef{URI: "at://src", CID: "csrc"},
		SourceURL: "https://bsky/9", SourceAuthor: "@bird",
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
	d := &Dispatcher{Store: openDispatchStore(t), Accounts: legacyAccounts(legacy{Bluesky: f})}
	post := d.Interact(context.Background(), InteractSpec{
		Action: actionRepost, SourcePlatform: "bluesky", Ref: InteractRef{URI: "at://src", CID: "csrc"},
	})
	if len(post.Targets) != 1 || post.Targets[0].Status != "success" {
		t.Fatalf("repost target wrong: %+v", post.Targets)
	}
}

func TestRunChainReplyHead(t *testing.T) {
	f := &fakeBsky{failAt: -1}
	d := &Dispatcher{Store: openDispatchStore(t), Accounts: legacyAccounts(legacy{Bluesky: f})}
	head := &headSpec{reply: &InteractRef{URI: "at://src", CID: "csrc"}}
	out := d.runChain(context.Background(), "bluesky", "aaa\n---\nbbb", Overrides{}, nil, false, nil, head, "t")
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
	d := &Dispatcher{Store: openDispatchStore(t), Accounts: legacyAccounts(legacy{Bluesky: f})}
	out := d.runChain(context.Background(), "bluesky", "solo", Overrides{}, nil, false, nil, nil, "t")
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

func TestReproductionMediaCaps(t *testing.T) {
	spec := InteractSpec{SourceMediaRecords: []store.Media{{BlossomURL: "s1"}, {BlossomURL: "s2"}, {BlossomURL: "s3"}}}
	out := reproductionMedia(2, spec, 4)
	if len(out) != 4 || out[0].attach != nil || out[1].post != 1 || out[2].attach.BlossomURL != "s1" || out[3].attach.BlossomURL != "s2" ||
		out[3].attach.Ordinal != 1 {
		t.Fatalf("cap should keep user media first then fill from source up to max: %+v", out)
	}
	// max<=0 → no cap (nostr)
	if all := reproductionMedia(2, spec, 0); len(all) != 5 {
		t.Errorf("max<=0 means no cap, got %d", len(all))
	}
}

func TestMediaMaxFromAccountCaps(t *testing.T) {
	d := &Dispatcher{Store: openDispatchStore(t), Accounts: legacyAccounts(legacy{Bluesky: &fakeBsky{}, Mastodon: &fakeMastoChain{}, Threads: fakeThreads{}, Nostr: fakeNostr{}})}
	for p, want := range map[string]int{"bluesky": 10, "mastodon": 4, "threads": 10, "nostr": 0} {
		if got := d.mediaMax(context.Background(), p); got != want {
			t.Errorf("mediaMax(%q)=%d want %d", p, got, want)
		}
	}
}

func TestInteractHonorsPerPlatformTextOverride(t *testing.T) {
	f := &fakeBsky{failAt: -1}
	d := &Dispatcher{Store: openDispatchStore(t), Accounts: legacyAccounts(legacy{Bluesky: f})}
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

func TestInteractMastodonQuoteCarriesMedia(t *testing.T) {
	masto := &fakeMastoActor{}
	d := &Dispatcher{Store: openDispatchStore(t), Accounts: legacyAccounts(legacy{Mastodon: masto})}
	post := d.Interact(context.Background(), InteractSpec{
		Action: actionQuote, SourcePlatform: "mastodon",
		Ref:  InteractRef{LocalID: "42"},
		Text: "look", Images: []Img{{Alt: "x", Bytes: []byte("i"), Mime: "image/png"}},
	})
	if len(masto.quoteImgs) != 1 || masto.quoteImgs[0].Alt != "x" {
		t.Errorf("mastodon native quote should carry the attached media, got %#v", masto.quoteImgs)
	}
	if masto.lastPostText.s != "look" {
		t.Errorf("native quote text should be the commentary only, got %q", masto.lastPostText.s)
	}
	if len(post.Targets) != 1 || post.Targets[0].FinalText != "look" {
		t.Errorf("expected 1 target with commentary-only final text, got %+v", post.Targets)
	}
}

func TestInteractNostrQuoteCarriesImetas(t *testing.T) {
	na := &fakeNostrActor{}
	d := &Dispatcher{Store: openDispatchStore(t), Accounts: legacyAccounts(legacy{Nostr: na})}
	d.Interact(context.Background(), InteractSpec{
		Action: actionQuote, SourcePlatform: "nostr",
		Ref:    InteractRef{EventID: "ev", Author: "auth"},
		Text:   "see this",
		Images: []Img{{BlossomURL: "https://blossom/x.jpg", Mime: "image/jpeg"}},
	})
	if len(na.gotImetas) != 1 {
		t.Fatalf("nostr quote should forward 1 imeta, got %d", len(na.gotImetas))
	}
}

// An enqueued quote is sent by the workers as the same interaction: the
// native quote, and a link-quote carrying the copied source media.
func TestEnqueueInteractionQuote(t *testing.T) {
	bsky := &fakeBskyActor{}
	masto := &fakeMastoActor{}
	d := &Dispatcher{Store: openDispatchStore(t), Accounts: legacyAccounts(legacy{Bluesky: bsky, Mastodon: masto}),
		Fetcher: fakeFetcher{}, PollInterval: 10 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)
	rec, err := d.EnqueueInteraction(context.Background(), InteractSpec{
		Action: actionQuote, SourcePlatform: "bluesky", Source: "api",
		Ref:       InteractRef{URI: "at://x", CID: "cidx"},
		SourceURL: "https://bsky.app/x", SourceAuthor: "@alice",
		SourcePreview:      SourcePreview{Author: "@alice", Text: "the original"},
		SourceMediaRecords: []store.Media{{BlossomURL: "https://blossom/a.jpg", Mime: "image/jpeg", Alt: "a cat"}},
		Text:               "great point",
		Fanout:             []string{"mastodon"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Source != "api" || rec.Status != "sending" {
		t.Fatalf("saved = %+v", rec)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		p, _ := d.Store.GetPost(rec.ID)
		if p.Status == "success" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("post never published: %+v", p)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if bsky.quoted != "great point" {
		t.Errorf("native quote = %q", bsky.quoted)
	}
	if !masto.lastPostText.contains("the original") || !masto.lastPostText.contains("https://bsky.app/x") {
		t.Errorf("link-quote = %q", masto.lastPostText.s)
	}
}
