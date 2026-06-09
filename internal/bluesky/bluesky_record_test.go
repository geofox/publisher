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

func galleryEntries(n int) []map[string]any {
	out := make([]map[string]any, n)
	for i := range out {
		out[i] = map[string]any{"alt": "a", "image": map[string]any{"$type": "blob"}}
	}
	return out
}

func TestMediaEmbedFourOrFewerKeepsImagesEmbed(t *testing.T) {
	imgs := galleryEntries(4)
	emb := mediaEmbed(imgs)
	if emb["$type"] != "app.bsky.embed.images" {
		t.Fatalf("$type=%v", emb["$type"])
	}
	if got := emb["images"].([]map[string]any); len(got) != 4 {
		t.Fatalf("images len=%d", len(got))
	}
	// Legacy entries must stay untouched (no per-item $type injected).
	if _, ok := imgs[0]["$type"]; ok {
		t.Error("legacy embed entries must not carry $type")
	}
}

// 5+ images use app.bsky.embed.gallery (Bluesky 1.123+). Gallery items are
// union members so each carries $type, and aspectRatio is REQUIRED by the
// lexicon — entries without one get a 1:1 fallback.
func TestMediaEmbedFivePlusUsesGallery(t *testing.T) {
	imgs := galleryEntries(5)
	imgs[0]["aspectRatio"] = map[string]int{"width": 3, "height": 2}
	emb := mediaEmbed(imgs)
	if emb["$type"] != "app.bsky.embed.gallery" {
		t.Fatalf("$type=%v", emb["$type"])
	}
	items := emb["items"].([]map[string]any)
	if len(items) != 5 {
		t.Fatalf("items len=%d", len(items))
	}
	for i, it := range items {
		if it["$type"] != "app.bsky.embed.gallery#image" {
			t.Errorf("item %d $type=%v", i, it["$type"])
		}
		if it["alt"] != "a" || it["image"] == nil {
			t.Errorf("item %d lost fields: %+v", i, it)
		}
		if _, ok := it["aspectRatio"]; !ok {
			t.Errorf("item %d missing required aspectRatio", i)
		}
	}
	ar := items[0]["aspectRatio"].(map[string]int)
	if ar["width"] != 3 || ar["height"] != 2 {
		t.Errorf("known aspectRatio must be preserved: %+v", ar)
	}
	fb := items[1]["aspectRatio"].(map[string]int)
	if fb["width"] != 1 || fb["height"] != 1 {
		t.Errorf("fallback must be 1:1: %+v", fb)
	}
	// The input entries must not be mutated (they're shared with callers).
	if _, ok := imgs[1]["$type"]; ok {
		t.Error("input entries were mutated")
	}
}

func TestMediaEmbedTenImagesAndEmptyAlt(t *testing.T) {
	imgs := galleryEntries(10)
	imgs[3]["alt"] = ""
	emb := mediaEmbed(imgs)
	if emb["$type"] != "app.bsky.embed.gallery" {
		t.Fatalf("$type=%v", emb["$type"])
	}
	items := emb["items"].([]map[string]any)
	if len(items) != 10 {
		t.Fatalf("items len=%d", len(items))
	}
	// alt is REQUIRED by the lexicon but may be empty — it must survive as "".
	if alt, ok := items[3]["alt"]; !ok || alt != "" {
		t.Errorf(`empty alt must be present: %v ok=%v`, alt, ok)
	}
}
