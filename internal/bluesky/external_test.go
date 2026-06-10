package bluesky

import (
	"encoding/json"
	"testing"
)

func TestExternalEmbedShape(t *testing.T) {
	c := ExternalCard{
		URI: "https://example.com/a", Title: "T", Description: "D",
		Refs: []ExternalRef{
			{URI: "at://did:plc:abc/site.standard.document/3k", CID: "bafy1"},
			{URI: "at://did:plc:abc/site.standard.publication/self", CID: "bafy2"},
		},
	}
	thumb := json.RawMessage(`{"$type":"blob","ref":{"$link":"bafkthumb"},"mimeType":"image/jpeg","size":9}`)
	got := externalEmbed(c, thumb)
	if got["$type"] != "app.bsky.embed.external" {
		t.Fatalf("$type: %v", got["$type"])
	}
	ext := got["external"].(map[string]any)
	if ext["uri"] != c.URI || ext["title"] != "T" || ext["description"] != "D" {
		t.Fatalf("external: %+v", ext)
	}
	if _, ok := ext["thumb"]; !ok {
		t.Fatal("thumb missing")
	}
	refs := ext["associatedRefs"].([]map[string]any)
	if len(refs) != 2 || refs[0]["uri"] != c.Refs[0].URI || refs[0]["cid"] != "bafy1" {
		t.Fatalf("associatedRefs: %+v", refs)
	}
}

func TestExternalEmbedMinimal(t *testing.T) {
	got := externalEmbed(ExternalCard{URI: "https://e.com", Title: "T"}, nil)
	ext := got["external"].(map[string]any)
	if _, ok := ext["thumb"]; ok {
		t.Fatal("nil thumb must be omitted")
	}
	if _, ok := ext["associatedRefs"]; ok {
		t.Fatal("empty refs must be omitted")
	}
	if ext["description"] != "" {
		t.Fatalf("description must be present (lexicon-required), got %v", ext["description"])
	}
}
