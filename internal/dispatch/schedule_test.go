package dispatch

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/store"
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
