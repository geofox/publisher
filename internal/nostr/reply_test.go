package nostr

import "testing"

func TestReplyTagsRootAndParent(t *testing.T) {
	tags := replyTags(&NostrReply{RootID: "rootid", ParentID: "parentid", RelayHint: "wss://r"}, "mypub")
	var eRoot, eReply, pTag []string
	for _, tg := range tags {
		switch {
		case len(tg) >= 4 && tg[0] == "e" && tg[3] == "root":
			eRoot = tg
		case len(tg) >= 4 && tg[0] == "e" && tg[3] == "reply":
			eReply = tg
		case len(tg) >= 2 && tg[0] == "p":
			pTag = tg
		}
	}
	if eRoot == nil || eRoot[1] != "rootid" || eRoot[2] != "wss://r" {
		t.Errorf("root e-tag wrong: %v", eRoot)
	}
	if eReply == nil || eReply[1] != "parentid" || eReply[2] != "wss://r" {
		t.Errorf("reply e-tag wrong: %v", eReply)
	}
	if pTag == nil || pTag[1] != "mypub" {
		t.Errorf("p-tag wrong: %v", pTag)
	}
}

func TestReplyTagsRootEqualsParentEmitsSingleRoot(t *testing.T) {
	// Replying directly to the root: emit a single "root" e-tag, not duplicate
	// root+reply tags with the same id (canonical NIP-10).
	tags := replyTags(&NostrReply{RootID: "x", ParentID: "x", RelayHint: "wss://r"}, "p")
	eCount := 0
	var eTag []string
	for _, tg := range tags {
		if tg[0] == "e" {
			eCount++
			eTag = tg
		}
	}
	if eCount != 1 {
		t.Fatalf("expected a single e-tag when root==parent, got %d: %v", eCount, tags)
	}
	if eTag[1] != "x" || eTag[3] != "root" {
		t.Errorf("e-tag should be the root marker: %v", eTag)
	}
}

func TestReplyTagsNil(t *testing.T) {
	if tags := replyTags(nil, "p"); tags != nil {
		t.Errorf("nil reply → nil tags, got %v", tags)
	}
}

func TestReplyTagsExternalAuthor(t *testing.T) {
	// Replying to an external author: p-tag is the replied-to author, and the
	// root e-tag carries that author as the 5th element (NIP-10).
	r := &NostrReply{RootID: "root1", ParentID: "root1", RelayHint: "wss://r", AuthorPubkey: "extpub"}
	tags := replyTags(r, "ownerpub")
	var eTag, pTag []string
	for _, tg := range tags {
		switch tg[0] {
		case "e":
			eTag = tg
		case "p":
			pTag = tg
		}
	}
	if pTag == nil || pTag[1] != "extpub" {
		t.Fatalf("p-tag should be the external author, got %v", pTag)
	}
	if len(eTag) < 5 || eTag[4] != "extpub" {
		t.Errorf("root e-tag should carry author as 5th element: %v", eTag)
	}
}

func TestReplyTagsFallsBackToOwnerWhenNoAuthor(t *testing.T) {
	// Self-thread (no AuthorPubkey): p-tag stays the owner, e-tags need no 5th elem.
	r := &NostrReply{RootID: "x", ParentID: "x", RelayHint: "wss://r"}
	tags := replyTags(r, "ownerpub")
	for _, tg := range tags {
		if tg[0] == "p" && tg[1] != "ownerpub" {
			t.Errorf("self-thread p-tag should be owner, got %v", tg)
		}
	}
}
