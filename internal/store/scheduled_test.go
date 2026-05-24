package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestScheduledPostRoundTrip(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	at := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	rec := &Post{
		ID: "s1", CreatedAt: time.Now().UTC(), Platforms: []string{"bluesky"}, Source: "web",
		Status: "scheduled", ScheduledAt: &at,
		Targets: []Target{{Platform: "bluesky", Status: "scheduled", FinalText: "hi", FieldsJSON: `{"langs":["en"]}`}},
	}
	if err := db.SavePost(rec); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetPost("s1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "scheduled" {
		t.Errorf("status = %q", got.Status)
	}
	if got.ScheduledAt == nil || !got.ScheduledAt.Equal(at) {
		t.Errorf("scheduled_at = %v, want %v", got.ScheduledAt, at)
	}
	if len(got.Targets) != 1 || got.Targets[0].Status != "scheduled" {
		t.Errorf("target = %+v", got.Targets)
	}

	imm := &Post{ID: "i1", CreatedAt: time.Now().UTC(), Platforms: []string{"nostr"}, Source: "web", Status: "success"}
	if err := db.SavePost(imm); err != nil {
		t.Fatal(err)
	}
	got2, err := db.GetPost("i1")
	if err != nil {
		t.Fatal(err)
	}
	if got2.ScheduledAt != nil {
		t.Errorf("immediate post ScheduledAt = %v, want nil", got2.ScheduledAt)
	}
}

func mkScheduled(t *testing.T, db *Store, id string, at time.Time) {
	t.Helper()
	rec := &Post{
		ID: id, CreatedAt: time.Now().UTC(), Platforms: []string{"bluesky"}, Source: "web",
		Status: "scheduled", ScheduledAt: &at,
		Targets: []Target{{Platform: "bluesky", Status: "scheduled", FinalText: "hi", FieldsJSON: "{}"}},
	}
	if err := db.SavePost(rec); err != nil {
		t.Fatal(err)
	}
}

func TestDueAndClaim(t *testing.T) {
	db, _ := Open(filepath.Join(t.TempDir(), "t.db"))
	defer db.Close()
	now := time.Now().UTC()
	mkScheduled(t, db, "due", now.Add(-time.Minute))
	mkScheduled(t, db, "future", now.Add(time.Hour))

	due, err := db.DueScheduledPosts(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0] != "due" {
		t.Fatalf("due = %v, want [due]", due)
	}
	ok, err := db.ClaimScheduled("due")
	if err != nil || !ok {
		t.Fatalf("first claim = %v,%v want true", ok, err)
	}
	ok, _ = db.ClaimScheduled("due")
	if ok {
		t.Error("second claim should fail (already sending)")
	}
	due, _ = db.DueScheduledPosts(now)
	if len(due) != 0 {
		t.Errorf("claimed post still due: %v", due)
	}
}

func TestCancelReschedule(t *testing.T) {
	db, _ := Open(filepath.Join(t.TempDir(), "t.db"))
	defer db.Close()
	now := time.Now().UTC()
	mkScheduled(t, db, "c", now.Add(time.Hour))

	newAt := now.Add(3 * time.Hour).Truncate(time.Second)
	if err := db.RescheduleScheduled("c", newAt); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetPost("c")
	if got.ScheduledAt == nil || !got.ScheduledAt.Equal(newAt) {
		t.Errorf("reschedule: scheduled_at = %v", got.ScheduledAt)
	}

	if err := db.CancelScheduled("c"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetPost("c"); err == nil {
		t.Error("cancelled post should be gone")
	}

	mkScheduled(t, db, "live", now.Add(time.Hour))
	_, _ = db.ClaimScheduled("live") // → sending
	if err := db.CancelScheduled("live"); err != ErrNotPending {
		t.Errorf("cancel non-pending = %v, want ErrNotPending", err)
	}
	if err := db.RescheduleScheduled("live", newAt); err != ErrNotPending {
		t.Errorf("reschedule non-pending = %v, want ErrNotPending", err)
	}
}

func TestMarkMissedAndResetSending(t *testing.T) {
	db, _ := Open(filepath.Join(t.TempDir(), "t.db"))
	defer db.Close()
	now := time.Now().UTC()
	mkScheduled(t, db, "m", now.Add(-5*time.Hour))
	_, _ = db.ClaimScheduled("m") // → sending
	if err := db.MarkMissed("m"); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetPost("m")
	if got.Status != "missed" || got.Targets[0].Status != "missed" {
		t.Errorf("missed: post=%q target=%q", got.Status, got.Targets[0].Status)
	}

	mkScheduled(t, db, "stuck", now.Add(time.Hour))
	_, _ = db.ClaimScheduled("stuck") // → sending (simulate crash mid-send)
	n, err := db.ResetSendingToScheduled()
	if err != nil || n != 1 {
		t.Fatalf("reset = %d,%v want 1", n, err)
	}
	got, _ = db.GetPost("stuck")
	if got.Status != "scheduled" {
		t.Errorf("reset status = %q, want scheduled", got.Status)
	}
	// Reset must touch only 'sending' rows — the missed post stays missed.
	gm, _ := db.GetPost("m")
	if gm.Status != "missed" {
		t.Errorf("reset clobbered missed post: %q", gm.Status)
	}
}

func TestHidePost(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()

	// A terminal (success) post is hideable and drops out of ListPosts.
	done := &Post{ID: "done", CreatedAt: now, Platforms: []string{"nostr"}, Source: "web", Status: "success",
		Targets: []Target{{Platform: "nostr", Status: "success"}}}
	if err := db.SavePost(done); err != nil {
		t.Fatal(err)
	}
	if err := db.HidePost("done"); err != nil {
		t.Fatalf("hide success post: %v", err)
	}
	list, err := db.ListPosts(50, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range list {
		if p.ID == "done" {
			t.Error("hidden post still in ListPosts")
		}
	}
	// Data is kept: GetPost still resolves it.
	if _, err := db.GetPost("done"); err != nil {
		t.Errorf("hidden post should still be retrievable: %v", err)
	}

	// A pending (scheduled) post cannot be hidden.
	mkScheduled(t, db, "pend", now.Add(time.Hour))
	if err := db.HidePost("pend"); err != ErrNotHideable {
		t.Errorf("hide pending = %v, want ErrNotHideable", err)
	}
	// A missing post cannot be hidden.
	if err := db.HidePost("nope"); err != ErrNotHideable {
		t.Errorf("hide missing = %v, want ErrNotHideable", err)
	}
}

func TestListPostsFiredAt(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	fireTime := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	fired := &Post{
		ID: "f", CreatedAt: time.Now().Add(-2 * time.Hour).UTC(), Platforms: []string{"nostr"}, Source: "web", Status: "success",
		Targets: []Target{{Platform: "nostr", Status: "success",
			Attempts: []Attempt{{AttemptNo: 1, Status: "success", AttemptedAt: fireTime}}}},
	}
	if err := db.SavePost(fired); err != nil {
		t.Fatal(err)
	}
	mkScheduled(t, db, "pend", time.Now().Add(time.Hour)) // pending → no attempts

	list, err := db.ListPosts(50, 0)
	if err != nil {
		t.Fatal(err)
	}
	var f, p *Post
	for i := range list {
		switch list[i].ID {
		case "f":
			f = &list[i]
		case "pend":
			p = &list[i]
		}
	}
	if f == nil || p == nil {
		t.Fatalf("missing posts in list: %+v", list)
	}
	if f.FiredAt == nil || !f.FiredAt.Equal(fireTime) {
		t.Errorf("fired post FiredAt = %v, want %v", f.FiredAt, fireTime)
	}
	if p.FiredAt != nil {
		t.Errorf("pending post FiredAt = %v, want nil", p.FiredAt)
	}
}
