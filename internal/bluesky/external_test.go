package bluesky

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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

// fakePDSForExternal spins up an httptest server with the standard fake-PDS
// handlers. uploadBlob calls are counted via uploadCalls; the handler func
// uploadHandler may be swapped to inject failures. createRecord captures the
// last decoded payload and calls the optional onRecord hook.
func fakePDSForExternal(t *testing.T, uploadHandler func(w http.ResponseWriter, r *http.Request)) (srv *httptest.Server, getRecord func() map[string]any, uploadCalls *int32) {
	t.Helper()
	var rec map[string]any
	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/xrpc/com.atproto.server.createSession", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"did": "did:plc:abc", "handle": "me.example.com", "accessJwt": "AAA", "refreshJwt": "RRR",
		})
	})
	mux.HandleFunc("/xrpc/com.atproto.repo.uploadBlob", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		uploadHandler(w, r)
	})
	mux.HandleFunc("/xrpc/com.atproto.repo.createRecord", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Repo, Collection string
			Record           map[string]any
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		rec = body.Record
		_ = json.NewEncoder(w).Encode(map[string]any{"uri": "at://did:plc:abc/app.bsky.feed.post/3kRKEY", "cid": "bafy"})
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, func() map[string]any { return rec }, &calls
}

// okUploadHandler is a uploadBlob handler that responds with a valid blob ref.
func okUploadHandler(w http.ResponseWriter, _ *http.Request) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"blob": map[string]any{"$type": "blob", "ref": map[string]any{"$link": "bafkThumb"}, "mimeType": "image/png", "size": 1},
	})
}

// tinyPNG returns the bytes of a small valid PNG (8×8 RGBA).
func tinyPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	_ = png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 8, 8)))
	return buf.Bytes()
}

// TestPostExternalEmbedsCard verifies that a Post carrying an ExternalCard
// produces an app.bsky.embed.external record with the expected thumb and
// associatedRefs, and that uploadBlob is called exactly once.
func TestPostExternalEmbedsCard(t *testing.T) {
	srv, getRecord, uploadCalls := fakePDSForExternal(t, okUploadHandler)
	c := New(srv.URL, "me.example.com", "app-pw")

	_, err := c.Post(context.Background(), Post{
		Text: "hi",
		External: &ExternalCard{
			URI: "https://e.com/a", Title: "T", Description: "D",
			Thumb: tinyPNG(t), ThumbMime: "image/png",
			Refs: []ExternalRef{{URI: "at://did:plc:a/site.standard.document/1", CID: "bafy1"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if n := atomic.LoadInt32(uploadCalls); n != 1 {
		t.Errorf("uploadBlob called %d times, want 1", n)
	}

	record := getRecord()
	embed, ok := record["embed"].(map[string]any)
	if !ok {
		t.Fatalf("embed missing or wrong type: %v", record["embed"])
	}
	if embed["$type"] != "app.bsky.embed.external" {
		t.Errorf("embed.$type = %v, want app.bsky.embed.external", embed["$type"])
	}
	ext, ok := embed["external"].(map[string]any)
	if !ok {
		t.Fatalf("embed.external missing or wrong type: %v", embed["external"])
	}
	if ext["thumb"] == nil {
		t.Error("embed.external.thumb must be present when upload succeeds")
	}
	refs, ok := ext["associatedRefs"].([]any)
	if !ok || len(refs) == 0 {
		t.Fatalf("embed.external.associatedRefs missing: %v", ext["associatedRefs"])
	}
	ref0, _ := refs[0].(map[string]any)
	if ref0["cid"] != "bafy1" {
		t.Errorf("associatedRefs[0].cid = %v, want bafy1", ref0["cid"])
	}
}

// TestPostExternalCardWithoutThumbOnUploadFailure verifies that when uploadBlob
// returns a server error the Post still succeeds, the embed.$type is still
// app.bsky.embed.external, and the "thumb" key is absent from the card.
func TestPostExternalCardWithoutThumbOnUploadFailure(t *testing.T) {
	errHandler := func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"InternalServerError"}`, http.StatusInternalServerError)
	}
	srv, getRecord, _ := fakePDSForExternal(t, errHandler)
	c := New(srv.URL, "me.example.com", "app-pw")

	_, err := c.Post(context.Background(), Post{
		Text: "hi",
		External: &ExternalCard{
			URI: "https://e.com/a", Title: "T", Description: "D",
			Thumb: tinyPNG(t), ThumbMime: "image/png",
			Refs: []ExternalRef{{URI: "at://did:plc:a/site.standard.document/1", CID: "bafy1"}},
		},
	})
	if err != nil {
		t.Fatalf("Post must succeed even when thumb upload fails: %v", err)
	}

	record := getRecord()
	embed, ok := record["embed"].(map[string]any)
	if !ok {
		t.Fatalf("embed missing or wrong type: %v", record["embed"])
	}
	if embed["$type"] != "app.bsky.embed.external" {
		t.Errorf("embed.$type = %v, want app.bsky.embed.external", embed["$type"])
	}
	ext, ok := embed["external"].(map[string]any)
	if !ok {
		t.Fatalf("embed.external missing or wrong type: %v", embed["external"])
	}
	if _, present := ext["thumb"]; present {
		t.Error("embed.external.thumb must be absent when upload failed")
	}
}

// TestPostExternalSuppressedByImages verifies that when a Post carries both
// Images and an ExternalCard the image embed wins and the card is silently
// dropped (embed.$type == "app.bsky.embed.images").
func TestPostExternalSuppressedByImages(t *testing.T) {
	srv, getRecord, _ := fakePDSForExternal(t, okUploadHandler)
	c := New(srv.URL, "me.example.com", "app-pw")

	png := tinyPNG(t)
	_, err := c.Post(context.Background(), Post{
		Text:   "hi",
		Images: []Image{{Bytes: png, Mime: "image/png"}},
		External: &ExternalCard{
			URI: "https://e.com/a", Title: "T", Description: "D",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	record := getRecord()
	embed, ok := record["embed"].(map[string]any)
	if !ok {
		t.Fatalf("embed missing or wrong type: %v", record["embed"])
	}
	if embed["$type"] != "app.bsky.embed.images" {
		t.Errorf("embed.$type = %v, want app.bsky.embed.images (card must be dropped when images present)", embed["$type"])
	}
}
