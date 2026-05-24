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
