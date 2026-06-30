package store

import (
	"path/filepath"
	"testing"
	"time"
)

func openFanoutStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "fanout.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestFanoutEnqueueAndDue(t *testing.T) {
	s := openFanoutStore(t)
	if err := s.EnqueueFanout("post1", `{"id":"ev1"}`, []string{"wss://a", "wss://b"}); err != nil {
		t.Fatalf("EnqueueFanout: %v", err)
	}
	now := time.Now()
	jobs, err := s.DueFanout(now.Add(time.Minute), 10)
	if err != nil {
		t.Fatalf("DueFanout: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("got %d due jobs, want 2", len(jobs))
	}
}

func TestFanoutMarkOKRemovesFromDue(t *testing.T) {
	s := openFanoutStore(t)
	_ = s.EnqueueFanout("post1", `{"id":"ev1"}`, []string{"wss://a"})
	jobs, _ := s.DueFanout(time.Now().Add(time.Minute), 10)
	if err := s.MarkFanoutOK(jobs[0].ID); err != nil {
		t.Fatalf("MarkFanoutOK: %v", err)
	}
	jobs, _ = s.DueFanout(time.Now().Add(time.Minute), 10)
	if len(jobs) != 0 {
		t.Fatalf("ok job still due: %d", len(jobs))
	}
}

func TestFanoutRetryDefersThenGiveUp(t *testing.T) {
	s := openFanoutStore(t)
	_ = s.EnqueueFanout("post1", `{"id":"ev1"}`, []string{"wss://a"})
	jobs, _ := s.DueFanout(time.Now().Add(time.Minute), 10)
	id := jobs[0].ID
	future := time.Now().Add(time.Hour)
	if err := s.MarkFanoutRetry(id, future); err != nil {
		t.Fatalf("MarkFanoutRetry: %v", err)
	}
	if jobs, _ = s.DueFanout(time.Now(), 10); len(jobs) != 0 {
		t.Fatalf("retried job due before next_attempt_at: %d", len(jobs))
	}
	if jobs, _ = s.DueFanout(future.Add(time.Minute), 10); len(jobs) != 1 || jobs[0].RetryCount != 1 {
		t.Fatalf("retried job not due after delay, or retry_count wrong: %+v", jobs)
	}
	if err := s.MarkFanoutGaveUp(id); err != nil {
		t.Fatalf("MarkFanoutGaveUp: %v", err)
	}
	if jobs, _ = s.DueFanout(future.Add(time.Hour), 10); len(jobs) != 0 {
		t.Fatalf("gave-up job still due: %d", len(jobs))
	}
}
