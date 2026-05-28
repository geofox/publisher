package store

import (
	"path/filepath"
	"testing"
	"time"
)

// mkFeedPost saves a post with explicit per-target attempts so tests control
// the first-success time. Each target gets one attempt per (status, time) pair.
func mkFeedPost(t *testing.T, db *Store, id, status string, targets []Target) {
	t.Helper()
	rec := &Post{
		ID:         id,
		CreatedAt:  time.Now().UTC(),
		MasterText: "text " + id,
		Platforms:  []string{},
		Source:     "web",
		Status:     status,
		Targets:    targets,
	}
	for _, tg := range targets {
		rec.Platforms = append(rec.Platforms, tg.Platform)
	}
	if err := db.SavePost(rec); err != nil {
		t.Fatalf("mkFeedPost %q: %v", id, err)
	}
}

func TestPublicFeedFirstSuccessAndOrder(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	t1 := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC) // a later retry
	t3 := time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC)

	// Older post: nostr succeeded at t1; mastodon failed at t1 then succeeded
	// on retry at t2. First success = t1; a retry must not move it.
	mkFeedPost(t, db, "old", "success", []Target{
		{Platform: "nostr", Status: "success", RemoteURL: "https://njump.me/x",
			Attempts: []Attempt{{AttemptNo: 1, Status: "success", AttemptedAt: t1}}},
		{Platform: "mastodon", Status: "success", RemoteURL: "https://m/1", FieldsJSON: `{"visibility":"public"}`,
			Attempts: []Attempt{
				{AttemptNo: 1, Status: "failed", AttemptedAt: t1},
				{AttemptNo: 2, Status: "success", AttemptedAt: t2},
			}},
	})
	// Newer post: nostr succeeded at t3.
	mkFeedPost(t, db, "new", "success", []Target{
		{Platform: "nostr", Status: "success", RemoteURL: "https://njump.me/y",
			Attempts: []Attempt{{AttemptNo: 1, Status: "success", AttemptedAt: t3}}},
	})

	posts, err := db.PublicFeed(20)
	if err != nil {
		t.Fatalf("PublicFeed: %v", err)
	}
	if len(posts) != 2 {
		t.Fatalf("got %d posts, want 2", len(posts))
	}
	// Ordered by first-success DESC → "new" (t3) before "old" (t1).
	if posts[0].ID != "new" || posts[1].ID != "old" {
		t.Fatalf("order = [%s,%s], want [new,old]", posts[0].ID, posts[1].ID)
	}
	if posts[1].FirstSuccessAt == nil || !posts[1].FirstSuccessAt.Equal(t1) {
		t.Fatalf("old FirstSuccessAt = %v, want %v (retry must not move it)", posts[1].FirstSuccessAt, t1)
	}
	// Targets hydrated with remote_url + fields_json.
	var masto *Target
	for i := range posts[1].Targets {
		if posts[1].Targets[i].Platform == "mastodon" {
			masto = &posts[1].Targets[i]
		}
	}
	if masto == nil || masto.RemoteURL != "https://m/1" || masto.FieldsJSON != `{"visibility":"public"}` {
		t.Fatalf("mastodon target not hydrated: %+v", masto)
	}
}

func TestPublicFeedExcludesHiddenAndUnpublished(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ts := time.Date(2026, 5, 22, 10, 0, 0, 0, time.UTC)
	ok := []Target{{Platform: "nostr", Status: "success", RemoteURL: "https://njump.me/z",
		Attempts: []Attempt{{AttemptNo: 1, Status: "success", AttemptedAt: ts}}}}

	mkFeedPost(t, db, "shown", "success", ok)
	mkFeedPost(t, db, "hidden", "success", ok)
	mkFeedPost(t, db, "scheduled", "scheduled", []Target{{Platform: "nostr", Status: "scheduled"}})
	mkFeedPost(t, db, "failed", "failed", []Target{{Platform: "nostr", Status: "failed",
		Attempts: []Attempt{{AttemptNo: 1, Status: "failed", AttemptedAt: ts}}}})

	if err := db.HidePost("hidden"); err != nil {
		t.Fatalf("HidePost: %v", err)
	}

	posts, err := db.PublicFeed(20)
	if err != nil {
		t.Fatalf("PublicFeed: %v", err)
	}
	if len(posts) != 1 || posts[0].ID != "shown" {
		ids := make([]string, len(posts))
		for i, p := range posts {
			ids[i] = p.ID
		}
		t.Fatalf("got %v, want [shown] (hidden/scheduled/failed excluded)", ids)
	}
}
