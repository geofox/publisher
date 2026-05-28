package feed

import (
	"testing"
	"time"

	"github.com/geofox/publisher/internal/store"
)

func successTarget(platform, url, fields string) store.Target {
	return store.Target{Platform: platform, Status: "success", RemoteURL: url, FieldsJSON: fields}
}

func TestBuildIncludesPublicLinksOnly(t *testing.T) {
	posts := []store.Post{{
		ID:         "p1",
		MasterText: "hello",
		FirstSuccessAt: ptr(time.Date(2026, 5, 20, 9, 0, 0, 0, time.UTC)),
		Targets: []store.Target{
			successTarget("nostr", "https://njump.me/x", ""),
			successTarget("mastodon", "https://m/1", `{"visibility":"unlisted"}`), // dropped
			successTarget("bluesky", "https://bsky/1", ""),
			{Platform: "threads", Status: "failed", RemoteURL: ""}, // dropped: not success
		},
	}}
	out := Build(posts, 20)
	if len(out.Posts) != 1 {
		t.Fatalf("got %d items, want 1", len(out.Posts))
	}
	it := out.Posts[0]
	if it.PublishedAt != *posts[0].FirstSuccessAt {
		t.Errorf("PublishedAt = %v, want first-success time", it.PublishedAt)
	}
	gotPlatforms := map[string]bool{}
	for _, l := range it.Links {
		gotPlatforms[l.Platform] = true
	}
	if len(it.Links) != 2 || !gotPlatforms["nostr"] || !gotPlatforms["bluesky"] {
		t.Errorf("links = %+v, want nostr+bluesky only (unlisted mastodon + failed threads dropped)", it.Links)
	}
}

func TestBuildDropsPostWithNoPublicLinks(t *testing.T) {
	posts := []store.Post{{
		ID: "only-unlisted", MasterText: "secret-ish",
		Targets: []store.Target{successTarget("mastodon", "https://m/9", `{"visibility":"private"}`)},
	}}
	if out := Build(posts, 20); len(out.Posts) != 0 {
		t.Fatalf("got %d items, want 0 (no public link → dropped)", len(out.Posts))
	}
}

func TestBuildExcludesRepliesIncludesQuotesReposts(t *testing.T) {
	mk := func(id, action string) store.Post {
		p := store.Post{ID: id, Targets: []store.Target{successTarget("nostr", "https://njump.me/"+id, "")}}
		if action != "" {
			p.Interaction = &store.Interaction{Action: action, SourcePlatform: "bluesky",
				SourceURL: "https://src/" + id, SourceAuthor: "@a"}
		}
		return p
	}
	out := Build([]store.Post{mk("orig", ""), mk("q", "quote"), mk("rp", "repost"), mk("rep", "reply")}, 20)
	got := map[string]bool{}
	for _, it := range out.Posts {
		got[it.ID] = true
	}
	if !got["orig"] || !got["q"] || !got["rp"] || got["rep"] {
		t.Fatalf("included = %v, want orig+q+rp (reply excluded)", got)
	}
	for _, it := range out.Posts {
		if it.ID == "q" {
			if it.Interaction == nil || it.Interaction.Action != "quote" || it.Interaction.SourceAuthor != "@a" {
				t.Errorf("quote interaction not surfaced: %+v", it.Interaction)
			}
		}
		if it.ID == "orig" && it.Interaction != nil {
			t.Errorf("original post should have no interaction block")
		}
	}
}

func TestBuildMapsMediaAndHonorsLimit(t *testing.T) {
	mkP := func(id string) store.Post {
		return store.Post{ID: id, Targets: []store.Target{successTarget("nostr", "https://njump.me/"+id, "")},
			Media: []store.Media{{BlossomURL: "https://b/" + id, Mime: "image/png", Alt: "alt", Dim: "1x1", Blurhash: "L0"}}}
	}
	out := Build([]store.Post{mkP("a"), mkP("b"), mkP("c")}, 2)
	if len(out.Posts) != 2 {
		t.Fatalf("got %d items, want 2 (limit)", len(out.Posts))
	}
	if len(out.Posts[0].Media) != 1 || out.Posts[0].Media[0].URL != "https://b/a" || out.Posts[0].Media[0].Alt != "alt" {
		t.Errorf("media not mapped: %+v", out.Posts[0].Media)
	}
	if out.Version != 1 {
		t.Errorf("Version = %d, want 1", out.Version)
	}
}

func TestEligible(t *testing.T) {
	public := store.Post{Targets: []store.Target{successTarget("nostr", "https://njump.me/x", "")}}
	reply := store.Post{Interaction: &store.Interaction{Action: "reply"},
		Targets: []store.Target{successTarget("nostr", "https://njump.me/x", "")}}
	unlistedOnly := store.Post{Targets: []store.Target{successTarget("mastodon", "https://m/1", `{"visibility":"unlisted"}`)}}
	if !Eligible(public) {
		t.Error("public post should be eligible")
	}
	if Eligible(reply) {
		t.Error("reply should not be eligible")
	}
	if Eligible(unlistedOnly) {
		t.Error("only-unlisted post should not be eligible")
	}
}

func TestBuildPublishedAtFallsBackToCreatedAt(t *testing.T) {
	created := time.Date(2026, 5, 24, 8, 0, 0, 0, time.UTC)
	posts := []store.Post{{
		ID: "p1", CreatedAt: created, // FirstSuccessAt deliberately nil
		Targets: []store.Target{successTarget("nostr", "https://njump.me/x", "")},
	}}
	out := Build(posts, 20)
	if len(out.Posts) != 1 {
		t.Fatalf("got %d items, want 1", len(out.Posts))
	}
	if out.Posts[0].PublishedAt != created {
		t.Errorf("PublishedAt = %v, want CreatedAt %v when FirstSuccessAt is nil", out.Posts[0].PublishedAt, created)
	}
}

func ptr(t time.Time) *time.Time { return &t }
