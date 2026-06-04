package progress

import (
	"testing"

	"github.com/geofox/publisher/internal/store"
)

func TestFromStorePost(t *testing.T) {
	p := &store.Post{
		ID: "p9", Status: "partial",
		Targets: []store.Target{
			{Platform: "bluesky", Status: "success", RemoteURL: "https://bsky/x"},
			{Platform: "nostr", Status: "partial", Relays: []store.RelayState{
				{URL: "wss://a", Status: "ok"},
				{URL: "wss://b", Status: "failed", Message: "timeout"},
			}},
		},
	}
	s := FromStorePost(p)
	if s.PostID != "p9" || s.Status != "partial" || len(s.Platforms) != 2 {
		t.Fatalf("bad snapshot: %+v", s)
	}
	if s.Platforms[0].URL != "https://bsky/x" {
		t.Fatalf("url not carried: %+v", s.Platforms[0])
	}
	np := s.Platforms[1]
	if len(np.Relays) != 2 || np.Relays[1].Status != RelayFailed || np.Relays[1].Message != "timeout" {
		t.Fatalf("relays wrong: %+v", np.Relays)
	}
}

func TestFromStorePostThreadCounter(t *testing.T) {
	p := &store.Post{ID: "t1", Status: "success", Targets: []store.Target{
		{Platform: "mastodon", Status: "success", Segments: []store.Segment{
			{Status: "success"}, {Status: "success"}, {Status: "success"},
		}},
	}}
	s := FromStorePost(p)
	if s.Platforms[0].Detail != "thread 3/3" {
		t.Fatalf("counter wrong: %q", s.Platforms[0].Detail)
	}
}
