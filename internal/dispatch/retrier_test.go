package dispatch

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/accounts"
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
	// The test accounts of the types tests name, so posts naming them save.
	for _, typ := range []string{"nostr", "mastodon", "bluesky", "threads", "stub", "stub2", "src"} {
		if err := ensureAccount(db, acct(typ), typ); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// retryingDispatcher is an engine with the default retry policy whose
// clock reads now.
func retryingDispatcher(st *store.Store, al Notifier, now time.Time) *Dispatcher {
	return &Dispatcher{Store: st, Alerter: al, Clock: func() time.Time { return now },
		Policy: RetryPolicy{Enabled: true, MaxAttempts: 6, Base: 2 * time.Minute, Max: time.Hour}}
}

// seedTarget saves a one-target post whose target already has n failed
// attempts (made now), with optional relay states and a signed event.
func seedTarget(t *testing.T, st *store.Store, id, plat, status string, n int, relays []store.RelayState, signed string) {
	t.Helper()
	tg := store.Target{Platform: plat, Status: status, Relays: relays, SignedEventJSON: signed}
	for i := 1; i <= n; i++ {
		tg.Attempts = append(tg.Attempts, store.Attempt{AttemptNo: i, Status: status, Error: "boom", AttemptedAt: time.Now().UTC()})
	}
	p := &store.Post{ID: id, CreatedAt: time.Now().UTC(), MasterText: "hi", Platforms: []string{plat},
		Source: "test", Status: status, Targets: []store.Target{tg}}
	if err := saveWithAccounts(st, p); err != nil {
		t.Fatalf("SavePost: %v", err)
	}
}

// fakeAlerter records Alert calls.
type fakeAlerter struct{ calls, bodies []string }

func (f *fakeAlerter) Alert(_ context.Context, summary, body string) error {
	f.calls = append(f.calls, summary)
	f.bodies = append(f.bodies, body)
	return nil
}

func TestRetrierRetriesDueFailedTarget(t *testing.T) {
	st := openDispatchStore(t)
	// A failed bluesky target, one attempt made 10 minutes ago → due (base 2m).
	seedTarget(t, st, "r1", "bluesky", "failed", 1, nil, "")

	// No platform adapters → the retry fails too, but is attempted.
	d := retryingDispatcher(st, &fakeAlerter{}, time.Now().UTC().Add(10*time.Minute))
	d.passes(context.Background())
	d.runDue(context.Background())

	gp, _ := st.GetPost("r1")
	if gp.Targets[0].AttemptCount < 2 {
		t.Errorf("expected a retry attempt (count>=2), got %d", gp.Targets[0].AttemptCount)
	}
}

func TestRetrierSkipsNotDue(t *testing.T) {
	st := openDispatchStore(t)
	seedTarget(t, st, "r2", "bluesky", "failed", 1, nil, "")

	d := retryingDispatcher(st, &fakeAlerter{}, time.Now().UTC().Add(30*time.Second)) // < base 2m → not due
	d.passes(context.Background())
	d.runDue(context.Background())
	gp, _ := st.GetPost("r2")
	if gp.Targets[0].AttemptCount != 1 {
		t.Errorf("not-due target must not be retried, count=%d", gp.Targets[0].AttemptCount)
	}
}

func TestRetrierGivesUpAndAlertsOnce(t *testing.T) {
	st := openDispatchStore(t)
	// attempt_count already at the cap (6) — all failures.
	seedTarget(t, st, "g1", "bluesky", "failed", 6, nil, "")
	al := &fakeAlerter{}
	d := retryingDispatcher(st, al, time.Now().UTC().Add(2*time.Hour)) // well past any backoff
	r := struct{ runDue func(context.Context) }{func(ctx context.Context) { d.passes(ctx); d.runDue(ctx) }}

	r.runDue(context.Background()) // crosses the cap → gives up + alerts
	gp, _ := st.GetPost("g1")
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
	// One relay ok, one failed → target partial; store a signed event so
	// RetryRelay has something to rebroadcast.
	seedTarget(t, st, "rl1", "nostr", "partial", 1, []store.RelayState{
		{URL: "wss://ok", Status: "ok"},
		{URL: "wss://down", Status: "failed"},
	}, `{"id":"abc"}`)

	// fakeNostr lets RetryRelay "succeed" without a network.
	d := &Dispatcher{Registry: legacyAccounts(legacy{Nostr: fakeRebroadcaster{ok: true}}), Store: st}
	now := time.Now().UTC().Add(10 * time.Minute)
	r := &Retrier{disp: d, notifier: &fakeAlerter{}, enabled: true, maxAttempts: 6,
		base: 2 * time.Minute, max: time.Hour, now: func() time.Time { return now }}
	r.runDue(context.Background())

	gp, _ := st.GetPost("rl1")
	for _, rl := range gp.Targets[0].Relays {
		if rl.URL == "wss://down" && rl.Status != "ok" {
			t.Errorf("down relay should have been rebroadcast to ok, got %q", rl.Status)
		}
	}
}

// With no account that can deliver to relays, a relay retry waits: no
// retry is counted, nothing is logged as an error, and the wait is logged
// when it starts and then every quarter hour, not on every tick.
func TestRetrierRelayRetryWaitsForAnAccount(t *testing.T) {
	st := openDispatchStore(t)
	seedTarget(t, st, "rl3", "nostr", "partial", 1, []store.RelayState{
		{URL: "wss://ok3", Status: "ok"},
		{URL: "wss://down3", Status: "failed"},
	}, `{"id":"w"}`)
	reg := legacyAccounts(legacy{Nostr: fakeRebroadcaster{ok: true}})
	reg.set(acct("nostr"), accounts.Locked, "")
	d := &Dispatcher{Registry: reg, Store: st}
	now := time.Now().UTC().Add(10 * time.Minute)
	r := &Retrier{disp: d, notifier: &fakeAlerter{}, enabled: true, maxAttempts: 6,
		base: 2 * time.Minute, max: time.Hour, now: func() time.Time { return now }}
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)
	for range 31 { // half an hour of one-minute ticks
		r.runDue(context.Background())
		now = now.Add(time.Minute)
	}
	gp, _ := st.GetPost("rl3")
	if rl := gp.Targets[0].Relays[1]; rl.Status != "failed" || rl.RetryCount != 0 || rl.GaveUpAt != nil {
		t.Fatalf("relay while waiting = %+v", rl)
	}
	if n, e := strings.Count(logs.String(), "relay retries wait"), strings.Count(logs.String(), "level=ERROR"); n != 3 || e != 0 {
		t.Errorf("logged the wait %d times (want 3) and %d errors:\n%s", n, e, logs.String())
	}
	reg.set(acct("nostr"), accounts.Connected, "")
	r.runDue(context.Background())
	gp, _ = st.GetPost("rl3")
	if rl := gp.Targets[0].Relays[1]; rl.Status != "ok" || !strings.Contains(logs.String(), "no relay retry waits") {
		t.Errorf("after the account is back: relay %+v; logs:\n%s", rl, logs.String())
	}
}

func TestRetrierRelayLevelSkipsNotDue(t *testing.T) {
	st := openDispatchStore(t)
	// Capture t0 right before seeding so we can place now() reliably within the
	// 2-minute backoff window regardless of how long seeding takes.
	t0 := time.Now().UTC()
	seedTarget(t, st, "rl2", "nostr", "partial", 1, []store.RelayState{
		{URL: "wss://ok2", Status: "ok"},
		{URL: "wss://down2", Status: "failed"},
	}, `{"id":"xyz"}`)

	// fakeRebroadcaster{ok:true}: if the down relay were (wrongly) retried it
	// would flip to "ok"; the assertion that it stays "failed" proves the gate.
	d := &Dispatcher{Registry: legacyAccounts(legacy{Nostr: fakeRebroadcaster{ok: true}}), Store: st}
	// Only 30s after seed — well inside the 2m base backoff for retry_count=0.
	now := t0.Add(30 * time.Second)
	r := &Retrier{disp: d, notifier: &fakeAlerter{}, enabled: true, maxAttempts: 6,
		base: 2 * time.Minute, max: time.Hour, now: func() time.Time { return now }}
	r.runDue(context.Background())

	gp, _ := st.GetPost("rl2")
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
