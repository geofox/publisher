package dispatch

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	gonostr "fiatjaf.com/nostr"
	"github.com/geofox/publisher/internal/store"
)

// countingRebroadcaster records each (relay) call and can be told to fail.
type countingRebroadcaster struct {
	fail  bool
	calls []string
}

func (c *countingRebroadcaster) RebroadcastToRelay(_ context.Context, _, relay string) (bool, string) {
	c.calls = append(c.calls, relay)
	if c.fail {
		return false, "rate-limited"
	}
	return true, ""
}

type stubNostrFanout struct{ cr *countingRebroadcaster }

func (s stubNostrFanout) PublishText(context.Context, string, *int, []gonostr.Tag, *ReplyRef) (TargetResult, error) {
	return TargetResult{}, nil
}
func (s stubNostrFanout) RebroadcastToRelay(ctx context.Context, ev, relay string) (bool, string) {
	return s.cr.RebroadcastToRelay(ctx, ev, relay)
}
func (s stubNostrFanout) Repost(context.Context, string, string, int, string) (TargetResult, error) {
	return TargetResult{}, nil
}
func (s stubNostrFanout) Quote(context.Context, string, string, string, string, []gonostr.Tag) (TargetResult, error) {
	return TargetResult{}, nil
}

func newFanoutTest(t *testing.T) (*store.Store, *countingRebroadcaster, *Dispatcher) {
	st, err := store.Open(filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cr := &countingRebroadcaster{}
	return st, cr, &Dispatcher{Store: st, Nostr: stubNostrFanout{cr}}
}

func TestFanoutWorkerDeliversAndMarksOK(t *testing.T) {
	st, cr, d := newFanoutTest(t)
	_ = st.EnqueueFanout("p", `{"id":"ev"}`, []string{"wss://a", "wss://b"})
	f := NewFanout(d, true, 6, time.Minute, time.Hour, time.Second) // different relays → both delivered in one pass
	f.runDue(context.Background())
	if len(cr.calls) != 2 {
		t.Fatalf("delivered %d, want 2", len(cr.calls))
	}
	if jobs, _ := st.DueFanout(time.Now().Add(time.Minute), 10); len(jobs) != 0 {
		t.Fatalf("jobs still due after success: %d", len(jobs))
	}
}

func TestFanoutWorkerPacesPerRelay(t *testing.T) {
	st, cr, d := newFanoutTest(t)
	// Two events to the SAME relay; per-relay pacing sends one per pass.
	_ = st.EnqueueFanout("p", `{"id":"ev1"}`, []string{"wss://a"})
	_ = st.EnqueueFanout("p", `{"id":"ev2"}`, []string{"wss://a"})
	f := NewFanout(d, true, 6, time.Minute, time.Hour, time.Minute)
	f.runDue(context.Background())
	if len(cr.calls) != 1 {
		t.Fatalf("same-relay pass delivered %d, want 1 (paced)", len(cr.calls))
	}
}

func TestFanoutWorkerBacksOffOnFailure(t *testing.T) {
	st, cr, d := newFanoutTest(t)
	cr.fail = true
	_ = st.EnqueueFanout("p", `{"id":"ev"}`, []string{"wss://a"})
	f := NewFanout(d, true, 6, time.Minute, time.Hour, 0)
	f.runDue(context.Background())
	// Failed → deferred, not due now.
	if jobs, _ := st.DueFanout(time.Now(), 10); len(jobs) != 0 {
		t.Fatalf("failed job due immediately: %d", len(jobs))
	}
	// Due again after backoff.
	if jobs, _ := st.DueFanout(time.Now().Add(2*time.Minute), 10); len(jobs) != 1 || jobs[0].RetryCount != 1 {
		t.Fatalf("failed job not re-scheduled: %+v", jobs)
	}
}
