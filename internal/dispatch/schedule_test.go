package dispatch

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/thread"
)

// newTestStore opens a real store.Store on a temp DB (Schedule/Fire need a real
// store, not a fake). Shared by schedule_test.go and scheduler_test.go.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestScheduleCreatesPendingPost(t *testing.T) {
	st := newTestStore(t)
	d := &Dispatcher{Store: st}
	at := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	rec, err := d.Schedule(context.Background(), PostSpec{
		MasterText: "hello", Platforms: []string{"bluesky"}, Source: "web",
		Overrides: map[string]Overrides{"bluesky": {Langs: []string{"en"}}},
	}, at)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != "scheduled" || rec.ScheduledAt == nil || !rec.ScheduledAt.Equal(at) {
		t.Fatalf("scheduled record wrong: %+v", rec)
	}
	got, _ := st.GetPost(rec.ID)
	if len(got.Targets) != 1 || got.Targets[0].Status != "scheduled" || got.Targets[0].FieldsJSON == "" {
		t.Errorf("pending target wrong: %+v", got.Targets)
	}
	if len(got.Targets[0].Attempts) != 0 {
		t.Errorf("pending post must have no attempts yet: %+v", got.Targets[0].Attempts)
	}
}

// A scheduled over-limit post must thread on fire exactly as the live preview
// (and an immediate post) would: the platform receives a reply-chain of
// within-limit segments, not one over-limit post that the platform rejects.
func TestScheduleSplitsOverLimitIntoChain(t *testing.T) {
	st := newTestStore(t)
	f := &fakeBsky{failAt: -1}
	d := &Dispatcher{Store: st, Bluesky: f}

	limit := thread.LimitFor("bluesky") // 300
	// Two paragraphs, each within the limit, together over it → 2 segments.
	long := strings.Repeat("a", limit-50) + "\n\n" + strings.Repeat("b", limit-50)
	at := time.Now().Add(-time.Minute).UTC()
	rec, err := d.Schedule(context.Background(), PostSpec{
		MasterText: long, Platforms: []string{"bluesky"}, Source: "web",
	}, at)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := d.Fire(context.Background(), rec.ID); err != nil {
		t.Fatal(err)
	}

	if len(f.calls) < 2 {
		t.Fatalf("over-limit scheduled post should thread into >=2 segments, got %d call(s)", len(f.calls))
	}
	for i, c := range f.calls {
		if len(c.text) > limit { // ASCII test text: byte length == grapheme count
			t.Errorf("segment %d posted %d chars, over the %d-char bluesky limit", i, len(c.text), limit)
		}
	}
	if got, _ := st.GetPost(rec.ID); got.Status != "success" {
		t.Errorf("fired scheduled post status = %q, want success", got.Status)
	}
}
