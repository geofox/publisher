package dispatch

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	gonostr "fiatjaf.com/nostr"
	"github.com/geofox/publisher/internal/store"
)

// fanoutNostr returns pending relays + a signed event per post, simulating
// primary-fanout mode.
type fanoutNostr struct{ n int }

func (f *fanoutNostr) PublishText(ctx context.Context, text string, pow *int, imetas []gonostr.Tag, replyTo *ReplyRef) (legacyResult, error) {
	f.n++
	return legacyResult{
		Platform: "nostr", Status: "success", RemoteID: "ev" + itoa(f.n),
		SignedEventJSON: `{"id":"ev` + itoa(f.n) + `"}`,
		PendingRelays:   []string{"wss://nos.lol", "wss://relay.damus.io"},
	}, nil
}
func (f *fanoutNostr) RebroadcastToRelay(context.Context, string, string) (bool, string) {
	return true, ""
}
func (f *fanoutNostr) Repost(context.Context, string, string, int, string) (legacyResult, error) {
	return legacyResult{}, nil
}
func (f *fanoutNostr) Quote(context.Context, string, string, string, string, []gonostr.Tag) (legacyResult, error) {
	return legacyResult{}, nil
}

// fanoutNostrFail simulates a primary relay that rejects the event (Status:
// "failed") but still returns a signed event and pending relays — these must
// NOT be enqueued for fan-out.
type fanoutNostrFail struct{ n int }

func (f *fanoutNostrFail) PublishText(ctx context.Context, text string, pow *int, imetas []gonostr.Tag, replyTo *ReplyRef) (legacyResult, error) {
	f.n++
	return legacyResult{
		Platform: "nostr", Status: "failed", RemoteID: "",
		SignedEventJSON: `{"id":"ev` + itoa(f.n) + `"}`,
		PendingRelays:   []string{"wss://nos.lol", "wss://relay.damus.io"},
	}, nil
}
func (f *fanoutNostrFail) RebroadcastToRelay(context.Context, string, string) (bool, string) {
	return false, "rejected"
}
func (f *fanoutNostrFail) Repost(context.Context, string, string, int, string) (legacyResult, error) {
	return legacyResult{}, nil
}
func (f *fanoutNostrFail) Quote(context.Context, string, string, string, string, []gonostr.Tag) (legacyResult, error) {
	return legacyResult{}, nil
}

func TestRunChainEnqueuesFanoutPerSegment(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	d := &Dispatcher{Registry: legacyAccounts(legacy{Nostr: &fanoutNostr{}}), Store: st}
	imgs := []Img{{BlossomURL: "https://b/x"}}
	o := d.runChain(context.Background(), "nostr", "a\n---\nb", Overrides{}, imgs, false, []int{0}, nil, "post1")
	if o.Status != "success" {
		t.Fatalf("status=%s", o.Status)
	}
	// 2 segments × 2 pending relays = 4 fan-out rows.
	jobs, err := st.DueFanout(time.Now().Add(time.Minute), 100)
	if err != nil {
		t.Fatalf("DueFanout: %v", err)
	}
	if len(jobs) != 4 {
		t.Fatalf("got %d fan-out rows, want 4 (2 segments × 2 relays)", len(jobs))
	}
}

func TestRunChainFailedPrimaryEnqueuesNoFanout(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	d := &Dispatcher{Registry: legacyAccounts(legacy{Nostr: &fanoutNostrFail{}}), Store: st}
	imgs := []Img{{BlossomURL: "https://b/x"}}
	// Single-segment publish that fails at the primary relay.
	d.runChain(context.Background(), "nostr", "hello", Overrides{}, imgs, false, []int{0}, nil, "post2")
	// A failed primary must not enqueue any fan-out rows.
	jobs, err := st.DueFanout(time.Now().Add(time.Minute), 100)
	if err != nil {
		t.Fatalf("DueFanout: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("got %d fan-out rows for failed publish, want 0", len(jobs))
	}
}
