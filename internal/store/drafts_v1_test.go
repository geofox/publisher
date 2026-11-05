package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestV1DraftVersionsAndOwners(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.sql.Exec(`INSERT INTO owners (id, created_at) VALUES ('own_other', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	d := &V1Draft{Title: "t", Tags: []string{"#A", "a"}, Text: "x", Body: `{}`}
	if err := s.CreateV1Draft(ctx, d); err != nil {
		t.Fatal(err)
	}
	if d.OwnerID != DefaultOwner || d.Version != 1 || len(d.Tags) != 1 {
		t.Fatalf("created %+v", d)
	}
	if _, err := s.GetV1Draft(ctx, "own_other", d.ID); !errors.Is(err, ErrDraftNotFound) {
		t.Fatalf("other owner read: %v", err)
	}
	other := *d
	other.OwnerID = "own_other"
	if err := s.UpdateV1Draft(ctx, &other, 0); !errors.Is(err, ErrDraftNotFound) {
		t.Fatalf("other owner update: %v", err)
	}
	if err := s.DeleteV1Draft(ctx, "own_other", d.ID, 0); !errors.Is(err, ErrDraftNotFound) {
		t.Fatalf("other owner delete: %v", err)
	}
	d.Text = "y"
	if err := s.UpdateV1Draft(ctx, d, 1); err != nil || d.Version != 2 || d.Text != "y" {
		t.Fatalf("update: %v %+v", err, d)
	}
	stale := *d
	if err := s.UpdateV1Draft(ctx, &stale, 1); !errors.Is(err, ErrDraftConflict) {
		t.Fatalf("stale update: %v", err)
	}
	if err := s.DeleteV1Draft(ctx, DefaultOwner, d.ID, 1); !errors.Is(err, ErrDraftConflict) {
		t.Fatalf("stale delete: %v", err)
	}
	if err := s.DeleteV1Draft(ctx, DefaultOwner, d.ID, 2); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteV1Draft(ctx, DefaultOwner, d.ID, 0); !errors.Is(err, ErrDraftNotFound) {
		t.Fatalf("delete again: %v", err)
	}
}

func TestListV1DraftsOrderAndKeyset(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for _, txt := range []string{"a_b", "axb", "c"} {
		if err := s.CreateV1Draft(ctx, &V1Draft{Text: txt, Body: `{}`}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	got, err := s.ListV1Drafts(ctx, V1DraftQuery{OwnerID: DefaultOwner, Text: "a_b"})
	if err != nil || len(got) != 1 || got[0].Text != "a_b" {
		t.Fatalf("_ is literal: %+v %v", got, err)
	}
	all, _ := s.ListV1Drafts(ctx, V1DraftQuery{OwnerID: DefaultOwner})
	if len(all) != 3 || all[0].Text != "c" {
		t.Fatalf("order: %+v", all)
	}
	rest, _ := s.ListV1Drafts(ctx, V1DraftQuery{OwnerID: DefaultOwner, BeforeTime: &all[0].UpdatedAt, BeforeID: all[0].ID})
	if len(rest) != 2 || rest[0].ID != all[1].ID {
		t.Fatalf("after first: %+v", rest)
	}
}
