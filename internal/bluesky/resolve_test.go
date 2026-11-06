package bluesky

import (
	"errors"
	"testing"

	"github.com/geofox/publisher/internal/destination"
)

// Moved from internal/resolve (BlueskyAdapter).
func TestResolvedFromPostMapsViewerFlags(t *testing.T) {
	rs, err := resolvedFromPost(&SourcePost{
		URI: "at://x", CID: "cid1", AuthorHandle: "alice.bsky.social",
		Text: "hi", EmbeddingDisabled: true, ReplyDisabled: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rs.Actions.Quote.Allowed || rs.Actions.Quote.Reason == "" {
		t.Errorf("embeddingDisabled should block quote with a reason: %+v", rs.Actions.Quote)
	}
	if !rs.Actions.Reply.Allowed || !rs.Actions.Repost.Allowed {
		t.Errorf("reply/repost should be allowed: %+v", rs.Actions)
	}
	if rs.Ref.URI != "at://x" || rs.Ref.CID != "cid1" || rs.Ref.Type != Type {
		t.Errorf("ref not carried: %+v", rs.Ref)
	}
	if rs.Preview.AuthorName != "alice.bsky.social" || rs.Preview.AuthorHandle != "@alice.bsky.social" {
		t.Errorf("author = %q %q", rs.Preview.AuthorName, rs.Preview.AuthorHandle)
	}
}

func TestResolvedFromPostCarriesReplyRoot(t *testing.T) {
	rs, err := resolvedFromPost(&SourcePost{
		URI: "at://x", CID: "cidx", AuthorHandle: "a",
		ReplyRootURI: "at://root", ReplyRootCID: "cidroot",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rs.Ref.ReplyRootURI != "at://root" || rs.Ref.ReplyRootCID != "cidroot" {
		t.Fatalf("reply root not carried into ref: %+v", rs.Ref)
	}
}

func TestResolvedFromPostReplyGateReason(t *testing.T) {
	rs, _ := resolvedFromPost(&SourcePost{ReplyDisabled: true, ThreadgateReason: "only people mentioned can reply"})
	if rs.Actions.Reply.Allowed || rs.Actions.Reply.Reason != "only people mentioned can reply" {
		t.Errorf("reply = %+v", rs.Actions.Reply)
	}
}

func TestResolvedFromPostNotFound(t *testing.T) {
	_, err := resolvedFromPost(&SourcePost{NotFoundOrBlocked: true})
	if !errors.Is(err, destination.ErrSourceNotFound) {
		t.Fatalf("err = %v, want ErrSourceNotFound", err)
	}
}
