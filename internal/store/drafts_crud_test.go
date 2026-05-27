package store

import (
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "drafts.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestCreateAndGetDraft(t *testing.T) {
	s := openTestStore(t)
	d := &Draft{
		ID:         "d1",
		CreatedAt:  time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
		Title:      "hello",
		MasterText: "hello world",
		Tags:       []string{"essay", "bugfix"},
		Spec:       `{"master_text":"hello world","platforms":["nostr"]}`,
		Media: []Media{
			{Ordinal: 0, BlossomURL: "https://blossom/x", SHA256: "abc", Mime: "image/png", Alt: "fig"},
		},
	}
	if err := s.CreateDraft(d); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	got, err := s.GetDraft("d1")
	if err != nil {
		t.Fatalf("GetDraft: %v", err)
	}
	if got.Title != "hello" || got.MasterText != "hello world" {
		t.Errorf("title/master mismatch: %+v", got)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "essay" {
		t.Errorf("tags mismatch: %v", got.Tags)
	}
	if len(got.Media) != 1 || got.Media[0].BlossomURL != "https://blossom/x" {
		t.Errorf("media mismatch: %+v", got.Media)
	}
	if got.Spec == "" {
		t.Errorf("spec round-trip lost")
	}
}

func TestGetDraftNotFound(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.GetDraft("missing"); err == nil {
		t.Fatal("expected error for missing draft")
	}
}
