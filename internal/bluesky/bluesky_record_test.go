package bluesky

import "testing"

func TestBuildPostRecordNoReply(t *testing.T) {
	rec := buildPostRecord(Post{Text: "hi"})
	if rec["$type"] != "app.bsky.feed.post" || rec["text"] != "hi" {
		t.Fatalf("base record wrong: %+v", rec)
	}
	if _, ok := rec["reply"]; ok {
		t.Errorf("no reply expected: %+v", rec["reply"])
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
