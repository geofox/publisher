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
