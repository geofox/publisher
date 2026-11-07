package accounts

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/store"
)

// From the part E reviews.

// flakyFlows is the store's flow side, failing or holding the finishes of
// completed flows.
type flakyFlows struct {
	FlowStore
	mu         sync.Mutex
	failFinish int           // fail this many completed finishes
	hold       chan struct{} // the first completed finish waits on it
	held       chan struct{} // closed once that finish waits
	n          int
}

func (f *flakyFlows) FinishOAuthFlow(ctx context.Context, owner, id string, o store.FlowOutcome) error {
	f.mu.Lock()
	if o.Status == store.FlowCompleted && f.failFinish > 0 {
		f.failFinish--
		f.mu.Unlock()
		return errors.New("database is locked (SQLITE_BUSY)")
	}
	first := false
	if o.Status == store.FlowCompleted {
		f.n++
		first = f.n == 1 && f.hold != nil
	}
	f.mu.Unlock()
	if first {
		close(f.held)
		<-f.hold
	}
	return f.FlowStore.FinishOAuthFlow(ctx, owner, id, o)
}

// Back in a browser without a Publisher session: the account isn't added
// until the owner confirms it, by who it is, in the tab that started the
// flow. A leaked sign-in link can't add someone else's account unseen.
func TestConnectWithoutSessionConfirms(t *testing.T) {
	w := newConnectEnv(t, false)
	ctx := context.Background()
	f, q := startConnect(t, w, ConnectStart{Config: map[string]any{"server": "https://net.example"}})
	v, err := w.r.CompleteConnect(ctx, connectType, callback(q.Get("state"), "c-tok9"), "")
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != store.FlowChoose || len(v.Candidates) != 1 || v.Candidates[0].Handle != "tok9.example" || w.count() != 0 {
		t.Fatalf("not confirmed yet: %+v (%d accounts)", v, w.count())
	}
	// A reload of the callback says the same, without the network.
	n := len(connectDrv.done)
	if again, err := w.r.CompleteConnect(ctx, connectType, callback(q.Get("state"), "c-tok9"), ""); err != nil ||
		again.Status != store.FlowChoose || len(connectDrv.done) != n {
		t.Fatalf("reload: %+v %v", again, err)
	}
	v, err = w.r.Choose(ctx, store.DefaultOwner, "user-1", f.ID, 0)
	if err != nil || v.Status != store.FlowCompleted || v.AccountID == "" || w.count() != 1 {
		t.Fatalf("confirmed: %+v %v", v, err)
	}
	if got := w.secretsOf(v.AccountID, "https://net.example"); got["token"] != "tok9" {
		t.Fatalf("secrets: %v", got)
	}
}

// A flow whose callback came but whose outcome wasn't recorded (a
// restart mid-exchange) reads as interrupted, not pending for ever; a
// failed write of the outcome is tried again, and when that fails too the
// callback still says the account is connected.
func TestConnectInterrupted(t *testing.T) {
	w := newConnectEnv(t, false)
	ctx := context.Background()
	at := time.Now()
	w.r.opt.Now = func() time.Time { return at }

	// A restart after the claim.
	f, q := startConnect(t, w, ConnectStart{Config: map[string]any{"server": "https://net.example"}})
	if err := w.st.ClaimOAuthFlow(ctx, f.ID, at); err != nil {
		t.Fatal(err)
	}
	if v, _ := w.r.Flow(ctx, store.DefaultOwner, "user-1", f.ID); v.Status != store.FlowPending {
		t.Fatalf("just claimed: %+v", v)
	}
	at = at.Add(claimTimeout)
	v, err := w.r.Flow(ctx, store.DefaultOwner, "user-1", f.ID)
	if err != nil || v.Status != store.FlowFailed || v.Error != destination.ConnectInterrupted {
		t.Fatalf("interrupted: %+v %v", v, err)
	}
	if v, _ := w.r.CompleteConnect(ctx, connectType, callback(q.Get("state"), "c-x"), "user-1"); v.Error != destination.ConnectInterrupted {
		t.Fatalf("callback reload: %+v", v)
	}

	// One failed write: tried again.
	ff := &flakyFlows{FlowStore: w.st, failFinish: 1}
	w.r.opt.Flows = ff
	f, q = startConnect(t, w, ConnectStart{Config: map[string]any{"server": "https://net.example"}})
	v, err = w.r.CompleteConnect(ctx, connectType, callback(q.Get("state"), "c-retry"), "user-1")
	if err != nil || v.Status != store.FlowCompleted || v.AccountID == "" {
		t.Fatalf("retried: %+v %v", v, err)
	}
	// Two: the callback says connected; the flow then reads interrupted.
	ff.failFinish = 2
	f, q = startConnect(t, w, ConnectStart{Config: map[string]any{"server": "https://net.example"}})
	v, err = w.r.CompleteConnect(ctx, connectType, callback(q.Get("state"), "c-lost"), "user-1")
	if err != nil || v.Status != store.FlowCompleted || v.AccountID == "" {
		t.Fatalf("not recorded: %+v %v", v, err)
	}
	if _, ok := w.r.Get(v.AccountID); !ok {
		t.Fatal("the account isn't there")
	}
	at = at.Add(claimTimeout)
	if v, _ := w.r.Flow(ctx, store.DefaultOwner, "user-1", f.ID); v.Error != destination.ConnectInterrupted {
		t.Fatalf("later: %+v", v)
	}
}

// Two choices racing on one flow store one account; the other answers
// the flow as it then stands.
func TestConnectChooseRace(t *testing.T) {
	w := newConnectEnv(t, false)
	ctx := context.Background()
	ff := &flakyFlows{FlowStore: w.st, hold: make(chan struct{}), held: make(chan struct{})}
	w.r.opt.Flows = ff
	f, q := startConnect(t, w, ConnectStart{Config: map[string]any{"server": "https://net.example"}})
	if v, err := w.r.CompleteConnect(ctx, connectType, callback(q.Get("state"), "many"), "user-1"); err != nil || v.Status != store.FlowChoose {
		t.Fatalf("choose: %+v %v", v, err)
	}
	var first FlowView
	var firstErr error
	done := make(chan struct{})
	go func() {
		first, firstErr = w.r.Choose(ctx, store.DefaultOwner, "user-1", f.ID, 0)
		close(done)
	}()
	<-ff.held // the first choice is storing its outcome
	second := make(chan FlowView)
	go func() {
		v, err := w.r.Choose(ctx, store.DefaultOwner, "user-1", f.ID, 1)
		if err != nil {
			t.Errorf("second: %v", err)
		}
		second <- v
	}()
	time.Sleep(50 * time.Millisecond) // the second waits for the first
	close(ff.hold)
	<-done
	b := <-second
	if firstErr != nil || first.Status != store.FlowCompleted || b.Status != store.FlowCompleted || b.AccountID != first.AccountID {
		t.Fatalf("first %+v %v; second %+v", first, firstErr, b)
	}
	if w.count() != 1 {
		t.Fatalf("one flow stored %d accounts", w.count())
	}
}

// Events of a connect name the session, as every other write from the
// web app does.
func TestConnectEventsActor(t *testing.T) {
	w := newConnectEnv(t, false)
	ctx := context.Background()
	_, q := startConnect(t, w, ConnectStart{Config: map[string]any{"server": "https://net.example"}})
	v, err := w.r.CompleteConnect(ctx, connectType, callback(q.Get("state"), "c-same1"), "user-1")
	if err != nil || v.Status != store.FlowCompleted {
		t.Fatalf("%+v %v", v, err)
	}
	_, q = startConnect(t, w, ConnectStart{Reconnect: v.AccountID})
	if v, err := w.r.CompleteConnect(ctx, connectType, callback(q.Get("state"), "c-same2"), "user-1"); err != nil || v.Status != store.FlowCompleted {
		t.Fatalf("reconnect: %+v %v", v, err)
	}
	evs, err := w.st.EventsSince(ctx, store.EventFilter{OwnerID: store.DefaultOwner})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range evs {
		if e.Type == store.EventAccountCreated || e.Type == store.EventAccountUpdated {
			seen[e.Type] = true
			if e.Data.Actor != "session" {
				t.Errorf("%s: actor %q", e.Type, e.Data.Actor)
			}
		}
	}
	if !seen[store.EventAccountCreated] || !seen[store.EventAccountUpdated] {
		t.Fatalf("events: %v", seen)
	}
}

// Why a sign-in method can't start, before it is tried: the master key.
func TestConnectUnavailable(t *testing.T) {
	d, _ := destination.Lookup(connectType)
	a := d.Manifest().Auth[0]
	w := newConnectEnv(t, false)
	if why := w.r.ConnectUnavailable(connectType, a, "https://pub.example/oauth/callback/x"); why != "" {
		t.Fatalf("ready: %q", why)
	}
	nk := newConnectEnv(t, true)
	if why := nk.r.ConnectUnavailable(connectType, a, "https://pub.example/oauth/callback/x"); !strings.Contains(why, "master key") {
		t.Fatalf("no key: %q", why)
	}
}

// An account that signed in at its network changes its settings without
// signing in again, unless its server changes.
func TestChangeSignedInAccount(t *testing.T) {
	w := newWriteEnv(t, false)
	ctx := context.Background()
	signedIn := func(n NewAccount) string {
		t.Helper()
		en, err := w.r.Create(ctx, n)
		if err != nil {
			t.Fatal(err)
		}
		// As a connect flow (or a reconnect) leaves it.
		d, _ := destination.Lookup(n.Type)
		a, _ := d.Manifest().AuthMethod("oauth")
		cfg, _ := rowConfig(en.Row)
		sec, _, err := w.r.openSecrets(ctx, en.Row, destination.AudienceOf(d.Manifest(), cfg))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.r.replaceChecked(ctx, en.Row, d, a, cfg, sec, store.CredentialChange{Fields: []string{"credentials", "method"},
			Actor: "session"}); err != nil {
			t.Fatal(err)
		}
		return en.Row.ID
	}
	th := signedIn(NewAccount{Owner: store.DefaultOwner, Type: "threads", Method: "token",
		Credentials: map[string]any{"access_token": "tok-th"}})
	en, err := w.r.ChangeCredentials(ctx, CredentialUpdate{Owner: store.DefaultOwner, ID: th, Config: map[string]any{"user_id": "17841400000"}})
	if err != nil || en.Row.AuthMethod != "oauth" {
		t.Fatalf("a setting: %+v %v", en.Row, err)
	}
	ma := signedIn(mastodon("tok-ma", "https://one.example"))
	_, err = w.r.ChangeCredentials(ctx, CredentialUpdate{Owner: store.DefaultOwner, ID: ma,
		Config: map[string]any{"instance_url": "https://two.example"}})
	pointers("/credentials")(t, err)
	if !strings.Contains(err.Error(), "signing in again") && !strings.Contains(errDetail(err), "signing in again") {
		t.Fatalf("detail: %v", err)
	}
}

func errDetail(err error) string {
	var inv *InvalidError
	if errors.As(err, &inv) && len(inv.Problems) > 0 {
		return inv.Problems[0].Detail
	}
	return ""
}
