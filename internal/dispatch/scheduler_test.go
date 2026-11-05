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

func (s stubNostr) PublishText(ctx context.Context, text string, pow *int, imetas []gonostr.Tag, replyTo *ReplyRef) (legacyResult, error) {
	if s.calls != nil {
		*s.calls++
	}
	return legacyResult{Platform: "nostr", Status: "success", RemoteID: "ev1"}, nil
}
func (stubNostr) RebroadcastToRelay(ctx context.Context, signedEventJSON, relayURL string) (bool, string) {
	return true, ""
}
func (stubNostr) Repost(context.Context, string, string, int, string) (legacyResult, error) {
	return legacyResult{Platform: "nostr", Status: "success"}, nil
}
func (stubNostr) Quote(context.Context, string, string, string, string, []gonostr.Tag) (legacyResult, error) {
	return legacyResult{Platform: "nostr", Status: "success"}, nil
}

func TestEngineFiresDueScheduledPost(t *testing.T) {
	st := newTestStore(t)
	fn := &fakeNotifier{}
	d := &Dispatcher{Accounts: legacyAccounts(legacy{Nostr: stubNostr{}}), Store: st, Alerter: fn, ScheduleGrace: 2 * time.Hour}
	at := time.Now().Add(-time.Minute).UTC()
	rec, err := d.Schedule(context.Background(), PostSpec{MasterText: "x", Platforms: []string{"nostr"}, Source: "web"}, at)
	if err != nil {
		t.Fatal(err)
	}
	d.passes(context.Background())
	if n := d.runDue(context.Background()); n != 1 {
		t.Fatalf("ran %d items, want 1", n)
	}
	got, _ := st.GetPost(rec.ID)
	if got.Status != "success" {
		t.Errorf("due post should have fired to success, status = %q", got.Status)
	}
	if len(fn.msgs) != 0 {
		t.Errorf("no missed-alert expected for an in-grace fire, got %v", fn.msgs)
	}
}

func TestEngineLeavesFutureScheduledPost(t *testing.T) {
	st := newTestStore(t)
	n := 0
	d := &Dispatcher{Accounts: legacyAccounts(legacy{Nostr: stubNostr{calls: &n}}), Store: st}
	rec, err := d.Schedule(context.Background(), PostSpec{MasterText: "x", Platforms: []string{"nostr"}, Source: "web"},
		time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if ran := d.runDue(context.Background()); ran != 0 || n != 0 {
		t.Fatalf("a post due in an hour ran now: items %d, publishes %d", ran, n)
	}
	if got, _ := st.GetPost(rec.ID); got.Status != "scheduled" {
		t.Errorf("status = %q, want scheduled", got.Status)
	}
}

// Fire only dispatches 'scheduled' targets, so re-firing an already-fired post
// (e.g. after a crash that left it 'sending' → reset → re-fire) does not
// re-publish targets that already succeeded.
func TestFireSkipsAlreadyDispatched(t *testing.T) {
	st := newTestStore(t)
	n := 0
	d := &Dispatcher{Accounts: legacyAccounts(legacy{Nostr: stubNostr{calls: &n}}), Store: st}
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

func TestEngineMarksMissed(t *testing.T) {
	st := newTestStore(t)
	n := 0
	fn := &fakeNotifier{}
	d := &Dispatcher{Accounts: legacyAccounts(legacy{Nostr: stubNostr{calls: &n}}), Store: st, Alerter: fn, ScheduleGrace: 2 * time.Hour}
	at := time.Now().Add(-5 * time.Hour).UTC() // overdue beyond 2h grace
	rec, err := d.Schedule(context.Background(), PostSpec{MasterText: "x", Platforms: []string{"nostr"}, Source: "web"}, at)
	if err != nil {
		t.Fatal(err)
	}
	// Workers never claim a missed post, even before the pass marks it.
	if ran := d.runDue(context.Background()); ran != 0 || n != 0 {
		t.Fatalf("an overdue post was sent: items %d, publishes %d", ran, n)
	}
	d.passes(context.Background())
	d.passes(context.Background()) // alerted once
	got, _ := st.GetPost(rec.ID)
	if got.Status != "missed" || got.Targets[0].Status != "missed" {
		t.Errorf("status = %q / %q, want missed", got.Status, got.Targets[0].Status)
	}
	if len(fn.msgs) != 1 {
		t.Errorf("expected one missed alert, got %v", fn.msgs)
	}
	// A retry sends it.
	if _, err := d.Retry(context.Background(), rec.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetPost(rec.ID); got.Status != "success" || n != 1 {
		t.Errorf("retried missed post: status %q, publishes %d", got.Status, n)
	}
}
