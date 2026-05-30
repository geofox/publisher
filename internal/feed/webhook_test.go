package feed

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/store"
)

func eligiblePost(id string) *store.Post {
	return &store.Post{ID: id, MasterText: "hi",
		Targets: []store.Target{{Platform: "nostr", Status: "success", RemoteURL: "https://njump.me/" + id}}}
}

func TestWebhookFiresForEligiblePost(t *testing.T) {
	type got struct {
		auth string
		body map[string]string
	}
	ch := make(chan got, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]string
		_ = json.Unmarshal(b, &m)
		ch <- got{auth: r.Header.Get("Authorization"), body: m}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	wh := NewWebhook(srv.URL, "tok")
	wh.PostPublished(context.Background(), eligiblePost("p1"))

	select {
	case g := <-ch:
		if g.auth != "Bearer tok" {
			t.Errorf("auth = %q, want Bearer tok", g.auth)
		}
		if g.body["event"] != "post.published" || g.body["id"] != "p1" {
			t.Errorf("body = %+v, want event=post.published id=p1", g.body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("webhook did not fire within 2s")
	}
}

func TestWebhookSilentForIneligibleOrUnconfigured(t *testing.T) {
	fired := make(chan struct{}, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fired <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Configured, but the post is a reply → must not fire.
	wh := NewWebhook(srv.URL, "")
	reply := eligiblePost("r1")
	reply.Interaction = &store.Interaction{Action: "reply"}
	wh.PostPublished(context.Background(), reply)

	// Unconfigured URL → must not fire even for an eligible post.
	NewWebhook("", "").PostPublished(context.Background(), eligiblePost("p2"))

	select {
	case <-fired:
		t.Fatal("webhook fired for an ineligible/unconfigured case")
	case <-time.After(300 * time.Millisecond):
		// success: nothing fired
	}
}
