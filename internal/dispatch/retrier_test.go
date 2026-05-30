package dispatch

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/store"
)

// openDispatchStore opens a real temp-file store for dispatch-package tests
// (the store package has no in-memory constructor; external packages use Open).
func openDispatchStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// fakeAlerter records Alert calls.
type fakeAlerter struct{ calls []string }

func (f *fakeAlerter) Alert(_ context.Context, summary, _ string) error {
	f.calls = append(f.calls, summary)
	return nil
}

func TestRetrierRetriesDueFailedTarget(t *testing.T) {
	st := openDispatchStore(t)
	// A failed bluesky target, one attempt made 10 minutes ago → due (base 2m).
	p := &store.Post{ID: "r1", CreatedAt: time.Now().UTC(), MasterText: "hi",
		Platforms: []string{"bluesky"}, Source: "test", Status: "failed",
		Targets: []store.Target{{Platform: "bluesky", Status: "failed"}}}
	if err := st.SavePost(p); err != nil {
		t.Fatalf("SavePost: %v", err)
	}
	gp, _ := st.GetPost("r1")
	tid := gp.Targets[0].ID
	_ = st.AppendTargetAttempt(tid, "failed", "boom", "", "", 1, "", "", nil, "")

	d := &Dispatcher{Store: st} // no platform adapters → runPlatform returns failed
	now := time.Now().UTC().Add(10 * time.Minute)
	r := &Retrier{disp: d, notifier: &fakeAlerter{}, enabled: true,
		maxAttempts: 6, base: 2 * time.Minute, max: time.Hour,
		now: func() time.Time { return now }}

	r.runDue(context.Background())

	gp, _ = st.GetPost("r1")
	if gp.Targets[0].AttemptCount < 2 {
		t.Errorf("expected a retry attempt (count>=2), got %d", gp.Targets[0].AttemptCount)
	}
}

func TestRetrierSkipsNotDue(t *testing.T) {
	st := openDispatchStore(t)
	p := &store.Post{ID: "r2", CreatedAt: time.Now().UTC(), MasterText: "hi",
		Platforms: []string{"bluesky"}, Source: "test", Status: "failed",
		Targets: []store.Target{{Platform: "bluesky", Status: "failed"}}}
	_ = st.SavePost(p)
	gp, _ := st.GetPost("r2")
	tid := gp.Targets[0].ID
	_ = st.AppendTargetAttempt(tid, "failed", "boom", "", "", 1, "", "", nil, "")

	d := &Dispatcher{Store: st}
	now := time.Now().UTC().Add(30 * time.Second) // < base 2m → not due
	r := &Retrier{disp: d, notifier: &fakeAlerter{}, enabled: true,
		maxAttempts: 6, base: 2 * time.Minute, max: time.Hour,
		now: func() time.Time { return now }}
	r.runDue(context.Background())
	gp, _ = st.GetPost("r2")
	if gp.Targets[0].AttemptCount != 1 {
		t.Errorf("not-due target must not be retried, count=%d", gp.Targets[0].AttemptCount)
	}
}

func TestBackoff(t *testing.T) {
	base, max := 2*time.Minute, 1*time.Hour
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 2 * time.Minute},  // clamped up to attempt 1
		{1, 2 * time.Minute},  // base
		{2, 4 * time.Minute},
		{3, 8 * time.Minute},
		{4, 16 * time.Minute},
		{5, 32 * time.Minute},
		{6, 1 * time.Hour},    // 64m clamped to max
		{20, 1 * time.Hour},    // far past cap, no overflow
		{100, 1 * time.Hour},   // int64 overflow boundary — must still return max, not negative
	}
	for _, c := range cases {
		if got := backoff(c.attempt, base, max); got != c.want {
			t.Errorf("backoff(%d) = %v, want %v", c.attempt, got, c.want)
		}
	}
}
