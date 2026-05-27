package store

import (
	"testing"
	"time"
)

func mkDraft(t *testing.T, s *Store, id, master string, tags []string, mediaURL string) {
	t.Helper()
	d := &Draft{
		ID:         id,
		CreatedAt:  time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
		MasterText: master,
		Tags:       tags,
		Spec:       `{}`,
	}
	if mediaURL != "" {
		d.Media = []Media{{Ordinal: 0, BlossomURL: mediaURL, SHA256: "x"}}
	}
	if err := s.CreateDraft(d); err != nil {
		t.Fatalf("mkDraft %q: %v", id, err)
	}
}

func TestListDraftsFiltered(t *testing.T) {
	s := openTestStore(t)
	mkDraft(t, s, "a", "hello world", []string{"essay"}, "https://blossom/img-a")
	mkDraft(t, s, "b", "second one", []string{"bugfix"}, "")
	mkDraft(t, s, "c", "hello again", []string{"essay", "bugfix"}, "")

	all, err := s.ListDraftsFiltered(DraftFilter{Limit: 50})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("expected 3, got %d", len(all))
	}

	hello, err := s.ListDraftsFiltered(DraftFilter{Query: "hello", Limit: 50})
	if err != nil {
		t.Fatalf("list q=hello: %v", err)
	}
	if len(hello) != 2 {
		t.Errorf("q=hello expected 2, got %d", len(hello))
	}

	essay, err := s.ListDraftsFiltered(DraftFilter{Tags: []string{"essay"}, Limit: 50})
	if err != nil {
		t.Fatalf("tag=essay: %v", err)
	}
	if len(essay) != 2 {
		t.Errorf("tag=essay expected 2, got %d", len(essay))
	}

	both, err := s.ListDraftsFiltered(DraftFilter{Tags: []string{"essay", "bugfix"}, Limit: 50})
	if err != nil {
		t.Fatalf("tags AND: %v", err)
	}
	if len(both) != 1 || both[0].ID != "c" {
		t.Errorf("AND filter expected only c, got %+v", both)
	}

	withImg, err := s.ListDraftsFiltered(DraftFilter{Limit: 50})
	if err != nil {
		t.Fatalf("list for media check: %v", err)
	}
	var foundImg string
	for _, it := range withImg {
		if it.ID == "a" {
			foundImg = it.FirstMediaURL
		}
	}
	if foundImg != "https://blossom/img-a" {
		t.Errorf("expected first_media_url for a, got %q", foundImg)
	}
}
