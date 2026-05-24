package bluesky

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPostCreatesRecordWithImageAndFacets(t *testing.T) {
	var record map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/xrpc/com.atproto.server.createSession", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"did": "did:plc:abc", "handle": "me.example.com", "accessJwt": "AAA", "refreshJwt": "RRR",
		})
	})
	mux.HandleFunc("/xrpc/com.atproto.repo.uploadBlob", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer AAA" {
			t.Errorf("uploadBlob auth = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"blob": map[string]any{"$type": "blob", "ref": map[string]any{"$link": "bafkX"}, "mimeType": "image/jpeg", "size": 1},
		})
	})
	mux.HandleFunc("/xrpc/com.atproto.repo.createRecord", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Repo, Collection string
			Record           map[string]any
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		record = body.Record
		_ = json.NewEncoder(w).Encode(map[string]any{"uri": "at://did:plc:abc/app.bsky.feed.post/3kRKEY", "cid": "bafy"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var pbuf bytes.Buffer
	_ = png.Encode(&pbuf, image.NewRGBA(image.Rect(0, 0, 8, 8)))

	c := New(srv.URL, "me.example.com", "app-pw")
	res, err := c.Post(context.Background(), Post{
		Text: "hello #tag", Langs: []string{"en"},
		Images: []Image{{Bytes: pbuf.Bytes(), Mime: "image/png", Alt: "a square"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.RemoteID != "at://did:plc:abc/app.bsky.feed.post/3kRKEY" {
		t.Errorf("RemoteID = %q", res.RemoteID)
	}
	if !strings.HasSuffix(res.RemoteURL, "/profile/me.example.com/post/3kRKEY") {
		t.Errorf("RemoteURL = %q", res.RemoteURL)
	}
	if res.CID != "bafy" {
		t.Errorf("CID = %q, want bafy", res.CID)
	}
	if record["text"] != "hello #tag" {
		t.Errorf("record text = %v", record["text"])
	}
	if record["facets"] == nil {
		t.Errorf("expected facets for #tag")
	}
	embed, ok := record["embed"].(map[string]any)
	if !ok {
		t.Fatalf("embed missing/wrong type: %v", record["embed"])
	}
	if embed["$type"] != "app.bsky.embed.images" {
		t.Errorf("embed type = %v", embed["$type"])
	}
	imgs, ok := embed["images"].([]any)
	if !ok || len(imgs) != 1 {
		t.Fatalf("embed images missing/wrong: %v", embed["images"])
	}
	entry, ok := imgs[0].(map[string]any)
	if !ok {
		t.Fatalf("image entry wrong type: %v", imgs[0])
	}
	if entry["alt"] != "a square" {
		t.Errorf("alt = %v, want \"a square\"", entry["alt"])
	}
	if entry["aspectRatio"] == nil {
		t.Errorf("aspectRatio missing on image entry")
	}
	if entry["image"] == nil {
		t.Errorf("blob image missing on entry")
	}
}

func TestPostRejectsOverlongText(t *testing.T) {
	c := New("http://unused", "id", "pw")
	long := strings.Repeat("a", 301)
	if _, err := c.Post(context.Background(), Post{Text: long}); err == nil {
		t.Errorf("expected error for >300 graphemes")
	}
}

func TestParseReplyGate(t *testing.T) {
	if ParseReplyGate("") != nil {
		t.Error(`"" → nil (anyone can reply)`)
	}
	if g := ParseReplyGate("nobody"); g == nil || g.AllowMention || g.AllowFollowing || g.AllowFollower {
		t.Errorf("nobody → empty gate, got %+v", g)
	}
	g := ParseReplyGate("following, mention")
	if g == nil || !g.AllowFollowing || !g.AllowMention || g.AllowFollower {
		t.Errorf("following,mention → %+v", g)
	}
	if ParseReplyGate("garbage") != nil {
		t.Error("unrecognized → nil (fail open, not locked)")
	}
}

// gateRec captures a createRecord call for assertions.
type gateRec struct {
	collection, rkey string
	record           map[string]any
}

func postWithGates(t *testing.T, p Post) []gateRec {
	t.Helper()
	var recs []gateRec
	mux := http.NewServeMux()
	mux.HandleFunc("/xrpc/com.atproto.server.createSession", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"did": "did:plc:abc", "handle": "me.example.com", "accessJwt": "AAA"})
	})
	mux.HandleFunc("/xrpc/com.atproto.repo.createRecord", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Collection string         `json:"collection"`
			Rkey       string         `json:"rkey"`
			Record     map[string]any `json:"record"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		recs = append(recs, gateRec{body.Collection, body.Rkey, body.Record})
		_ = json.NewEncoder(w).Encode(map[string]any{"uri": "at://did:plc:abc/app.bsky.feed.post/3kRKEY", "cid": "bafy"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := New(srv.URL, "me.example.com", "app-pw")
	if _, err := c.Post(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return recs
}

func TestPostCreatesThreadgateAndPostgate(t *testing.T) {
	recs := postWithGates(t, Post{Text: "hi", ReplyGate: &ReplyGate{AllowFollowing: true}, DisableQuotes: true})
	if len(recs) != 3 {
		t.Fatalf("expected 3 createRecord calls (post+threadgate+postgate), got %d", len(recs))
	}
	if recs[0].collection != "app.bsky.feed.post" {
		t.Errorf("first record = %s, want post", recs[0].collection)
	}

	tg := recs[1]
	if tg.collection != "app.bsky.feed.threadgate" || tg.rkey != "3kRKEY" {
		t.Errorf("threadgate coll/rkey = %s/%s, want app.bsky.feed.threadgate/3kRKEY", tg.collection, tg.rkey)
	}
	if tg.record["post"] != "at://did:plc:abc/app.bsky.feed.post/3kRKEY" {
		t.Errorf("threadgate post = %v", tg.record["post"])
	}
	allow, ok := tg.record["allow"].([]any)
	if !ok || len(allow) != 1 {
		t.Fatalf("allow = %v, want 1 rule", tg.record["allow"])
	}
	if m, _ := allow[0].(map[string]any); m["$type"] != "app.bsky.feed.threadgate#followingRule" {
		t.Errorf("allow rule = %v", allow[0])
	}

	pg := recs[2]
	if pg.collection != "app.bsky.feed.postgate" || pg.rkey != "3kRKEY" {
		t.Errorf("postgate coll/rkey = %s/%s", pg.collection, pg.rkey)
	}
	er, ok := pg.record["embeddingRules"].([]any)
	if !ok || len(er) != 1 {
		t.Fatalf("embeddingRules = %v", pg.record["embeddingRules"])
	}
	if m, _ := er[0].(map[string]any); m["$type"] != "app.bsky.feed.postgate#disableRule" {
		t.Errorf("embeddingRule = %v", er[0])
	}
}

func TestPostNobodyGateWritesEmptyAllow(t *testing.T) {
	recs := postWithGates(t, Post{Text: "hi", ReplyGate: &ReplyGate{}}) // nobody
	if len(recs) != 2 {
		t.Fatalf("expected 2 calls (post+threadgate), got %d", len(recs))
	}
	allow, ok := recs[1].record["allow"].([]any)
	if !ok {
		t.Fatalf("allow missing/wrong type: %v", recs[1].record["allow"])
	}
	if len(allow) != 0 {
		t.Errorf("allow = %v, want empty array (nobody can reply)", allow)
	}
}

func TestPostNoGatesByDefault(t *testing.T) {
	recs := postWithGates(t, Post{Text: "hi"})
	if len(recs) != 1 || recs[0].collection != "app.bsky.feed.post" {
		t.Errorf("default post should write exactly the post record, got %d: %+v", len(recs), recs)
	}
}

func TestPostGateFailureKeepsPostLink(t *testing.T) {
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/xrpc/com.atproto.server.createSession", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"did": "did:plc:abc", "handle": "me.example.com", "accessJwt": "AAA"})
	})
	mux.HandleFunc("/xrpc/com.atproto.repo.createRecord", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 { // the post itself succeeds
			_ = json.NewEncoder(w).Encode(map[string]any{"uri": "at://did:plc:abc/app.bsky.feed.post/3kRKEY", "cid": "bafy"})
			return
		}
		http.Error(w, `{"error":"InternalServerError"}`, http.StatusInternalServerError) // threadgate write fails
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL, "me.example.com", "app-pw")
	res, err := c.Post(context.Background(), Post{Text: "hi", ReplyGate: &ReplyGate{}}) // nobody
	if err == nil {
		t.Fatal("expected gate write error")
	}
	if res.RemoteID != "at://did:plc:abc/app.bsky.feed.post/3kRKEY" {
		t.Errorf("post link must be preserved on gate failure, got %q", res.RemoteID)
	}
	if !strings.Contains(err.Error(), "threadgate") {
		t.Errorf("error should name the failed gate: %v", err)
	}
}
