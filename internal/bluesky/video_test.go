package bluesky

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPostVideoEmbedsVideo verifies that a Post with a Video field uploads the
// blob as video/mp4 and writes an app.bsky.embed.video embed with the correct
// alt and aspectRatio fields.
func TestPostVideoEmbedsVideo(t *testing.T) {
	var (
		uploadedMime string
		record       map[string]any
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/xrpc/com.atproto.server.createSession", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"did": "did:plc:abc", "handle": "me.example.com", "accessJwt": "AAA", "refreshJwt": "RRR",
		})
	})
	mux.HandleFunc("/xrpc/com.atproto.repo.uploadBlob", func(w http.ResponseWriter, r *http.Request) {
		uploadedMime = r.Header.Get("Content-Type")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"blob": map[string]any{"$type": "blob", "ref": map[string]any{"$link": "bafkvid"}, "mimeType": "video/mp4", "size": 9},
		})
	})
	mux.HandleFunc("/xrpc/com.atproto.repo.createRecord", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Record map[string]any }
		_ = json.NewDecoder(r.Body).Decode(&body)
		record = body.Record
		_ = json.NewEncoder(w).Encode(map[string]any{"uri": "at://did:plc:abc/app.bsky.feed.post/vid1", "cid": "bafyv"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL, "me.example.com", "app-pw")
	_, err := c.Post(context.Background(), Post{
		Text:  "watch this",
		Video: &Video{Bytes: []byte("videodata"), Alt: "v", W: 240, H: 320},
	})
	if err != nil {
		t.Fatal(err)
	}

	if uploadedMime != "video/mp4" {
		t.Errorf("uploadBlob Content-Type = %q, want video/mp4", uploadedMime)
	}

	embed, ok := record["embed"].(map[string]any)
	if !ok {
		t.Fatalf("embed missing or wrong type: %v", record["embed"])
	}
	if embed["$type"] != "app.bsky.embed.video" {
		t.Errorf("embed $type = %v, want app.bsky.embed.video", embed["$type"])
	}
	if embed["alt"] != "v" {
		t.Errorf("embed alt = %v, want v", embed["alt"])
	}
	ar, ok := embed["aspectRatio"].(map[string]any)
	if !ok {
		t.Fatalf("aspectRatio missing or wrong type: %v", embed["aspectRatio"])
	}
	// JSON numbers decode as float64 in map[string]any
	if ar["width"] != float64(240) || ar["height"] != float64(320) {
		t.Errorf("aspectRatio = %v, want {width:240, height:320}", ar)
	}
}

// TestPostVideoWithQuoteUsesRecordWithMedia verifies that a Video + Quote post
// wraps the video embed in app.bsky.embed.recordWithMedia.
func TestPostVideoWithQuoteUsesRecordWithMedia(t *testing.T) {
	var record map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/xrpc/com.atproto.server.createSession", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"did": "did:plc:abc", "handle": "me.example.com", "accessJwt": "AAA"})
	})
	mux.HandleFunc("/xrpc/com.atproto.repo.uploadBlob", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"blob": map[string]any{"$type": "blob", "ref": map[string]any{"$link": "bafkvid"}, "mimeType": "video/mp4", "size": 9}})
	})
	mux.HandleFunc("/xrpc/com.atproto.repo.createRecord", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Record map[string]any }
		_ = json.NewDecoder(r.Body).Decode(&body)
		record = body.Record
		_ = json.NewEncoder(w).Encode(map[string]any{"uri": "at://did:plc:abc/app.bsky.feed.post/vid2", "cid": "bafyv2"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL, "me.example.com", "app-pw")
	_, err := c.Post(context.Background(), Post{
		Text:  "my take",
		Video: &Video{Bytes: []byte("videodata"), Alt: "clip"},
		Quote: &QuoteRef{URI: "at://did/app.bsky.feed.post/x", CID: "cidq"},
	})
	if err != nil {
		t.Fatal(err)
	}

	embed, ok := record["embed"].(map[string]any)
	if !ok {
		t.Fatalf("embed missing/wrong type: %v", record["embed"])
	}
	if embed["$type"] != "app.bsky.embed.recordWithMedia" {
		t.Fatalf("expected recordWithMedia, got %v", embed["$type"])
	}
	media, _ := embed["media"].(map[string]any)
	if media["$type"] != "app.bsky.embed.video" {
		t.Errorf("media embed type = %v, want app.bsky.embed.video", media["$type"])
	}
	rec, _ := embed["record"].(map[string]any)
	inner, _ := rec["record"].(map[string]any)
	if inner["uri"] != "at://did/app.bsky.feed.post/x" {
		t.Errorf("quoted strongRef URI wrong: %v", inner)
	}
}
