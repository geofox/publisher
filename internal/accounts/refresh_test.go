package accounts

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/drivers"
	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/threads"
)

// refreshAcct is a fake account whose credentials need renewing.
type refreshAcct struct {
	*fakeAcct
	mu        sync.Mutex
	next, exp time.Time
	err       error
	calls     int
}

func (a *refreshAcct) NextRefresh(time.Time) time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.next
}
func (a *refreshAcct) CredentialsExpireAt() time.Time { a.mu.Lock(); defer a.mu.Unlock(); return a.exp }
func (a *refreshAcct) Refresh(context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	return a.err
}

type alerts struct {
	mu   sync.Mutex
	msgs []string
}

func (a *alerts) Alert(_ context.Context, summary, body string) error {
	a.mu.Lock()
	a.msgs = append(a.msgs, summary+": "+body)
	a.mu.Unlock()
	return nil
}

func (a *alerts) count() int { a.mu.Lock(); defer a.mu.Unlock(); return len(a.msgs) }

func refreshHarness(t *testing.T, ra *refreshAcct) (*harness, *alerts) {
	al := &alerts{}
	h := newHarness(t, []store.Account{row("acc_t", "threads", "env", "default", 0, true)}, []EnvAccount{env("threads", "default")},
		func(o *Options) {
			o.Alerter = al
			o.Open = func(_ context.Context, typ string, req destination.OpenRequest) (destination.Account, error) {
				ra.fakeAcct = &fakeAcct{id: req.AccountID, typ: typ, authOK: &yes}
				return ra, nil
			}
		})
	return h, al
}

// A failing refresh keeps the account usable, alerts at most once a day
// and backs off instead of asking the network every minute.
func TestRefreshFailureBacksOffAndAlertsDaily(t *testing.T) {
	ra := &refreshAcct{err: errors.New("threads refresh: status 400: bad token")}
	h, al := refreshHarness(t, ra)
	now := h.clock.Now()
	ra.next, ra.exp = now.Add(-time.Minute), now.Add(30*24*time.Hour)

	h.RefreshDue(context.Background())
	h.RefreshDue(context.Background())
	if ra.calls != 1 || al.count() != 1 {
		t.Fatalf("%d refreshes, %d alerts; want 1 and 1", ra.calls, al.count())
	}
	if e, _ := h.Get("acc_t"); e.Status != Connected {
		t.Errorf("a failed refresh changed the status to %s", e.Status)
	}
	h.clock.Add(refreshRetryFirst)
	h.RefreshDue(context.Background())
	if ra.calls != 2 || al.count() != 1 {
		t.Errorf("after an hour: %d refreshes, %d alerts; want 2 and 1", ra.calls, al.count())
	}
	h.clock.Add(alertEvery)
	h.RefreshDue(context.Background())
	if ra.calls != 3 || al.count() != 2 {
		t.Errorf("after a day: %d refreshes, %d alerts; want 3 and 2", ra.calls, al.count())
	}
	if !strings.Contains(al.msgs[0], "status 400") {
		t.Errorf("alert = %q", al.msgs[0])
	}
}

// Ported from TestNearExpiryAlertWhenTooYoungToRefresh: credentials close
// to expiring that can't be renewed yet alert, once a day.
func TestExpiryAlert(t *testing.T) {
	ra := &refreshAcct{}
	h, al := refreshHarness(t, ra)
	now := h.clock.Now()
	ra.next, ra.exp = now.Add(time.Hour), now.Add(5*24*time.Hour)
	h.RefreshDue(context.Background())
	h.RefreshDue(context.Background())
	if ra.calls != 0 || al.count() != 1 || !strings.Contains(al.msgs[0], "expire") {
		t.Fatalf("%d refreshes, alerts %v", ra.calls, al.msgs)
	}
	ra.exp = now.Add(30 * 24 * time.Hour)
	h.clock.Add(alertEvery)
	h.RefreshDue(context.Background())
	if al.count() != 1 {
		t.Errorf("alerted for credentials a month from expiry: %v", al.msgs)
	}
}

// A successful refresh resets the backoff.
func TestRefreshSuccess(t *testing.T) {
	ra := &refreshAcct{}
	h, al := refreshHarness(t, ra)
	now := h.clock.Now()
	ra.next, ra.exp = now.Add(-time.Minute), now.Add(5*24*time.Hour)
	h.RefreshDue(context.Background())
	if ra.calls != 1 || al.count() != 0 {
		t.Errorf("%d refreshes, alerts %v", ra.calls, al.msgs)
	}
	if e := h.entries["acc_t"]; e.refresh.failures != 0 || !e.refresh.retryAt.IsZero() {
		t.Errorf("refresh state = %+v", e.refresh)
	}
}

// An account that can't send isn't refreshed.
func TestRefreshKeepsTurnedOffAccountAlive(t *testing.T) {
	ra := &refreshAcct{}
	h, _ := refreshHarness(t, ra)
	ra.next = h.clock.Now().Add(-time.Minute)
	r, _ := h.Get("acc_t")
	r.Row.Enabled = false
	r.Row.UpdatedAt = r.Row.UpdatedAt.Add(time.Second)
	if err := h.Update("acc_t", r.Row); err != nil {
		t.Fatal(err)
	}
	if en, _ := h.Get("acc_t"); en.Status != Disabled {
		t.Fatalf("status %s, want disabled", en.Status)
	}
	h.RefreshDue(context.Background())
	if ra.calls != 1 {
		t.Errorf("a turned-off account was refreshed %d times, want 1: its token would expire unseen", ra.calls)
	}
}

// The whole path with the real Threads driver and the 2.x token row: a
// database whose token expires in 5 days (refreshed long ago) is renewed
// on the first loop, and the new token and expiry land in threads_token
// with the seed hash kept.
func TestThreadsRefreshThroughLegacyState(t *testing.T) {
	drivers.Register()
	st, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if _, err := st.SeedEnvAccounts(ctx, store.DefaultOwner, []store.EnvAccount{{Type: threads.Type, Label: "Threads"}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.SaveThreadsToken("refreshed-before", now.Add(5*24*time.Hour), threads.SeedHash("envtok"), now.Add(-55*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var sentToken atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		sentToken.Store(r.URL.Query().Get("access_token"))
		_, _ = w.Write([]byte(`{"access_token":"fresh","expires_in":5184000}`))
	}))
	defer srv.Close()

	accts, err := drivers.EnvAccounts(func(k string) string {
		return map[string]string{"THREADS_ACCESS_TOKEN": "envtok"}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	var envs []EnvAccount
	for _, a := range accts {
		envs = append(envs, EnvAccount{Type: a.Type, EnvAccount: a.EnvAccount})
	}
	reg := New(Options{Store: st, Env: envs, State: drivers.LegacyState{Store: st}})
	if err := reg.Load(ctx); err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	list := reg.OfType(store.DefaultOwner, threads.Type)
	if len(list) != 1 || list[0].Status != Connected {
		t.Fatalf("threads accounts = %+v", list)
	}
	acct, release, err := reg.Acquire(list[0].Row.ID)
	if err != nil {
		t.Fatal(err)
	}
	acct.(*threads.Account).C.RefreshURL = srv.URL
	release()

	reg.RefreshDue(ctx)
	if calls.Load() != 1 || sentToken.Load() != "refreshed-before" {
		t.Fatalf("%d refresh calls with token %v; want the saved token renewed once", calls.Load(), sentToken.Load())
	}
	tok, err := st.GetThreadsToken()
	if err != nil {
		t.Fatal(err)
	}
	if tok.Token != "fresh" || tok.SeedHash != threads.SeedHash("envtok") || tok.ExpiresAt.Sub(now) < 59*24*time.Hour {
		t.Errorf("saved token = %+v", tok)
	}
	reg.RefreshDue(ctx)
	if calls.Load() != 1 {
		t.Error("refreshed again right after a refresh")
	}

	// A restart opens with the refreshed token, not the environment's.
	reg2 := New(Options{Store: st, Env: envs, State: drivers.LegacyState{Store: st}})
	if err := reg2.Load(ctx); err != nil {
		t.Fatal(err)
	}
	defer reg2.Close()
	acct2, release2, err := reg2.Acquire(list[0].Row.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer release2()
	if exp := acct2.(*threads.Account).CredentialsExpireAt(); exp.Sub(now) < 59*24*time.Hour {
		t.Errorf("after a restart the expiry is %v", exp)
	}
}

// A grant the network ends (Meta's 90 days) alerts within a week of its
// end, at most once a day, saying to connect again: renewing the token
// doesn't move it.
func TestGrantEndAlerts(t *testing.T) {
	ra := &refreshAcct{}
	h, al := refreshHarness(t, ra)
	apply := func(end time.Time) {
		t.Helper()
		l, release := h.leaseAny("acc_t")
		defer release()
		h.applyHealth(context.Background(), "acc_t", l, destination.Health{OK: true, Reachable: true, AuthOK: &yes, ReconnectBy: &end})
	}
	now := h.clock.Now()
	apply(now.Add(30 * 24 * time.Hour))
	if al.count() != 0 {
		t.Fatalf("a month ahead: %v", al.msgs)
	}
	apply(now.Add(6 * 24 * time.Hour))
	apply(now.Add(6 * 24 * time.Hour))
	if al.count() != 1 || !strings.Contains(al.msgs[0], "connect it again") {
		t.Fatalf("within a week: %v", al.msgs)
	}
	h.clock.Add(alertEvery)
	apply(now.Add(6 * 24 * time.Hour))
	if al.count() != 2 {
		t.Fatalf("a day later: %v", al.msgs)
	}
	if e, _ := h.Get("acc_t"); e.Health == nil || e.Health.ReconnectBy == nil {
		t.Fatalf("the entry's health: %+v", e.Health)
	}
}
