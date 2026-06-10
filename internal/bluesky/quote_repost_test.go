package bluesky

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBuildPostRecordQuoteEmbed(t *testing.T) {
	rec := buildPostRecord(Post{
		Text:  "my take",
		Quote: &QuoteRef{URI: "at://did/app.bsky.feed.post/x", CID: "cidq"},
	})
	embed, ok := rec["embed"].(map[string]any)
	if !ok || embed["$type"] != "app.bsky.embed.record" {
		t.Fatalf("expected embed.record, got %#v", rec["embed"])
	}
	r, _ := embed["record"].(map[string]any)
	if r["uri"] != "at://did/app.bsky.feed.post/x" || r["cid"] != "cidq" {
		t.Errorf("quote strongRef wrong: %#v", r)
	}
	if _, err := json.Marshal(rec); err != nil {
		t.Fatalf("record not JSON-serializable: %v", err)
	}
}

func TestRepostRecordShape(t *testing.T) {
	rec := repostRecord("at://did/app.bsky.feed.post/x", "cidr")
	if rec["$type"] != "app.bsky.feed.repost" {
		t.Fatalf("wrong type: %#v", rec)
	}
	subj, _ := rec["subject"].(map[string]any)
	if subj["uri"] != "at://did/app.bsky.feed.post/x" || subj["cid"] != "cidr" {
		t.Errorf("repost subject wrong: %#v", subj)
	}
	if _, ok := rec["createdAt"]; !ok {
		t.Error("repost record needs createdAt")
	}
}

func TestGetPostReplyRoot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/xrpc/com.atproto.server.createSession":
			w.Write([]byte(`{"accessJwt":"jwt","did":"did:plc:me","handle":"me.bsky.social"}`))
		case "/xrpc/app.bsky.feed.getPosts":
			w.Write([]byte(`{"posts":[{
				"uri":"at://did:plc:a/app.bsky.feed.post/x","cid":"cidx",
				"author":{"handle":"a.bsky.social","displayName":"A"},
				"record":{"text":"hi","createdAt":"2026-05-25T10:00:00Z","reply":{"root":{"uri":"at://root","cid":"cidroot"}}},
				"viewer":{}
			}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "id", "pw")
	sp, err := c.GetPost(context.Background(), "https://bsky.app/profile/did:plc:a/post/x")
	if err != nil {
		t.Fatal(err)
	}
	if sp.ReplyRootURI != "at://root" || sp.ReplyRootCID != "cidroot" {
		t.Fatalf("reply root not decoded: %+v", sp)
	}
}

func TestGetPostNoReplyRootForTopLevel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/xrpc/com.atproto.server.createSession":
			w.Write([]byte(`{"accessJwt":"jwt","did":"did:plc:me","handle":"me"}`))
		case "/xrpc/app.bsky.feed.getPosts":
			w.Write([]byte(`{"posts":[{"uri":"at://did:plc:a/app.bsky.feed.post/x","cid":"cidx","author":{"handle":"a"},"record":{"text":"hi"},"viewer":{}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "id", "pw")
	sp, _ := c.GetPost(context.Background(), "https://bsky.app/profile/did:plc:a/post/x")
	if sp.ReplyRootURI != "" || sp.ReplyRootCID != "" {
		t.Fatalf("top-level post should have empty reply root: %+v", sp)
	}
}

// TestRepostRemoteURLPointsAtSubject guards the repost-link regression: the
// RemoteURL must address the ORIGINAL post (the subject) rather than gluing the
// reposter's handle to the repost record's rkey under /post/.
func TestRepostRemoteURLPointsAtSubject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/xrpc/com.atproto.server.createSession":
			w.Write([]byte(`{"accessJwt":"jwt","did":"did:plc:me","handle":"me.bsky.social"}`))
		case "/xrpc/com.atproto.repo.createRecord":
			// The created repost record gets its own rkey in the repost collection.
			w.Write([]byte(`{"uri":"at://did:plc:me/app.bsky.feed.repost/rp123","cid":"cidrepost"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "id", "pw")
	res, err := c.Repost(context.Background(), "at://did:plc:author/app.bsky.feed.post/orig456", "cidsubj")
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://bsky.app/profile/did:plc:author/post/orig456"; res.RemoteURL != want {
		t.Errorf("RemoteURL = %q, want %q", res.RemoteURL, want)
	}
	// RemoteID stays the repost record's URI (needed for later deletion).
	if want := "at://did:plc:me/app.bsky.feed.repost/rp123"; res.RemoteID != want {
		t.Errorf("RemoteID = %q, want %q", res.RemoteID, want)
	}
}

func TestAuthorityOf(t *testing.T) {
	cases := map[string]string{
		"at://did:plc:abc/app.bsky.feed.post/3k":      "did:plc:abc",
		"at://alice.bsky.social/app.bsky.feed.post/x": "alice.bsky.social",
		"did:plc:naked": "did:plc:naked",
	}
	for in, want := range cases {
		if got := authorityOf(in); got != want {
			t.Errorf("authorityOf(%q) = %q, want %q", in, got, want)
		}
	}
}
