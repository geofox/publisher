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

func TestRetrierGivesUpAndAlertsOnce(t *testing.T) {
	st := openDispatchStore(t)
	p := &store.Post{ID: "g1", CreatedAt: time.Now().UTC(), MasterText: "hi",
		Platforms: []string{"bluesky"}, Source: "test", Status: "failed",
		Targets: []store.Target{{Platform: "bluesky", Status: "failed"}}}
	_ = st.SavePost(p)
	gp, _ := st.GetPost("g1")
	tid := gp.Targets[0].ID
	// Drive attempt_count up to the cap (6) — all failures.
	for i := 0; i < 6; i++ {
		_ = st.AppendTargetAttempt(tid, "failed", "boom", "", "", 1, "", "", nil, "")
	}
	al := &fakeAlerter{}
	d := &Dispatcher{Store: st}
	now := time.Now().UTC().Add(2 * time.Hour) // well past any backoff
	r := &Retrier{disp: d, notifier: al, enabled: true, maxAttempts: 6,
		base: 2 * time.Minute, max: time.Hour, now: func() time.Time { return now }}

	r.runDue(context.Background()) // crosses the cap → gives up + alerts
	gp, _ = st.GetPost("g1")
	if gp.Targets[0].GaveUpAt == nil {
		t.Fatal("target should be marked given up at the cap")
	}
	if len(al.calls) != 1 {
		t.Fatalf("expected exactly 1 give-up alert, got %d", len(al.calls))
	}
	r.runDue(context.Background()) // second pass must not re-alert
	if len(al.calls) != 1 {
		t.Errorf("give-up alert must fire only once, got %d", len(al.calls))
	}
}

func TestRetrierRelayLevelRetry(t *testing.T) {
	st := openDispatchStore(t)
	p := &store.Post{ID: "rl1", CreatedAt: time.Now().UTC(), MasterText: "hi",
		Platforms: []string{"nostr"}, Source: "test", Status: "partial",
		Targets: []store.Target{{Platform: "nostr", Status: "partial"}}}
	_ = st.SavePost(p)
	gp, _ := st.GetPost("rl1")
	tid := gp.Targets[0].ID
	// One relay ok, one failed → target partial; store a signed event so
	// RetryRelay has something to rebroadcast.
	_ = st.AppendTargetAttempt(tid, "partial", "", "", "", 1, "", "",
		[]store.RelayState{
			{URL: "wss://ok", Status: "ok"},
			{URL: "wss://down", Status: "failed"},
		}, `{"id":"abc"}`)

	// fakeNostr lets RetryRelay "succeed" without a network.
	d := &Dispatcher{Store: st, Nostr: fakeRebroadcaster{ok: true}}
	now := time.Now().UTC().Add(10 * time.Minute)
	r := &Retrier{disp: d, notifier: &fakeAlerter{}, enabled: true, maxAttempts: 6,
		base: 2 * time.Minute, max: time.Hour, now: func() time.Time { return now }}
	r.runDue(context.Background())

	gp, _ = st.GetPost("rl1")
	for _, rl := range gp.Targets[0].Relays {
		if rl.URL == "wss://down" && rl.Status != "ok" {
			t.Errorf("down relay should have been rebroadcast to ok, got %q", rl.Status)
		}
	}
}

func TestRetrierRelayLevelSkipsNotDue(t *testing.T) {
	st := openDispatchStore(t)
	p := &store.Post{ID: "rl2", CreatedAt: time.Now().UTC(), MasterText: "hi",
		Platforms: []string{"nostr"}, Source: "test", Status: "partial",
		Targets: []store.Target{{Platform: "nostr", Status: "partial"}}}
	_ = st.SavePost(p)
	gp, _ := st.GetPost("rl2")
	tid := gp.Targets[0].ID
	// Capture t0 right before seeding so we can place now() reliably within the
	// 2-minute backoff window regardless of how long AppendTargetAttempt takes.
	t0 := time.Now().UTC()
	_ = st.AppendTargetAttempt(tid, "partial", "", "", "", 1, "", "",
		[]store.RelayState{
			{URL: "wss://ok2", Status: "ok"},
			{URL: "wss://down2", Status: "failed"},
		}, `{"id":"xyz"}`)

	// fakeRebroadcaster{ok:true}: if the down relay were (wrongly) retried it
	// would flip to "ok"; the assertion that it stays "failed" proves the gate.
	d := &Dispatcher{Store: st, Nostr: fakeRebroadcaster{ok: true}}
	// Only 30s after seed — well inside the 2m base backoff for retry_count=0.
	now := t0.Add(30 * time.Second)
	r := &Retrier{disp: d, notifier: &fakeAlerter{}, enabled: true, maxAttempts: 6,
		base: 2 * time.Minute, max: time.Hour, now: func() time.Time { return now }}
	r.runDue(context.Background())

	gp, _ = st.GetPost("rl2")
	for _, rl := range gp.Targets[0].Relays {
		if rl.URL == "wss://down2" && rl.Status != "failed" {
			t.Errorf("not-due relay must not be retried, got status %q", rl.Status)
		}
	}
}

type fakeRebroadcaster struct {
	NostrPoster // embed; nil — only RebroadcastToRelay is called in this test
	ok          bool
}

func (f fakeRebroadcaster) RebroadcastToRelay(_ context.Context, _, _ string) (bool, string) {
	return f.ok, ""
}

func TestBackoff(t *testing.T) {
	base, max := 2*time.Minute, 1*time.Hour
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 2 * time.Minute}, // clamped up to attempt 1
		{1, 2 * time.Minute}, // base
		{2, 4 * time.Minute},
		{3, 8 * time.Minute},
		{4, 16 * time.Minute},
		{5, 32 * time.Minute},
		{6, 1 * time.Hour},   // 64m clamped to max
		{20, 1 * time.Hour},  // far past cap, no overflow
		{100, 1 * time.Hour}, // int64 overflow boundary — must still return max, not negative
	}
	for _, c := range cases {
		if got := backoff(c.attempt, base, max); got != c.want {
			t.Errorf("backoff(%d) = %v, want %v", c.attempt, got, c.want)
		}
	}
}
