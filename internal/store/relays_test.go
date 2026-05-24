package store

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRelayPersistenceAndRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Migration must be idempotent: opening again must not error.
	db.Close()
	db, err = Open(path)
	if err != nil {
		t.Fatalf("reopen (idempotent migrate): %v", err)
	}
	defer db.Close()

	rec := &Post{
		ID: "p1", CreatedAt: time.Now().UTC(), Platforms: []string{"nostr"}, Source: "web", Status: "partial",
		Targets: []Target{{
			Platform: "nostr", Status: "partial", FinalText: "hi",
			SignedEventJSON: `{"id":"ev1","sig":"deadbeef"}`,
			Relays: []RelayState{
				{URL: "wss://relay.geoffrey.one", Status: "ok"},
				{URL: "wss://relay.damus.io", Status: "failed", Message: "503"},
				{URL: "ws://x.onion", Status: "skipped"},
			},
			Attempts: []Attempt{{AttemptNo: 1, Status: "partial", AttemptedAt: time.Now()}},
		}},
	}
	if err := db.SavePost(rec); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetPost("p1")
	if err != nil {
		t.Fatal(err)
	}
	tg := got.Targets[0]
	if len(tg.Relays) != 3 {
		t.Fatalf("relays = %d, want 3", len(tg.Relays))
	}
	if tg.SignedEventJSON == "" {
		t.Errorf("signed event not persisted")
	}

	// Flip the failed relay to ok → target recomputes to success, post to success.
	targetID := tg.ID
	if err := db.UpdateRelayStatus(targetID, "wss://relay.damus.io", "ok", ""); err != nil {
		t.Fatal(err)
	}
	got, err = db.GetPost("p1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Targets[0].Status != "success" {
		t.Errorf("target status = %q, want success", got.Targets[0].Status)
	}
	if got.Status != "success" {
		t.Errorf("post status = %q, want success", got.Status)
	}
	// skipped relay must NOT count: 2 ok + 1 skipped = success.
}

func TestSignedEventNotSerialized(t *testing.T) {
	tg := Target{SignedEventJSON: "secret-event"}
	b, _ := json.Marshal(tg)
	if strings.Contains(string(b), "secret-event") {
		t.Errorf("SignedEventJSON leaked into JSON: %s", b)
	}
}
