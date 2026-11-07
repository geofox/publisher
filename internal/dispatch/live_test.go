package dispatch

import (
	"context"
	"testing"
	"time"
)

// What the owner sets in the app applies at the next use: the retry
// switch and tries, with delays that hold when the environment had
// retries off, and the grace for late scheduled posts.
func TestPolicyFollowsLive(t *testing.T) {
	live := Live{}
	d := &Dispatcher{Policy: RetryPolicy{Enabled: true, MaxAttempts: 3, Base: time.Minute, Max: 10 * time.Minute},
		ScheduleGrace: time.Hour, Live: func() Live { return live }}
	if p := d.policy(); p.Enabled || p.MaxAttempts != 0 {
		t.Fatalf("off in the app: %+v", p)
	}
	live = Live{AutoRetry: true, MaxAttempts: 9, ScheduleGrace: 5 * time.Minute}
	if p := d.policy(); !p.Enabled || p.MaxAttempts != 9 || p.Base != time.Minute || p.Max != 10*time.Minute {
		t.Fatalf("on in the app: %+v", p)
	}
	now := time.Now()
	if d.grace() != 5*time.Minute || !d.missedBefore(now).Equal(now.Add(-5*time.Minute)) {
		t.Fatalf("grace %v", d.grace())
	}
	live.ScheduleGrace = 0
	if !d.missedBefore(now).IsZero() {
		t.Fatal("no grace: posts are missed")
	}
	// The environment had retries off (delays unchecked, zero): turned on
	// in the app, the delays fall back.
	d.Policy = RetryPolicy{}
	live.AutoRetry = true
	if p := d.policy(); p.Base != 2*time.Minute || p.Max != time.Hour {
		t.Fatalf("fallback delays: %+v", p)
	}
	d.Policy = RetryPolicy{Base: 3 * time.Hour, Max: time.Minute}
	if p := d.policy(); p.Base != 3*time.Hour || p.Max != 3*time.Hour {
		t.Fatalf("max below base: %+v", p)
	}
	// Without Live, the environment's.
	d.Live = nil
	if d.grace() != time.Hour {
		t.Fatalf("env grace %v", d.grace())
	}
}

// With Live, the retrier keeps ticking while retries are off (so turning
// them on needs no restart), and a tick while they are off does nothing.
func TestRetrierFollowsLive(t *testing.T) {
	on, tries := false, 4
	r := NewRetrier(nil, nil, false, 6, time.Minute, time.Hour, 5*time.Millisecond)
	r.Live = func() (bool, int) { return on, tries }
	if enabled, n := r.settings(); enabled || n != 4 || r.maxTries() != 4 {
		t.Fatalf("settings %v %d", enabled, n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	start := time.Now()
	r.Start(ctx) // a pass would use the nil dispatcher and panic
	if time.Since(start) < 50*time.Millisecond {
		t.Fatal("the retrier stopped while retries were off")
	}
	r.Live = nil
	start = time.Now()
	r.Start(context.Background()) // off in the environment, no Live: returns at once
	if time.Since(start) > time.Second {
		t.Fatal("no Live and off: still ticking")
	}
}

// The relay fan-out reads its tries at each delivery.
func TestFanoutFollowsLiveTries(t *testing.T) {
	st, cr, d := newFanoutTest(t)
	cr.fail = true
	_ = st.EnqueueFanout("p", 0, `{"id":"ev"}`, []string{"wss://a"})
	f := NewFanout(d, 6, time.Minute, time.Hour, 0)
	f.MaxAttempts = func() int { return 1 }
	f.runDue(context.Background())
	if jobs, _ := st.DueFanout(time.Now().Add(24*time.Hour), 10); len(jobs) != 0 {
		t.Fatalf("one try set, but the job was rescheduled: %+v", jobs)
	}
}
