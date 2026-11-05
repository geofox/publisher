package bluesky

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/geofox/publisher/internal/destination"
)

func TestLocateOwnPost(t *testing.T) {
	posts := `{"posts":[{"uri":"at://did:plc:me/app.bsky.feed.post/x","cid":"cidx","author":{"handle":"me.bsky.social"},"record":{"text":"hi"},"viewer":{}}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/xrpc/com.atproto.server.createSession":
			w.Write([]byte(`{"accessJwt":"jwt","did":"did:plc:me","handle":"me.bsky.social"}`))
		case "/xrpc/com.atproto.identity.resolveHandle":
			w.Write([]byte(`{"did":"did:plc:` + r.URL.Query().Get("handle")[:2] + `"}`))
		case "/xrpc/app.bsky.feed.getPosts":
			w.Write([]byte(posts))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	a := &Account{C: New(srv.URL, "id", "pw")}
	ref, err := a.Locate(context.Background(), "https://bsky.app/profile/me.bsky.social/post/x")
	if err != nil {
		t.Fatal(err)
	}
	if ref.ID != "at://did:plc:me/app.bsky.feed.post/x" || cidOf(ref) != "cidx" ||
		ref.URL != "https://bsky.app/profile/me.bsky.social/post/x" {
		t.Fatalf("ref = %+v", ref)
	}
	// Someone else's post.
	posts = `{"posts":[{"uri":"at://did:plc:other/app.bsky.feed.post/x","cid":"c","author":{"handle":"o"},"record":{"text":"hi"},"viewer":{}}]}`
	if _, err := a.Locate(context.Background(), "https://bsky.app/profile/did:plc:other/post/x"); !errors.Is(err, destination.ErrNotOwnPost) {
		t.Fatalf("another's post: %v", err)
	}
	// No such post.
	posts = `{"posts":[]}`
	if _, err := a.Locate(context.Background(), "https://bsky.app/profile/did:plc:me/post/y"); !errors.Is(err, destination.ErrPostNotFound) {
		t.Fatalf("missing post: %v", err)
	}
	// Not a post URL.
	_, err = a.Locate(context.Background(), "https://example.com/x")
	if e := destination.Classify(err); e == nil || e.Class != destination.ClassInvalid {
		t.Fatalf("not a post URL: %v", err)
	}
}
