package accounts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/drivers"
	"github.com/geofox/publisher/internal/store"
)

// clock is a settable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// fakeStore holds account rows and records identity writes.
type fakeStore struct {
	mu         sync.Mutex
	rows       []store.Account
	identities map[string][2]string
	dup        *store.DuplicateAccountError // what SetAccountIdentity answers, when set
}

func (s *fakeStore) AllAccounts(context.Context) ([]store.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.Account(nil), s.rows...), nil
}

func (s *fakeStore) SetAccountIdentity(_ context.Context, id, handle, remote string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.identities == nil {
		s.identities = map[string][2]string{}
	}
	if s.dup != nil {
		s.identities[id] = [2]string{handle, ""}
		return s.dup
	}
	s.identities[id] = [2]string{handle, remote}
	return nil
}

// fakeAcct is an open account whose check, identity and refresh the test
// controls.
type fakeAcct struct {
	id     string
	gen    int
	typ    string
	closed atomic.Int32

	mu       sync.Mutex
	authOK   *bool // the check's answer
	checks   int
	ident    destination.Identity
	identErr error
	idents   int
	caps     destination.Capabilities
	lastReq  destination.CheckRequest
}

func (a *fakeAcct) Manifest() destination.Manifest {
	d, _ := destination.Lookup(a.typ)
	return d.Manifest()
}
func (a *fakeAcct) Capabilities(context.Context) destination.Capabilities { return a.caps }
func (a *fakeAcct) Publish(context.Context, destination.PublishRequest) (destination.PublishResult, error) {
	return destination.PublishResult{}, nil
}
func (a *fakeAcct) Close() error { a.closed.Add(1); return nil }
func (a *fakeAcct) Check(_ context.Context, req destination.CheckRequest) destination.Health {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.checks++
	a.lastReq = req
	if a.authOK == nil {
		return destination.Health{Message: "the server did not answer in time"}
	}
	return destination.Health{OK: *a.authOK, Reachable: true, AuthOK: a.authOK, Message: map[bool]string{false: "refused"}[*a.authOK]}
}
func (a *fakeAcct) Identity(context.Context) (destination.Identity, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.idents++
	return a.ident, a.identErr
}
func (a *fakeAcct) setAuth(ok *bool) { a.mu.Lock(); a.authOK = ok; a.mu.Unlock() }
func (a *fakeAcct) checkCount() int  { a.mu.Lock(); defer a.mu.Unlock(); return a.checks }

var (
	yes, no = true, false
	t0      = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
)

func row(id, typ, source, key string, created time.Duration, enabled bool) store.Account {
	return store.Account{ID: id, OwnerID: store.DefaultOwner, Type: typ, Label: typ + " " + id, Source: source, EnvKey: key,
		Enabled: enabled, CreatedAt: t0.Add(created), UpdatedAt: t0.Add(created)}
}

func env(typ, key string) EnvAccount {
	return EnvAccount{Type: typ, EnvAccount: destination.EnvAccount{Key: key, Config: map[string]any{}, Secrets: map[string]string{}}}
}

// harness is a registry over fake accounts.
type harness struct {
	*Registry
	clock *clock
	store *fakeStore
	mu    sync.Mutex
	open  map[string][]*fakeAcct // every object opened, by account id
}

func newHarness(t *testing.T, rows []store.Account, envs []EnvAccount, opts ...func(*Options)) *harness {
	t.Helper()
	drivers.Register()
	h := &harness{clock: newClock(), store: &fakeStore{rows: rows}, open: map[string][]*fakeAcct{}}
	o := Options{Store: h.store, Env: envs, Now: h.clock.Now,
		Open: func(_ context.Context, typ string, req destination.OpenRequest) (destination.Account, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			a := &fakeAcct{id: req.AccountID, typ: typ, gen: len(h.open[req.AccountID]), authOK: &yes}
			h.open[req.AccountID] = append(h.open[req.AccountID], a)
			return a, nil
		}}
	for _, f := range opts {
		f(&o)
	}
	h.Registry = New(o)
	if err := h.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}

// live is the account's current object.
func (h *harness) live(id string) *fakeAcct {
	h.mu.Lock()
	defer h.mu.Unlock()
	objs := h.open[id]
	return objs[len(objs)-1]
}

func (h *harness) wait() { h.bgWait.Wait() }

func TestLoadStatusesAndOrder(t *testing.T) {
	h := newHarness(t, []store.Account{
		row("acc_n", "nostr", "env", "default", 0, true),
		row("acc_m", "mastodon", "env", "default", time.Hour, true),
		row("acc_b2", "bluesky", "env", "work", 3*time.Hour, false),
		row("acc_b1", "bluesky", "env", "default", 2*time.Hour, true),
		row("acc_t", "threads", "db", "", 0, true),
		row("acc_x", "myspace", "env", "default", 0, true),
	}, []EnvAccount{env("bluesky", "default"), env("bluesky", "work"), env("mastodon", "default")})

	var got []string
	for _, e := range h.List(store.DefaultOwner) {
		got = append(got, fmt.Sprintf("%s:%s:%d", e.Row.ID, e.Status, e.Order))
	}
	want := "acc_b1:connected:0 acc_b2:disabled:1 acc_m:connected:2 acc_t:locked:3 acc_n:not_configured:4 acc_x:not_configured:5"
	if strings.Join(got, " ") != want {
		t.Errorf("list =\n%s\nwant\n%s", strings.Join(got, " "), want)
	}
	if e, _ := h.Get("acc_n"); !strings.Contains(e.Reason, "NSEC_HEX") || strings.Contains(e.Reason, "OWNER_PUBKEY") {
		t.Errorf("not-configured reason %q should name the required variables only", e.Reason)
	}
	if e, _ := h.Get("acc_x"); !strings.Contains(e.Reason, "no driver") {
		t.Errorf("unknown type reason = %q", e.Reason)
	}
	if of := h.OfType(store.DefaultOwner, "bluesky"); len(of) != 2 || of[0].Row.ID != "acc_b1" {
		t.Errorf("OfType = %+v", of)
	}
	if len(h.List("someone_else")) != 0 {
		t.Error("another owner's list isn't empty")
	}
}

func TestAcquireUnavailable(t *testing.T) {
	h := newHarness(t, []store.Account{row("acc_b", "bluesky", "env", "default", 0, false), row("acc_n", "nostr", "env", "default", 0, true)},
		[]EnvAccount{env("bluesky", "default")})
	_, _, err := h.Acquire("acc_b")
	if u, ok := IsUnavailable(err); !ok || u.Status != Disabled {
		t.Errorf("disabled: err = %v", err)
	}
	_, _, err = h.Acquire("acc_n")
	if u, ok := IsUnavailable(err); !ok || u.Status != NotConfigured || !strings.Contains(err.Error(), "NSEC_HEX") {
		t.Errorf("not configured: err = %v", err)
	}
	if _, _, err := h.Acquire("acc_nope"); !errors.Is(err, ErrNoAccount) {
		t.Errorf("unknown: err = %v", err)
	}
}

// A send holding the old object keeps it open through a Reopen; it is
// closed when that send ends, and new sends get the new object.
func TestLeaseSwap(t *testing.T) {
	h := newHarness(t, []store.Account{row("acc_m", "mastodon", "env", "default", 0, true)}, []EnvAccount{env("mastodon", "default")})
	first, release, err := h.Acquire("acc_m")
	if err != nil {
		t.Fatal(err)
	}
	var changes []Change
	unsub := h.OnChange(func(c Change) { changes = append(changes, c) })
	defer unsub()
	if err := h.Reopen(context.Background(), "acc_m"); err != nil {
		t.Fatal(err)
	}
	if first.(*fakeAcct).closed.Load() != 0 {
		t.Fatal("the object was closed while a send held it")
	}
	second, release2, _ := h.Acquire("acc_m")
	if second == first {
		t.Fatal("Acquire after Reopen gave the old object")
	}
	release()
	release() // releasing twice is harmless
	if first.(*fakeAcct).closed.Load() != 1 {
		t.Errorf("old object closed %d times, want 1", first.(*fakeAcct).closed.Load())
	}
	release2()
	if second.(*fakeAcct).closed.Load() != 0 {
		t.Error("the current object was closed")
	}
	if len(changes) != 1 || changes[0].Kind != ChangeReopened {
		t.Errorf("changes = %+v", changes)
	}
}

// Sends and Reopens racing: every replaced object is closed exactly once,
// never while held, and the current one stays open.
func TestConcurrentAcquireReopen(t *testing.T) {
	h := newHarness(t, []store.Account{row("acc_m", "mastodon", "env", "default", 0, true)}, []EnvAccount{env("mastodon", "default")})
	var wg sync.WaitGroup
	stop := make(chan struct{})
	var heldClosed atomic.Int32
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				a, release, err := h.Acquire("acc_m")
				if err != nil {
					t.Error(err)
					return
				}
				if a.(*fakeAcct).closed.Load() != 0 {
					heldClosed.Add(1)
				}
				release()
			}
		}()
	}
	for range 50 {
		if err := h.Reopen(context.Background(), "acc_m"); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	if heldClosed.Load() != 0 {
		t.Errorf("%d sends got a closed object", heldClosed.Load())
	}
	h.mu.Lock()
	objs := h.open["acc_m"]
	h.mu.Unlock()
	for i, a := range objs {
		want := int32(1)
		if i == len(objs)-1 {
			want = 0
		}
		if a.closed.Load() != want {
			t.Errorf("object %d closed %d times, want %d", i, a.closed.Load(), want)
		}
	}
}

// Owner settings keep the live object (and its caches).
func TestUpdateKeepsObject(t *testing.T) {
	r := row("acc_m", "mastodon", "env", "default", 0, true)
	h := newHarness(t, []store.Account{r}, []EnvAccount{env("mastodon", "default")})
	before := h.live("acc_m")
	var kinds []ChangeKind
	h.OnChange(func(c Change) { kinds = append(kinds, c.Kind) })
	r.Enabled = false
	if err := h.Update("acc_m", r); err != nil {
		t.Fatal(err)
	}
	if e, _ := h.Get("acc_m"); e.Status != Disabled {
		t.Errorf("status = %s", e.Status)
	}
	r.Enabled, r.Label = true, "Work"
	_ = h.Update("acc_m", r)
	a, release, err := h.Acquire("acc_m")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if a != before || before.closed.Load() != 0 {
		t.Error("Update rebuilt the account")
	}
	if e, _ := h.Get("acc_m"); e.Row.Label != "Work" || e.Status != Connected {
		t.Errorf("entry = %+v", e)
	}
	if len(kinds) != 2 || kinds[0] != ChangeUpdated {
		t.Errorf("changes = %v", kinds)
	}
	if err := h.Update("acc_nope", r); !errors.Is(err, ErrNoAccount) {
		t.Errorf("unknown id: %v", err)
	}
}

// Two saves whose notices cross: the older row doesn't undo the newer
// one, and the network's identity the registry learnt is kept.
func TestUpdateNewestWins(t *testing.T) {
	r := row("acc_m", "mastodon", "env", "default", 0, true)
	h := newHarness(t, []store.Account{r}, []EnvAccount{env("mastodon", "default")})
	h.Registry.mu.Lock()
	h.entries["acc_m"].row.Handle, h.entries["acc_m"].row.RemoteID = "@me@new.example", "https://new.example/users/me"
	h.Registry.mu.Unlock()
	older, newer := r, r
	older.Label, older.UpdatedAt = "Old", r.UpdatedAt.Add(time.Second)
	newer.Label, newer.Enabled, newer.UpdatedAt = "New", false, r.UpdatedAt.Add(2*time.Second)
	if err := h.Update("acc_m", newer); err != nil {
		t.Fatal(err)
	}
	if err := h.Update("acc_m", older); err != nil {
		t.Fatal(err)
	}
	e, _ := h.Get("acc_m")
	if e.Row.Label != "New" || e.Row.Enabled || e.Status != Disabled || !e.Row.UpdatedAt.Equal(newer.UpdatedAt) {
		t.Errorf("entry = %+v; the older row won", e)
	}
	if e.Row.Handle != "@me@new.example" || e.Row.RemoteID != "https://new.example/users/me" {
		t.Errorf("identity = %q %q; the stored row's replaced the network's", e.Row.Handle, e.Row.RemoteID)
	}
}

func TestCapsFallback(t *testing.T) {
	h := newHarness(t, []store.Account{row("acc_m", "mastodon", "env", "default", 0, true), row("acc_b", "bluesky", "env", "default", 0, true)},
		[]EnvAccount{env("mastodon", "default")})
	h.live("acc_m").caps = destination.Capabilities{Text: destination.TextCaps{MaxLength: 11000}}
	if c := h.Caps(context.Background(), "acc_m", "mastodon"); c.Text.MaxLength != 11000 {
		t.Errorf("open account caps = %+v", c)
	}
	d, _ := destination.Lookup("bluesky")
	if c := h.Caps(context.Background(), "acc_b", "bluesky"); c.Text.MaxLength != d.Manifest().Defaults.Text.MaxLength || c.Text.MaxLength == 0 {
		t.Errorf("not-open account caps = %+v, want the manifest defaults", c)
	}
	if c := h.Caps(context.Background(), "acc_gone", "nonesuch"); c.Text.MaxLength != 0 {
		t.Errorf("unknown = %+v", c)
	}
}

// An auth error on a send (a reply the account may not make) doesn't park
// the account: only a check that sees the credentials refused does, and a
// later passing check clears it without a restart.
func TestNeedsReconnectOnlyWhenConfirmed(t *testing.T) {
	h := newHarness(t, []store.Account{row("acc_b", "bluesky", "env", "default", 0, true)}, []EnvAccount{env("bluesky", "default")})
	a := h.live("acc_b")
	var statuses []Status
	h.OnChange(func(c Change) { statuses = append(statuses, c.Entry.Status) })

	h.ReportAuthFailure("acc_b") // a 403 on a reply; the credentials are fine
	h.wait()
	if e, _ := h.Get("acc_b"); e.Status != Connected || a.checkCount() != 1 || e.Health == nil {
		t.Fatalf("after a passing check: %+v, %d checks", e, a.checkCount())
	}

	a.setAuth(&no)
	h.ReportAuthFailure("acc_b") // within 10 minutes: no second check
	h.wait()
	if a.checkCount() != 1 {
		t.Errorf("%d checks; reports within 10 minutes should share one", a.checkCount())
	}
	h.clock.Add(reportGap)
	h.ReportAuthFailure("acc_b")
	h.wait()
	e, _ := h.Get("acc_b")
	if e.Status != NeedsReconnect || e.Reason != "the network refused its credentials: refused" {
		t.Fatalf("after a refusing check: %s %q", e.Status, e.Reason)
	}
	if _, _, err := h.Acquire("acc_b"); err == nil {
		t.Error("a refused account was leased for sending")
	}

	// An outage says nothing: the status stays.
	a.setAuth(nil)
	h.clock.Add(checkEveryRefused)
	h.checkDue(context.Background())
	if e, _ := h.Get("acc_b"); e.Status != NeedsReconnect {
		t.Errorf("an outage changed the status to %s", e.Status)
	}

	// Fixed on the network's side: the 15-minute check clears it.
	a.setAuth(&yes)
	h.clock.Add(checkEveryRefused)
	h.checkDue(context.Background())
	if e, _ := h.Get("acc_b"); e.Status != Connected {
		t.Errorf("a passing check left %s", e.Status)
	}
	if fmt.Sprint(statuses) != "[needs_reconnect connected]" {
		t.Errorf("status changes = %v", statuses)
	}
}

// The first check runs soon after start, then daily.
func TestChecksAreScheduled(t *testing.T) {
	h := newHarness(t, []store.Account{row("acc_b", "bluesky", "env", "default", 0, true)}, []EnvAccount{env("bluesky", "default")})
	a := h.live("acc_b")
	h.checkDue(context.Background())
	if a.checkCount() != 0 {
		t.Fatal("checked at once on start")
	}
	h.clock.Add(time.Minute)
	h.checkDue(context.Background())
	h.clock.Add(time.Hour)
	h.checkDue(context.Background())
	if a.checkCount() != 1 {
		t.Errorf("%d checks in the first hour, want 1", a.checkCount())
	}
	h.clock.Add(checkEvery)
	h.checkDue(context.Background())
	if a.checkCount() != 2 {
		t.Errorf("%d checks after a day, want 2", a.checkCount())
	}
}

// A check that raced a Reopen says nothing about the new credentials.
func TestCheckAfterReopenIsIgnored(t *testing.T) {
	h := newHarness(t, []store.Account{row("acc_b", "bluesky", "env", "default", 0, true)}, []EnvAccount{env("bluesky", "default")})
	l, release := h.leaseAny("acc_b")
	defer release()
	if err := h.Reopen(context.Background(), "acc_b"); err != nil {
		t.Fatal(err)
	}
	h.applyHealth(context.Background(), "acc_b", l, destination.Health{Reachable: true, AuthOK: &no})
	if e, _ := h.Get("acc_b"); e.Status != Connected {
		t.Errorf("an old object's check set %s", e.Status)
	}
}

// The registry hands out its own hosts: env accounts reach their own
// addresses as before; content and app-added accounts are guarded.
func TestHostPolicies(t *testing.T) {
	var hosts []destination.AccountHost
	h := newHarness(t, []store.Account{row("acc_m", "mastodon", "env", "default", 0, true)}, []EnvAccount{env("mastodon", "default")},
		func(o *Options) {
			open := o.Open
			o.Open = func(ctx context.Context, typ string, req destination.OpenRequest) (destination.Account, error) {
				hosts = append(hosts, req.Host)
				return open(ctx, typ, req)
			}
		})
	_ = h
	if len(hosts) != 1 {
		t.Fatalf("%d hosts", len(hosts))
	}
	if n := hosts[0].Net(destination.NetAccount); n.Proxy == nil {
		t.Error("an env account's own addresses should be reached as before (with the proxy)")
	}
	_, err := hosts[0].Net(destination.NetContent).DialContext(context.Background(), "tcp", "127.0.0.1:1")
	if !errors.Is(err, destination.ErrBlockedAddress) {
		t.Errorf("content dial err = %v", err)
	}
	if err := hosts[0].Save(context.Background(), destination.StateUpdate{}); err == nil {
		t.Error("Save without a state store succeeded")
	}
}

// Profiles never wait: the stored handle first, the network's answer once
// fetched (saved without a Change), failures retried after a minute.
func TestProfiles(t *testing.T) {
	r := row("acc_b", "bluesky", "env", "default", 0, true)
	r.Handle = "old.example"
	h := newHarness(t, []store.Account{r}, []EnvAccount{env("bluesky", "default")})
	a := h.live("acc_b")
	a.ident = destination.Identity{Handle: "new.example", RemoteID: "did:plc:x", DisplayName: "Me"}
	var changes int
	h.OnChange(func(Change) { changes++ })

	if p, _ := h.Profile("acc_b"); p.Handle != "old.example" || !p.FetchedAt.IsZero() {
		t.Errorf("first answer = %+v, want the stored handle", p)
	}
	h.wait()
	p, _ := h.Profile("acc_b")
	if p.Handle != "new.example" || p.DisplayName != "Me" || p.FetchedAt.IsZero() {
		t.Errorf("fetched = %+v", p)
	}
	if got := h.store.identities["acc_b"]; got != [2]string{"new.example", "did:plc:x"} {
		t.Errorf("stored identity = %v", got)
	}
	if e, _ := h.Get("acc_b"); e.Row.Handle != "new.example" || e.Row.RemoteID != "did:plc:x" {
		t.Errorf("row = %+v", e.Row)
	}
	if changes != 0 {
		t.Error("an identity refresh emitted a change")
	}

	// Within 10 minutes the cache answers; a failure is retried after a
	// minute and keeps the last good profile.
	a.mu.Lock()
	a.ident, a.identErr = destination.Identity{}, errors.New("timeout")
	a.mu.Unlock()
	h.clock.Add(profileTTL)
	h.Profile("acc_b")
	h.wait()
	if p, _ := h.Profile("acc_b"); p.Handle != "new.example" {
		t.Errorf("a failed fetch lost the profile: %+v", p)
	}
	h.wait()
	h.mu.Lock()
	h.Registry.mu.RLock()
	failedAt := h.entries["acc_b"].profile.at
	h.Registry.mu.RUnlock()
	h.mu.Unlock()
	if !failedAt.Equal(h.clock.Now()) {
		t.Errorf("failure not recorded at %v", failedAt)
	}
}

// Two accounts that turn out to be one network account: the second keeps
// its handle but no remote id, and the owner is told once, not at every
// profile refresh.
func TestDuplicateIdentityWarnsOnce(t *testing.T) {
	var buf bytes.Buffer
	var bufMu sync.Mutex
	logger := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		bufMu.Lock()
		defer bufMu.Unlock()
		return buf.Write(p)
	}), nil))
	h := newHarness(t, []store.Account{row("acc_b", "bluesky", "env", "default", 0, true)}, []EnvAccount{env("bluesky", "default")},
		func(o *Options) { o.Logger = logger })
	h.store.mu.Lock()
	h.store.dup = &store.DuplicateAccountError{ID: "acc_first", Label: "Bluesky"}
	h.store.mu.Unlock()
	a := h.live("acc_b")
	a.mu.Lock()
	a.ident = destination.Identity{Handle: "me.example", RemoteID: "did:plc:x"}
	a.mu.Unlock()
	for range 3 {
		h.Profile("acc_b")
		h.wait()
		h.clock.Add(profileTTL)
	}
	bufMu.Lock()
	out := buf.String()
	bufMu.Unlock()
	if n := strings.Count(out, "two accounts are the same network account"); n != 1 {
		t.Errorf("warned %d times:\n%s", n, out)
	}
	if !strings.Contains(out, "same_as=acc_first") || strings.Contains(out, "account identity not saved") {
		t.Errorf("log:\n%s", out)
	}
	if e, _ := h.Get("acc_b"); e.Row.Handle != "me.example" || e.Row.RemoteID != "" {
		t.Errorf("row = %q %q, want the handle and no remote id", e.Row.Handle, e.Row.RemoteID)
	}
	// The other account is deleted: the remote id is kept at the next
	// refresh.
	h.store.mu.Lock()
	h.store.dup = nil
	h.store.mu.Unlock()
	h.Profile("acc_b")
	h.wait()
	if e, _ := h.Get("acc_b"); e.Row.RemoteID != "did:plc:x" || h.store.identities["acc_b"][1] != "did:plc:x" {
		t.Errorf("after the other went: %q %v", e.Row.RemoteID, h.store.identities["acc_b"])
	}
	// The same account added again later is a new duplicate: said again.
	h.store.mu.Lock()
	h.store.dup = &store.DuplicateAccountError{ID: "acc_first", Label: "Bluesky"}
	h.store.mu.Unlock()
	a.mu.Lock()
	a.ident = destination.Identity{Handle: "me.example", RemoteID: "did:plc:y"}
	a.mu.Unlock()
	h.clock.Add(profileTTL)
	h.Profile("acc_b")
	h.wait()
	bufMu.Lock()
	out = buf.String()
	bufMu.Unlock()
	if n := strings.Count(out, "two accounts are the same network account"); n != 2 {
		t.Errorf("a new duplicate warned %d times in all", n)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// Close closes every open account.
func TestCloseClosesAccounts(t *testing.T) {
	h := newHarness(t, []store.Account{row("acc_m", "mastodon", "env", "default", 0, true)}, []EnvAccount{env("mastodon", "default")})
	a := h.live("acc_m")
	h.Close()
	if a.closed.Load() != 1 {
		t.Errorf("closed %d times", a.closed.Load())
	}
	// A send that comes too late is told the server is stopping, not that
	// the account is gone.
	if _, _, err := h.Acquire("acc_m"); !errors.Is(err, ErrClosed) {
		t.Errorf("Acquire after Close = %v, want ErrClosed", err)
	}
}

// The environment's credentials are the operator's: an env row of
// another owner never opens with them, and an env row stored without a
// key is the unnumbered account.
func TestEnvCredentialsOnlyForTheOperator(t *testing.T) {
	other := row("acc_other", "mastodon", "env", "default", 0, true)
	other.OwnerID = "own_other"
	blank := row("acc_blank", "bluesky", "env", "", time.Second, true)
	h := newHarness(t, []store.Account{other, blank}, []EnvAccount{env("mastodon", "default"), env("bluesky", "default")})
	if en, _ := h.Get("acc_other"); en.Status != NotConfigured {
		t.Errorf("another owner's env row = %s, want not_configured", en.Status)
	}
	if len(h.open["acc_other"]) != 0 {
		t.Error("another owner's env row was opened with the operator's credentials")
	}
	if en, _ := h.Get("acc_blank"); en.Status != Connected {
		t.Errorf("env row with no key = %s %q, want connected", en.Status, en.Reason)
	}
}
