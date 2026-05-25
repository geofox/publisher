package store

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSaveAndGetPost(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rec := &Post{
		ID: "abc123", CreatedAt: time.Now().UTC().Truncate(time.Second),
		MasterText: "hi", Platforms: []string{"nostr", "mastodon"}, Source: "web", Status: "partial",
		Media: []Media{{Ordinal: 0, BlossomURL: "https://b/x", SHA256: "deadbeef", Alt: "a cat"}},
		Targets: []Target{
			{Platform: "nostr", FinalText: "hi", Status: "success", RemoteID: "ev1",
				Attempts: []Attempt{{AttemptNo: 1, Status: "success", RequestJSON: "{}", ResponseJSON: "{}", AttemptedAt: time.Now()}}},
			{Platform: "mastodon", FinalText: "hi", Status: "failed",
				Attempts: []Attempt{{AttemptNo: 1, Status: "failed", Error: "boom", AttemptedAt: time.Now()}}},
		},
	}
	if err := db.SavePost(rec); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetPost("abc123")
	if err != nil {
		t.Fatal(err)
	}
	if got.MasterText != "hi" || len(got.Targets) != 2 || len(got.Media) != 1 {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if got.Targets[0].Attempts[0].AttemptNo != 1 {
		t.Errorf("attempt not loaded")
	}
	list, err := db.ListPosts(10, 0)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v len=%d", err, len(list))
	}
	// list view carries lightweight per-platform status for the pills
	if len(list[0].Targets) != 2 {
		t.Fatalf("list targets = %d, want 2", len(list[0].Targets))
	}
	byPlat := map[string]string{}
	for _, tg := range list[0].Targets {
		byPlat[tg.Platform] = tg.Status
	}
	if byPlat["nostr"] != "success" || byPlat["mastodon"] != "failed" {
		t.Errorf("list per-platform status wrong: %v", byPlat)
	}
}

func TestSavePostScrubsAttemptSecrets(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil { t.Fatal(err) }
	defer db.Close()
	rec := &Post{
		ID: "p1", CreatedAt: time.Now().UTC(), Platforms: []string{"mastodon"}, Source: "web", Status: "success",
		Targets: []Target{{Platform: "mastodon", Status: "success",
			Attempts: []Attempt{{AttemptNo: 1, Status: "success",
				RequestJSON:  `{"Authorization":"Bearer SECRET123","text":"hi"}`,
				ResponseJSON: `{"ok":true}`, AttemptedAt: time.Now()}}}},
	}
	if err := db.SavePost(rec); err != nil { t.Fatal(err) }
	got, err := db.GetPost("p1")
	if err != nil { t.Fatal(err) }
	rj := got.Targets[0].Attempts[0].RequestJSON
	if strings.Contains(rj, "SECRET123") {
		t.Errorf("secret persisted unscrubbed: %s", rj)
	}
	if !strings.Contains(rj, "hi") {
		t.Errorf("over-scrubbed: %s", rj)
	}
}

func TestSavePostWithSegments(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "seg.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rec := &Post{
		ID: "seg1", CreatedAt: time.Now().UTC().Truncate(time.Second),
		MasterText: "long", Platforms: []string{"bluesky"}, Source: "web", Status: "partial",
		Targets: []Target{{
			Platform: "bluesky", FinalText: "long", Status: "partial",
			RemoteID: "at://seg0", RemoteURL: "https://bsky/0",
			Segments: []Segment{
				{Ordinal: 0, Text: "seg zero 1/2", RemoteID: "at://seg0", RemoteURL: "https://bsky/0", CID: "cid0", Status: "success"},
				{Ordinal: 1, Text: "seg one 2/2", Status: "failed", Error: "boom"},
			},
			Attempts: []Attempt{{AttemptNo: 1, Status: "partial", AttemptedAt: time.Now()}},
		}},
	}
	if err := db.SavePost(rec); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetPost("seg1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Targets) != 1 {
		t.Fatalf("targets: %+v", got.Targets)
	}
	segs := got.Targets[0].Segments
	if len(segs) != 2 {
		t.Fatalf("segments not round-tripped: %+v", segs)
	}
	if segs[0].CID != "cid0" || segs[0].Status != "success" || segs[1].Status != "failed" || segs[1].Error != "boom" {
		t.Errorf("segment fields wrong: %+v", segs)
	}
}

func TestSavePostNoSegmentsIsEmpty(t *testing.T) {
	db, _ := Open(filepath.Join(t.TempDir(), "noseg.db"))
	defer db.Close()
	rec := &Post{
		ID: "ns1", CreatedAt: time.Now().UTC().Truncate(time.Second),
		Platforms: []string{"mastodon"}, Source: "web", Status: "success",
		Targets: []Target{{Platform: "mastodon", Status: "success", RemoteID: "m1",
			Attempts: []Attempt{{AttemptNo: 1, Status: "success", AttemptedAt: time.Now()}}}},
	}
	if err := db.SavePost(rec); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetPost("ns1")
	if len(got.Targets[0].Segments) != 0 {
		t.Fatalf("expected no segments, got %+v", got.Targets[0].Segments)
	}
}

func TestPostInteractionRoundTrip(t *testing.T) {
	db, _ := Open(filepath.Join(t.TempDir(), "i.db"))
	defer db.Close()
	rec := &Post{
		ID: "i1", CreatedAt: time.Now().UTC().Truncate(time.Second),
		Platforms: []string{"bluesky"}, Source: "web", Status: "success",
		Interaction: &Interaction{Action: "quote", SourcePlatform: "bluesky",
			SourceURL: "https://bsky.app/x", SourceAuthor: "@alice"},
		Targets: []Target{{Platform: "bluesky", Status: "success"}},
	}
	if err := db.SavePost(rec); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetPost("i1")
	if got.Interaction == nil || got.Interaction.Action != "quote" ||
		got.Interaction.SourceAuthor != "@alice" || got.Interaction.SourceURL != "https://bsky.app/x" {
		t.Fatalf("interaction not round-tripped: %+v", got.Interaction)
	}
}

func TestListPostsCarriesInteraction(t *testing.T) {
	// The list view (not just detail) must load the interaction descriptor so the
	// history badge renders in the list.
	db, _ := Open(filepath.Join(t.TempDir(), "li.db"))
	defer db.Close()
	rec := &Post{ID: "li1", CreatedAt: time.Now().UTC().Truncate(time.Second),
		Platforms: []string{"bluesky"}, Source: "web", Status: "success",
		Interaction: &Interaction{Action: "repost", SourcePlatform: "bluesky", SourceAuthor: "@bob"},
		Targets: []Target{{Platform: "bluesky", Status: "success"}}}
	if err := db.SavePost(rec); err != nil {
		t.Fatal(err)
	}
	list, err := db.ListPosts(10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Interaction == nil || list[0].Interaction.Action != "repost" || list[0].Interaction.SourceAuthor != "@bob" {
		t.Fatalf("list view should carry interaction: %+v", list)
	}
}

func TestGetPostHandlesLegacyNullInteraction(t *testing.T) {
	// Rows that predate the interaction_json column have it as SQL NULL; GetPost
	// must read them without error and leave Interaction nil.
	db, _ := Open(filepath.Join(t.TempDir(), "legacy.db"))
	defer db.Close()
	rec := &Post{ID: "l1", CreatedAt: time.Now().UTC().Truncate(time.Second),
		Platforms: []string{"nostr"}, Source: "web", Status: "success",
		Targets: []Target{{Platform: "nostr", Status: "success"}}}
	if err := db.SavePost(rec); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`UPDATE posts SET interaction_json=NULL WHERE id=?`, "l1"); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetPost("l1")
	if err != nil {
		t.Fatalf("GetPost must tolerate NULL interaction_json: %v", err)
	}
	if got.Interaction != nil {
		t.Errorf("NULL interaction should load as nil, got %+v", got.Interaction)
	}
}

func TestPostWithoutInteractionLoadsNil(t *testing.T) {
	db, _ := Open(filepath.Join(t.TempDir(), "n.db"))
	defer db.Close()
	rec := &Post{ID: "n1", CreatedAt: time.Now().UTC().Truncate(time.Second),
		Platforms: []string{"nostr"}, Source: "web", Status: "success",
		Targets: []Target{{Platform: "nostr", Status: "success"}}}
	if err := db.SavePost(rec); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetPost("n1")
	if got.Interaction != nil {
		t.Fatalf("normal post should have nil interaction, got %+v", got.Interaction)
	}
}

func TestUpdateTargetSegments(t *testing.T) {
	db, _ := Open(filepath.Join(t.TempDir(), "upd.db"))
	defer db.Close()
	rec := &Post{
		ID: "u1", CreatedAt: time.Now().UTC().Truncate(time.Second),
		Platforms: []string{"bluesky"}, Source: "web", Status: "partial",
		Targets: []Target{{
			Platform: "bluesky", Status: "partial", RemoteID: "at://0", RemoteURL: "https://b/0",
			Segments: []Segment{
				{Ordinal: 0, Text: "a", RemoteID: "at://0", CID: "c0", Status: "success"},
				{Ordinal: 1, Text: "b", Status: "failed", Error: "x"},
			},
			Attempts: []Attempt{{AttemptNo: 1, Status: "partial", AttemptedAt: time.Now()}},
		}},
	}
	if err := db.SavePost(rec); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetPost("u1")
	tid := got.Targets[0].ID

	resumed := []Segment{
		{Ordinal: 0, Text: "a", RemoteID: "at://0", CID: "c0", Status: "success"},
		{Ordinal: 1, Text: "b", RemoteID: "at://1", CID: "c1", Status: "success"},
	}
	if err := db.UpdateTargetSegments(tid, resumed, "success", "at://0", "https://b/0", 12, ""); err != nil {
		t.Fatal(err)
	}
	after, _ := db.GetPost("u1")
	tg := after.Targets[0]
	if tg.Status != "success" || len(tg.Segments) != 2 || tg.Segments[1].Status != "success" || tg.Segments[1].RemoteID != "at://1" {
		t.Fatalf("segments not updated: status=%s segs=%+v", tg.Status, tg.Segments)
	}
	if tg.AttemptCount < 2 {
		t.Errorf("attempt_count should bump: %d", tg.AttemptCount)
	}
	if after.Status != "success" {
		t.Errorf("post status should recompute to success: %s", after.Status)
	}
}
