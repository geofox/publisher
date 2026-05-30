package store

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGaveUpColumnsAndLastAttemptLoad(t *testing.T) {
	st := openTestStore(t) // existing same-package helper (store_test.go-style)
	p := &Post{
		ID: "p-gaveup", CreatedAt: time.Now().UTC(), MasterText: "x",
		Platforms: []string{"bluesky"}, Source: "test", Status: "failed",
		Targets: []Target{{Platform: "bluesky", Status: "failed"}},
	}
	if err := st.SavePost(p); err != nil {
		t.Fatalf("SavePost: %v", err)
	}
	// Record an attempt so last_attempt_at is set.
	got, _ := st.GetPost("p-gaveup")
	tid := got.Targets[0].ID
	if err := st.AppendTargetAttempt(tid, "failed", "boom", "", "", 5, "", "", nil, ""); err != nil {
		t.Fatalf("AppendTargetAttempt: %v", err)
	}
	got, err := st.GetPost("p-gaveup")
	if err != nil {
		t.Fatalf("GetPost: %v", err)
	}
	if got.Targets[0].LastAttempt.IsZero() {
		t.Error("LastAttempt should be loaded, got zero")
	}
	if got.Targets[0].GaveUpAt != nil {
		t.Error("GaveUpAt should be nil initially")
	}
}

func TestPostsNeedingRetryAndMarkGaveUp(t *testing.T) {
	st := openTestStore(t)
	mk := func(id, status string) {
		p := &Post{ID: id, CreatedAt: time.Now().UTC(), MasterText: "x",
			Platforms: []string{"bluesky"}, Source: "test", Status: status,
			Targets: []Target{{Platform: "bluesky", Status: status}}}
		if err := st.SavePost(p); err != nil {
			t.Fatalf("SavePost %s: %v", id, err)
		}
	}
	mk("p-failed", "failed")
	mk("p-partial", "partial")
	mk("p-success", "success")
	mk("p-missed", "missed")

	ids, err := st.PostsNeedingRetry()
	if err != nil {
		t.Fatalf("PostsNeedingRetry: %v", err)
	}
	set := map[string]bool{}
	for _, id := range ids {
		set[id] = true
	}
	if !set["p-failed"] || !set["p-partial"] {
		t.Errorf("failed/partial should be candidates: %v", ids)
	}
	if set["p-success"] || set["p-missed"] {
		t.Errorf("success/missed must not be candidates: %v", ids)
	}

	// Mark the failed target given up → it drops out of the candidate set.
	gp, _ := st.GetPost("p-failed")
	tid := gp.Targets[0].ID
	at := time.Now().UTC()
	set1, err := st.MarkTargetGaveUp(tid, at)
	if err != nil {
		t.Fatalf("MarkTargetGaveUp: %v", err)
	}
	if !set1 {
		t.Error("first MarkTargetGaveUp should report it set the flag")
	}
	set2, _ := st.MarkTargetGaveUp(tid, at) // idempotent guard
	if set2 {
		t.Error("second MarkTargetGaveUp should report no-op (already set)")
	}
	ids, _ = st.PostsNeedingRetry()
	for _, id := range ids {
		if id == "p-failed" {
			t.Error("given-up post should no longer be a candidate")
		}
	}
}

func TestSuccessClearsGaveUpAndRelayRetryCount(t *testing.T) {
	st := openTestStore(t)
	p := &Post{ID: "p-clear", CreatedAt: time.Now().UTC(), MasterText: "x",
		Platforms: []string{"bluesky"}, Source: "test", Status: "failed",
		Targets: []Target{{Platform: "bluesky", Status: "failed"}}}
	if err := st.SavePost(p); err != nil {
		t.Fatalf("SavePost: %v", err)
	}
	gp, _ := st.GetPost("p-clear")
	tid := gp.Targets[0].ID
	if _, err := st.MarkTargetGaveUp(tid, time.Now().UTC()); err != nil {
		t.Fatalf("MarkTargetGaveUp: %v", err)
	}
	// A successful later attempt clears the give-up flag.
	if err := st.AppendTargetAttempt(tid, "success", "", "rid", "https://x/1", 10, "", "", nil, ""); err != nil {
		t.Fatalf("AppendTargetAttempt: %v", err)
	}
	gp, _ = st.GetPost("p-clear")
	if gp.Targets[0].GaveUpAt != nil {
		t.Error("a successful attempt must clear gave_up_at")
	}

	// Relay retry_count bumps on UpdateRelayStatus.
	if err := st.AppendTargetAttempt(tid, "failed", "x", "", "", 1, "", "",
		[]RelayState{{URL: "wss://r1", Status: "failed"}}, "{}"); err != nil {
		t.Fatalf("seed relay: %v", err)
	}
	if err := st.UpdateRelayStatus(tid, "wss://r1", "failed", "still down"); err != nil {
		t.Fatalf("UpdateRelayStatus: %v", err)
	}
	gp, _ = st.GetPost("p-clear")
	var rc int
	for _, r := range gp.Targets[0].Relays {
		if r.URL == "wss://r1" {
			rc = r.RetryCount
		}
	}
	if rc != 1 {
		t.Errorf("relay retry_count = %d, want 1 after one UpdateRelayStatus", rc)
	}
}

func TestAttentionFilterAndCount(t *testing.T) {
	st := openTestStore(t)
	mk := func(id, status string) {
		p := &Post{ID: id, CreatedAt: time.Now().UTC(), MasterText: "x",
			Platforms: []string{"bluesky"}, Source: "test", Status: status,
			Targets: []Target{{Platform: "bluesky", Status: status}}}
		if err := st.SavePost(p); err != nil {
			t.Fatalf("SavePost: %v", err)
		}
	}
	mk("a1", "failed")
	mk("a2", "partial")
	mk("a3", "success")
	mk("a4", "missed")

	got, err := st.ListPostsFiltered(PostFilter{Status: "attention", Limit: 50})
	if err != nil {
		t.Fatalf("ListPostsFiltered: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("attention filter returned %d, want 2 (failed+partial)", len(got))
	}
	n, err := st.AttentionCount()
	if err != nil {
		t.Fatalf("AttentionCount: %v", err)
	}
	if n != 2 {
		t.Errorf("AttentionCount = %d, want 2", n)
	}
}

func TestAppendTargetAttemptAndRecompute(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rec := &Post{
		ID: "p1", CreatedAt: time.Now().UTC(), Platforms: []string{"mastodon"}, Source: "web", Status: "failed",
		Targets: []Target{{Platform: "mastodon", Status: "failed", FinalText: "hi",
			Attempts: []Attempt{{AttemptNo: 1, Status: "failed", Error: "boom", AttemptedAt: time.Now()}}}},
	}
	if err := db.SavePost(rec); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetPost("p1")
	if err != nil {
		t.Fatal(err)
	}
	tid := got.Targets[0].ID

	if err := db.AppendTargetAttempt(tid, "success", "", "st1", "https://m/1", 42,
		`{"Authorization":"Bearer SECRET"}`, `{"ok":true}`, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.RecomputeStatus("p1"); err != nil {
		t.Fatal(err)
	}

	got, err = db.GetPost("p1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "success" {
		t.Errorf("post status = %q, want success", got.Status)
	}
	tg := got.Targets[0]
	if tg.Status != "success" || tg.RemoteID != "st1" || tg.RemoteURL != "https://m/1" || tg.AttemptCount != 2 {
		t.Errorf("target not updated: %+v", tg)
	}
	if len(tg.Attempts) != 2 || tg.Attempts[1].AttemptNo != 2 || tg.Attempts[1].Status != "success" {
		t.Errorf("attempt history wrong: %+v", tg.Attempts)
	}
	if rj := tg.Attempts[1].RequestJSON; rj == "" || strings.Contains(rj, "SECRET") {
		t.Errorf("request json not scrubbed: %q", rj)
	}
}
