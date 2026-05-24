package store

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func mkPost(t *testing.T, db *Store, id, status, text string) {
	t.Helper()
	rec := &Post{
		ID:         id,
		CreatedAt:  time.Now().UTC(),
		MasterText: text,
		Platforms:  []string{"nostr"},
		Source:     "web",
		Status:     status,
		Targets:    []Target{{Platform: "nostr", Status: status}},
	}
	if err := db.SavePost(rec); err != nil {
		t.Fatalf("mkPost %q: %v", id, err)
	}
}

func TestListPostsFiltered(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// ── Status filter ──────────────────────────────────────────────────────
	mkPost(t, db, "s1", "success", "post one")
	mkPost(t, db, "s2", "failed", "post two")
	mkPost(t, db, "s3", "partial", "post three")
	mkPost(t, db, "s4", "scheduled", "post four")
	mkPost(t, db, "s5", "missed", "post five")

	all5 := func(t *testing.T, status string) {
		t.Helper()
		posts, err := db.ListPostsFiltered(PostFilter{Status: status, Limit: 50})
		if err != nil {
			t.Fatalf("status=%q: %v", status, err)
		}
		if len(posts) != 5 {
			t.Errorf("status=%q: got %d posts, want 5", status, len(posts))
		}
	}
	all5(t, "")
	all5(t, "all")

	sentPosts, err := db.ListPostsFiltered(PostFilter{Status: "sent", Limit: 50})
	if err != nil {
		t.Fatalf("status=sent: %v", err)
	}
	if len(sentPosts) != 1 {
		t.Errorf("status=sent: got %d posts, want 1", len(sentPosts))
	} else if sentPosts[0].Status != "success" {
		t.Errorf("status=sent: post status = %q, want success", sentPosts[0].Status)
	}

	// scheduled maps to scheduled+sending — we have "scheduled" (and not "sending"), so expect 1
	scheduledPosts, err := db.ListPostsFiltered(PostFilter{Status: "scheduled", Limit: 50})
	if err != nil {
		t.Fatalf("status=scheduled: %v", err)
	}
	if len(scheduledPosts) != 1 {
		t.Errorf("status=scheduled: got %d posts, want 1", len(scheduledPosts))
	}

	// Add a "sending" post so we can confirm the mapping includes both
	mkPost(t, db, "s6", "sending", "post six")
	scheduledPosts2, err := db.ListPostsFiltered(PostFilter{Status: "scheduled", Limit: 50})
	if err != nil {
		t.Fatalf("status=scheduled+sending: %v", err)
	}
	if len(scheduledPosts2) != 2 {
		t.Errorf("status=scheduled (both scheduled+sending): got %d posts, want 2", len(scheduledPosts2))
	}

	// failed maps to failed+partial+missed — 3 posts
	failedPosts, err := db.ListPostsFiltered(PostFilter{Status: "failed", Limit: 50})
	if err != nil {
		t.Fatalf("status=failed: %v", err)
	}
	if len(failedPosts) != 3 {
		t.Errorf("status=failed: got %d posts, want 3", len(failedPosts))
	}

	// ── Search ─────────────────────────────────────────────────────────────
	db2, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()

	mkPost(t, db2, "q1", "success", "Hello world")
	mkPost(t, db2, "q2", "success", "HELLO THERE")
	mkPost(t, db2, "q3", "success", "nothing here")

	qPosts, err := db2.ListPostsFiltered(PostFilter{Query: "HELLO", Limit: 50})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(qPosts) != 2 {
		t.Errorf("search HELLO: got %d posts, want 2", len(qPosts))
	}

	// ── Paging ─────────────────────────────────────────────────────────────
	db3, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db3.Close()

	for i := 0; i < 4; i++ {
		mkPost(t, db3, fmt.Sprintf("p%d", i), "success", fmt.Sprintf("page post %d", i))
	}

	page1, err := db3.ListPostsFiltered(PostFilter{Limit: 2, Offset: 0})
	if err != nil {
		t.Fatalf("paging page1: %v", err)
	}
	if len(page1) != 2 {
		t.Errorf("page1: got %d, want 2", len(page1))
	}

	page2, err := db3.ListPostsFiltered(PostFilter{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("paging page2: %v", err)
	}
	if len(page2) != 2 {
		t.Errorf("page2: got %d, want 2", len(page2))
	}

	ids1 := map[string]bool{page1[0].ID: true, page1[1].ID: true}
	for _, p := range page2 {
		if ids1[p.ID] {
			t.Errorf("paging: post %q appears in both pages", p.ID)
		}
	}
}
