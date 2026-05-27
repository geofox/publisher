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

func TestUpdateDraftReplacesMedia(t *testing.T) {
	s := openTestStore(t)
	d := &Draft{
		ID: "u1", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		MasterText: "v1", Spec: `{}`,
		Media: []Media{{Ordinal: 0, BlossomURL: "u-a", SHA256: "a"}},
	}
	if err := s.CreateDraft(d); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	d.MasterText = "v2"
	d.UpdatedAt = time.Now().UTC().Add(time.Second)
	d.Media = []Media{{Ordinal: 0, BlossomURL: "u-b", SHA256: "b"}}
	if err := s.UpdateDraft(d); err != nil {
		t.Fatalf("UpdateDraft: %v", err)
	}
	got, err := s.GetDraft("u1")
	if err != nil {
		t.Fatalf("GetDraft: %v", err)
	}
	if got.MasterText != "v2" {
		t.Errorf("master not updated: %q", got.MasterText)
	}
	if len(got.Media) != 1 || got.Media[0].BlossomURL != "u-b" {
		t.Errorf("media not replaced: %+v", got.Media)
	}
}

func TestUpdateDraftNotFound(t *testing.T) {
	s := openTestStore(t)
	d := &Draft{ID: "missing", UpdatedAt: time.Now().UTC(), Spec: `{}`}
	if err := s.UpdateDraft(d); err == nil {
		t.Fatal("expected ErrDraftNotFound")
	}
}

func TestDeleteDraftCascadesMedia(t *testing.T) {
	s := openTestStore(t)
	d := &Draft{
		ID: "del1", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		MasterText: "bye", Spec: `{}`,
		Media: []Media{{Ordinal: 0, BlossomURL: "x", SHA256: "y"}},
	}
	if err := s.CreateDraft(d); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if err := s.DeleteDraft("del1"); err != nil {
		t.Fatalf("DeleteDraft: %v", err)
	}
	if _, err := s.GetDraft("del1"); err == nil {
		t.Error("draft still gettable after delete")
	}
	// confirm cascade
	var n int
	if err := s.sql.QueryRow(`SELECT COUNT(*) FROM draft_media WHERE draft_id='del1'`).Scan(&n); err != nil {
		t.Fatalf("count media: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 media rows after delete, got %d", n)
	}
}

func TestDeleteDraftNotFound(t *testing.T) {
	s := openTestStore(t)
	if err := s.DeleteDraft("nope"); err == nil {
		t.Fatal("expected ErrDraftNotFound")
	}
}
