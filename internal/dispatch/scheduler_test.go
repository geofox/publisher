package dispatch

import (
	"context"
	"testing"
	"time"

	gonostr "fiatjaf.com/nostr"
)

type fakeNotifier struct{ msgs []string }

func (f *fakeNotifier) Alert(ctx context.Context, summary, body string) error {
	f.msgs = append(f.msgs, summary)
	return nil
}

// stubNostr is a NostrPoster that always succeeds (so a scheduled nostr post
// fires without needing media bytes). newTestStore is defined in schedule_test.go.
type stubNostr struct{ calls *int }

func (s stubNostr) PublishText(ctx context.Context, text string, pow *int, imetas []gonostr.Tag) (TargetResult, error) {
	if s.calls != nil {
		*s.calls++
	}
	return TargetResult{Platform: "nostr", Status: "success", RemoteID: "ev1"}, nil
}
func (stubNostr) RebroadcastToRelay(ctx context.Context, signedEventJSON, relayURL string) (bool, string) {
	return true, ""
}

func TestOverdue(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	grace := 2 * time.Hour
	if overdue(now, now.Add(-time.Minute), grace) {
		t.Error("1m overdue is within grace")
	}
	if !overdue(now, now.Add(-3*time.Hour), grace) {
		t.Error("3h overdue exceeds 2h grace")
	}
}

func TestSchedulerFiresDue(t *testing.T) {
	st := newTestStore(t)
	d := &Dispatcher{Store: st, Nostr: stubNostr{}}
	at := time.Now().Add(-time.Minute).UTC()
	rec, err := d.Schedule(context.Background(), PostSpec{MasterText: "x", Platforms: []string{"nostr"}, Source: "web"}, at)
	if err != nil {
		t.Fatal(err)
	}

	fn := &fakeNotifier{}
	now := time.Now().UTC()
	s := NewScheduler(d, fn, 2*time.Hour)
	s.now = func() time.Time { return now }
	s.runDue(context.Background())

	got, _ := st.GetPost(rec.ID)
	if got.Status != "success" {
		t.Errorf("due post should have fired to success, status = %q", got.Status)
	}
	if len(fn.msgs) != 0 {
		t.Errorf("no missed-alert expected for an in-grace fire, got %v", fn.msgs)
	}
}

// Fire only dispatches 'scheduled' targets, so re-firing an already-fired post
// (e.g. after a crash that left it 'sending' → reset → re-fire) does not
// re-publish targets that already succeeded.
func TestFireSkipsAlreadyDispatched(t *testing.T) {
	st := newTestStore(t)
	n := 0
	d := &Dispatcher{Store: st, Nostr: stubNostr{calls: &n}}
	at := time.Now().Add(-time.Minute).UTC()
	rec, err := d.Schedule(context.Background(), PostSpec{MasterText: "x", Platforms: []string{"nostr"}, Source: "web"}, at)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Fire(context.Background(), rec.ID); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("first fire publish calls = %d, want 1", n)
	}
	if _, err := d.Fire(context.Background(), rec.ID); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("second fire re-published an already-dispatched target: calls = %d, want 1", n)
	}
}

func TestSchedulerMarksMissed(t *testing.T) {
	st := newTestStore(t)
	d := &Dispatcher{Store: st, Nostr: stubNostr{}}
	at := time.Now().Add(-5 * time.Hour).UTC() // overdue beyond 2h grace
	rec, err := d.Schedule(context.Background(), PostSpec{MasterText: "x", Platforms: []string{"nostr"}, Source: "web"}, at)
	if err != nil {
		t.Fatal(err)
	}

	fn := &fakeNotifier{}
	now := time.Now().UTC()
	s := NewScheduler(d, fn, 2*time.Hour)
	s.now = func() time.Time { return now }
	s.runDue(context.Background())

	got, _ := st.GetPost(rec.ID)
	if got.Status != "missed" {
		t.Errorf("status = %q, want missed", got.Status)
	}
	if len(fn.msgs) != 1 {
		t.Errorf("expected one missed alert, got %v", fn.msgs)
	}
}
