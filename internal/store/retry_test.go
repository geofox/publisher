package store

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
