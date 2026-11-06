package store

import (
	"testing"
	"time"
)

func TestLookupMediaURL_postsAndDrafts(t *testing.T) {
	s := openTestStore(t)
	// no rows yet → empty result, no error
	if url, ok, err := s.LookupMediaURL("abc123"); err != nil || ok || url != "" {
		t.Fatalf("expected empty miss, got %q ok=%v err=%v", url, ok, err)
	}
	// draft media hit
	mkDraft(t, s, "d1", "hi", nil, "https://blossom/x")
	url, ok, err := s.LookupMediaURL("x") // mkDraft used SHA256="x"
	if err != nil {
		t.Fatalf("lookup draft sha: %v", err)
	}
	if !ok || url != "https://blossom/x" {
		t.Errorf("draft hit: got url=%q ok=%v", url, ok)
	}
	// post media hit
	post := &Post{
		ID: "p1", CreatedAt: time.Now().UTC(), MasterText: "x",
		Platforms: []string{"nostr"}, Source: "test", Status: "sent",
		Targets: []Target{{Platform: "nostr", Status: "sent"}},
		Media:   []Media{{Ordinal: 0, BlossomURL: "https://blossom/p", SHA256: "p-sha"}},
	}
	if err := saveWithAccounts(s, post); err != nil {
		t.Fatalf("SavePost: %v", err)
	}
	url, ok, err = s.LookupMediaURL("p-sha")
	if err != nil {
		t.Fatalf("lookup post sha: %v", err)
	}
	if !ok || url != "https://blossom/p" {
		t.Errorf("post hit: got url=%q ok=%v", url, ok)
	}
}

func mkDraft(t *testing.T, s *Store, id, master string, tags []string, mediaURL string) {
	t.Helper()
	d := &Draft{ID: id, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), MasterText: master, Tags: tags, Spec: `{}`}
	if mediaURL != "" {
		d.Media = []Media{{Ordinal: 0, BlossomURL: mediaURL, SHA256: "x"}}
	}
	if err := s.CreateDraft(d); err != nil {
		t.Fatalf("mkDraft %q: %v", id, err)
	}
}
