package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// An unreadable thread or rows an older version orphaned must not stop the
// upgrade, since a failed migration fails every start after it.
func TestMigrateSurvivesUnreadableThreadAndOrphans(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	v2DB(t, path)
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(OFF)")
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `UPDATE post_targets SET segments_json='{bad' WHERE id=3`)
	mustExec(t, db, `INSERT INTO target_attempts (target_id, attempt_no, status, error, latency_ms, attempted_at)
		VALUES (999, 1, 'failed', 'x', 0, ?)`, time.Now().UTC())
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	sched, err := s.GetPost("sched")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(sched.Targets[0].Items); n != 1 || sched.Targets[0].Items[0].Text != "later" {
		t.Errorf("unreadable thread should become one item from the target row, got %+v", sched.Targets[0].Items)
	}

	// A legacy Bluesky part still shows its CID in the /api view.
	thread, _ := s.GetPost("thread")
	if got := thread.Targets[0].Segments[0].CID; got != "cid0" {
		t.Errorf("legacy segment cid = %q, want cid0", got)
	}
	// Only the part that stopped the chain carries the failure's class.
	if items := thread.Targets[0].Items; items[0].ErrorClass != "" || items[1].ErrorClass != "transient" {
		t.Errorf("error classes = %q, %q; want \"\", transient", items[0].ErrorClass, items[1].ErrorClass)
	}

	// The backup holds credentials, so only the owner may read it.
	fi, err := os.Stat(path + ".pre-v3.bak")
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("backup mode = %v, want 0600", fi.Mode().Perm())
	}
}

// New items are keyed as the dispatcher keys its sends, and planned items
// are due at their post's time, as migrated items are.
func TestNewItemsKeyedAndDue(t *testing.T) {
	st := openTestStore(t)
	at := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	p := &Post{ID: "nk", CreatedAt: time.Now().UTC(), MasterText: "x", Platforms: []string{"bluesky"},
		Source: "test", Status: "scheduled", ScheduledAt: &at,
		Targets: []Target{{Platform: "bluesky", Status: "scheduled", Segments: []Segment{
			{Ordinal: 0, Text: "a", Status: "pending"}, {Ordinal: 1, Text: "b", Status: "pending"}}}}}
	if err := st.SavePost(p); err != nil {
		t.Fatal(err)
	}
	items := func() []Item {
		gp, err := st.GetPost("nk")
		if err != nil {
			t.Fatal(err)
		}
		return gp.Targets[0].Items
	}
	for _, it := range items() {
		if it.IdemKey != ItemKey("nk", "bluesky", it.Ordinal) {
			t.Errorf("item %d key %q, want the derived key", it.Ordinal, it.IdemKey)
		}
		if it.NextAttemptAt == nil || !it.NextAttemptAt.Equal(at) {
			t.Errorf("item %d due %v, want %v", it.Ordinal, it.NextAttemptAt, at)
		}
	}

	later := at.Add(time.Hour)
	if err := st.RescheduleScheduled("nk", later); err != nil {
		t.Fatal(err)
	}
	for _, it := range items() {
		if it.NextAttemptAt == nil || !it.NextAttemptAt.Equal(later) {
			t.Errorf("rescheduled item %d due %v, want %v", it.Ordinal, it.NextAttemptAt, later)
		}
	}

	if ok, err := st.markMissed(context.Background(), "nk"); err != nil || !ok {
		t.Fatalf("markMissed = %v, %v", ok, err)
	}
	for _, it := range items() {
		if it.NextAttemptAt != nil {
			t.Errorf("missed item %d still due at %v", it.Ordinal, it.NextAttemptAt)
		}
	}
}

// Retrying a Nostr thread's head relay can't mark the thread a success
// while one of its parts is unpublished.
func TestRelayRetryLeavesUnfinishedThread(t *testing.T) {
	st := openTestStore(t)
	p := &Post{ID: "nt", CreatedAt: time.Now().UTC(), MasterText: "x", Platforms: []string{"nostr"},
		Source: "test", Status: "partial",
		Targets: []Target{{Platform: "nostr", Status: "partial", SignedEventJSON: `{"id":"e0"}`,
			Relays: []RelayState{{URL: "wss://r1", Status: "failed"}},
			Segments: []Segment{
				{Ordinal: 0, Text: "a", Status: "success", RemoteID: "e0"},
				{Ordinal: 1, Text: "b", Status: "failed", Error: "boom"}}}}}
	if err := st.SavePost(p); err != nil {
		t.Fatal(err)
	}
	gp, _ := st.GetPost("nt")
	if err := st.UpdateRelayStatus(gp.Targets[0].ID, "wss://r1", "ok", ""); err != nil {
		t.Fatal(err)
	}
	gp, _ = st.GetPost("nt")
	if gp.Targets[0].Status != "partial" || gp.Status == "success" {
		t.Errorf("after head relay retry: target %q post %q; want the thread left partial", gp.Targets[0].Status, gp.Status)
	}
}

// Writers that read first don't fail with SQLITE_BUSY under contention: each
// transaction takes the write lock up front and waits its turn.
func TestConcurrentWritersDontFailBusy(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	errs := make(chan error, 400)
	done := make(chan struct{})
	for g := 0; g < 20; g++ {
		go func(g int) {
			defer func() { done <- struct{}{} }()
			for i := 0; i < 10; i++ {
				id := newID()
				p := &Post{ID: id, CreatedAt: time.Now().UTC(), MasterText: "x", Platforms: []string{"mastodon"},
					Source: "test", Status: "failed", Targets: []Target{{Platform: "mastodon", Status: "failed"}}}
				if err := st.SavePost(p); err != nil {
					errs <- err
					continue
				}
				gp, err := st.GetPost(id)
				if err != nil {
					errs <- err
					continue
				}
				ctx := context.Background()
				tid := gp.Targets[0].ID
				if _, err := st.MarkDue(ctx, id, []int64{tid}, time.Now()); err != nil {
					errs <- err
					continue
				}
				c, err := st.ClaimItem(ctx, time.Now(), ClaimFilter{TargetID: tid})
				if err != nil || c == nil {
					errs <- fmt.Errorf("claim: %v, %v", c, err)
					continue
				}
				if _, err := st.RecordItemResult(ctx, ItemResult{Claim: *c, Status: ItemPublished, RemoteID: "r", At: time.Now()}); err != nil {
					errs <- err
				}
			}
		}(g)
	}
	for g := 0; g < 20; g++ {
		<-done
	}
	close(errs)
	n := 0
	for err := range errs {
		if n == 0 {
			t.Errorf("first error: %v", err)
		}
		n++
	}
	if n > 0 {
		t.Errorf("%d of 400 operations failed", n)
	}
}
