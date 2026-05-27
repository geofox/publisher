package bluesky

import (
	"strings"
	"testing"
)

func TestBuildPostRecordNoReply(t *testing.T) {
	rec := buildPostRecord(Post{Text: "hi"})
	if rec["$type"] != "app.bsky.feed.post" || rec["text"] != "hi" {
		t.Fatalf("base record wrong: %+v", rec)
	}
	if _, ok := rec["reply"]; ok {
		t.Errorf("no reply expected: %+v", rec["reply"])
	}
}

// createdAt must include sub-second precision: Bluesky's getAuthorFeed dedupes
// posts sharing the same (author, createdAt). With second-precision timestamps,
// rapidly-published chain segments collide and the head can vanish from the
// "Posts" tab — only the highest-rkey record per second survives the dedup.
func TestBuildPostRecordCreatedAtHasSubSecondPrecision(t *testing.T) {
	rec := buildPostRecord(Post{Text: "hi"})
	ts, _ := rec["createdAt"].(string)
	if !strings.Contains(ts, ".") {
		t.Errorf("createdAt missing fractional seconds: %q", ts)
	}
}

func TestBuildPostRecordWithReply(t *testing.T) {
	rec := buildPostRecord(Post{Text: "second", Reply: &ReplyRef{
		RootURI: "at://root", RootCID: "cidR", ParentURI: "at://par", ParentCID: "cidP",
	}})
	reply, ok := rec["reply"].(map[string]any)
	if !ok {
		t.Fatalf("reply missing/!map: %+v", rec["reply"])
	}
	root := reply["root"].(map[string]any)
	parent := reply["parent"].(map[string]any)
	if root["uri"] != "at://root" || root["cid"] != "cidR" {
		t.Errorf("root wrong: %+v", root)
	}
	if parent["uri"] != "at://par" || parent["cid"] != "cidP" {
		t.Errorf("parent wrong: %+v", parent)
	}
}
