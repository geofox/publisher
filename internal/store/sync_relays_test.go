package store

import (
	"path/filepath"
	"testing"
)

func TestSyncRelaysCRUDAndSeed(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Seed only when empty.
	if err := db.SeedSyncRelaysIfEmpty([]string{"wss://a", "wss://b"}); err != nil {
		t.Fatal(err)
	}
	got, err := db.SyncRelays()
	if err != nil || len(got) != 2 {
		t.Fatalf("after seed: %v len=%d", err, len(got))
	}
	// Re-seed must be a no-op (does not re-add / resurrect removed entries).
	if err := db.RemoveSyncRelay("wss://a"); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedSyncRelaysIfEmpty([]string{"wss://a", "wss://b"}); err != nil {
		t.Fatal(err)
	}
	got, err = db.SyncRelays()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "wss://b" {
		t.Errorf("re-seed should be no-op, got %v", got)
	}
	// Add is idempotent.
	if err := db.AddSyncRelay("wss://c"); err != nil {
		t.Fatal(err)
	}
	if err := db.AddSyncRelay("wss://c"); err != nil {
		t.Fatal(err)
	}
	got, err = db.SyncRelays()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("add idempotent: got %v", got)
	}
}
