package dispatch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/accounts"
	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/thread"
)

// The engine by account id: several accounts of a type, accounts that
// can't be used, recovery and fan-out per account.

// twoStubs is a dispatcher with two accounts of the stub type: acc_a with
// the manifest's 20-character limit, acc_b with 40.
func twoStubs(t *testing.T, mode destination.IdempotencyMode) (*Dispatcher, *testRegistry, *stubAccount, *stubAccount) {
	t.Helper()
	a := &stubAccount{manifest: stubManifest(mode)}
	b := &stubAccount{manifest: stubManifest(mode)}
	capsB := b.manifest.Defaults
	capsB.Text.MaxLength = 40
	b.caps = &capsB
	reg := registryOf(nil).add("acc_a", "stub", a).add("acc_b", "stub", b)
	st := openDispatchStore(t)
	for _, id := range []string{"acc_a", "acc_b"} {
		if err := ensureAccount(st, id, "stub"); err != nil {
			t.Fatal(err)
		}
	}
	d := &Dispatcher{Registry: reg, Store: st, Policy: RetryPolicy{Enabled: true, MaxAttempts: 2, Base: time.Minute, Max: time.Hour}}
	return d, reg, a, b
}

func textsOf(reqs []destination.PublishRequest) []string {
	var out []string
	for _, r := range reqs {
		out = append(out, r.Text)
	}
	return out
}

// Two accounts of one type in one post: each sends its own text, split by
// its own limit and layout, with its own idempotency keys, through its
// own credentials.
func TestTwoAccountsOfATypeInOnePost(t *testing.T) {
	d, reg, a, b := twoStubs(t, destination.IdempotentKey)
	shared := "one two three four five six seven"
	own := "first part\nsecond part"
	rec := d.PostSpec(context.Background(), "p2", PostSpec{MasterText: shared, Targets: []TargetSpec{
		{AccountID: "acc_a", Type: "stub"},
		{AccountID: "acc_b", Type: "stub", Text: own, Layout: thread.Layout{Breaks: []int{len("first part\n")}}},
		{AccountID: "acc_a", Type: "stub", Text: "named twice: ignored"},
	}})
	if rec.Status != "success" || len(rec.Targets) != 2 || len(rec.Platforms) != 1 || rec.Platforms[0] != "stub" {
		t.Fatalf("post = %+v", rec)
	}
	if len(a.reqs) < 2 || strings.Join(textsOf(a.reqs), " ") != shared {
		t.Errorf("acc_a sent %q; want the shared text in parts of at most 20", textsOf(a.reqs))
	}
	for _, r := range a.reqs {
		if n := len([]rune(r.Text)); n > 20 {
			t.Errorf("acc_a part of %d characters", n)
		}
	}
	if got := textsOf(b.reqs); len(got) != 2 || got[0] != "first part" || got[1] != "second part" {
		t.Errorf("acc_b sent %q; want its own text at its own break", got)
	}
	keys := map[string]bool{}
	for _, tg := range rec.Targets {
		for _, it := range tg.Items {
			if it.IdemKey != store.AccountItemKey("p2", tg.AccountID, it.Ordinal) || keys[it.IdemKey] {
				t.Errorf("%s item %d key %q", tg.AccountID, it.Ordinal, it.IdemKey)
			}
			keys[it.IdemKey] = true
		}
	}
	for _, r := range append(append([]destination.PublishRequest{}, a.reqs...), b.reqs...) {
		if !keys[r.IdempotencyKey] {
			t.Errorf("sent key %q isn't a stored one", r.IdempotencyKey)
		}
	}
	if reg.held() != 0 {
		t.Errorf("%d leases still held", reg.held())
	}
}

// An item of a locked account waits: nothing is sent, no attempt is
// counted, it never gives up (past MaxAttempts, and with automatic retries
// off), no failure is alerted, and it goes out once the account opens.
func TestLockedAccountWaits(t *testing.T) {
	d, reg, a, _ := twoStubs(t, destination.IdempotentKey)
	al := &fakeAlerter{}
	d.Alerter = al
	now := time.Now().UTC()
	d.Clock = func() time.Time { return now }
	reg.set("acc_a", accounts.Locked, "its saved state could not be read")
	due := now
	rec := d.planPost(context.Background(), "lk", PostSpec{MasterText: "hi", Targets: []TargetSpec{{AccountID: "acc_a", Type: "stub"}}}, &due, "sending")
	if err := d.create(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	var last time.Time
	for i := 0; i < 6; i++ {
		if i == 3 {
			d.Policy.Enabled = false
		}
		if n := d.runDue(context.Background()); n != 1 {
			t.Fatalf("round %d: ran %d items", i, n)
		}
		post, _ := d.Store.GetPost("lk")
		tg, it := post.Targets[0], post.Targets[0].Items[0]
		if post.Status != "sending" || it.Status != store.ItemPlanned || it.AttemptCount != 0 || tg.GaveUpAt != nil ||
			it.NextAttemptAt == nil || !strings.Contains(it.Error, "locked") {
			t.Fatalf("round %d: post %q target %+v item %+v", i, post.Status, tg, it)
		}
		wait := it.NextAttemptAt.Sub(now)
		if wait < time.Minute || wait > lockedWaitMax || it.NextAttemptAt.Before(last) {
			t.Fatalf("round %d: waits %v", i, wait)
		}
		last = *it.NextAttemptAt
		now = *it.NextAttemptAt
	}
	if len(a.reqs) != 0 || len(al.bodies) != 0 {
		t.Fatalf("sent %d, alerts %v", len(a.reqs), al.bodies)
	}
	if last.Sub(rec.CreatedAt) < 20*time.Minute {
		t.Errorf("waits didn't grow: last due %v after creation", last.Sub(rec.CreatedAt))
	}
	reg.set("acc_a", accounts.Connected, "")
	d.runDue(context.Background())
	post, _ := d.Store.GetPost("lk")
	if post.Status != "success" || len(a.reqs) != 1 || post.Targets[0].Items[0].Error != "" {
		t.Fatalf("after unlocking: post %q, sent %d, item %+v", post.Status, len(a.reqs), post.Targets[0].Items[0])
	}
}

// A disabled account's item fails as auth, saying why, without a network
// call, and the registry isn't asked to check its credentials.
func TestDisabledAccountFailsWithoutSending(t *testing.T) {
	d, reg, a, b := twoStubs(t, destination.IdempotentKey)
	reg.set("acc_a", accounts.Disabled, "turned off in the app")
	rec := d.PostSpec(context.Background(), "ds", PostSpec{MasterText: "hi", Targets: []TargetSpec{
		{AccountID: "acc_a", Type: "stub"}, {AccountID: "acc_b", Type: "stub"}}})
	if len(a.reqs) != 0 || len(b.reqs) != 1 || rec.Status != "partial" {
		t.Fatalf("sent %d and %d; post %q", len(a.reqs), len(b.reqs), rec.Status)
	}
	tg := rec.Targets[0]
	if tg.AccountID != "acc_a" || tg.ErrorClass != string(destination.ClassAuth) || tg.GaveUpAt == nil ||
		!strings.Contains(tg.Items[0].Error, "turned off") {
		t.Errorf("target = %+v, item %+v", tg, tg.Items[0])
	}
	if len(reg.reported) != 0 {
		t.Errorf("reported %v", reg.reported)
	}
}

// A refusal of the credentials by the network asks the registry to check
// them; other failures don't.
func TestAuthRefusalIsReported(t *testing.T) {
	d, reg, a, b := twoStubs(t, destination.IdempotentKey)
	a.publish = func(int, destination.PublishRequest) (destination.PublishResult, error) {
		return destination.PublishResult{}, &destination.Error{Class: destination.ClassAuth, Err: errors.New("401")}
	}
	b.publish = func(int, destination.PublishRequest) (destination.PublishResult, error) {
		return destination.PublishResult{}, &destination.Error{Class: destination.ClassTransient, Err: errors.New("503")}
	}
	d.PostSpec(context.Background(), "au", PostSpec{MasterText: "hi", Targets: []TargetSpec{
		{AccountID: "acc_a", Type: "stub"}, {AccountID: "acc_b", Type: "stub"}}})
	if len(reg.reported) != 1 || reg.reported[0] != "acc_a" {
		t.Errorf("reported %v; want acc_a only", reg.reported)
	}
}

// locatingStub is a stub account that looks its posts up, recording the
// URLs it was asked for.
type locatingStub struct {
	*stubAccount
	asked []string
}

func (l *locatingStub) Locate(_ context.Context, url string) (destination.ItemRef, error) {
	l.asked = append(l.asked, url)
	return destination.ItemRef{ID: "found", URL: url}, nil
}

// Two accounts of one type each leave an item sending when the process
// stops. A restart settles each by its own account's capabilities (the
// keyed one is sent again, the other is left for the owner), the alert
// names the account, and looking the post up asks the item's own account.
func TestRestartRecoversEachAccountsItem(t *testing.T) {
	keyed := &locatingStub{stubAccount: &stubAccount{manifest: stubManifest(destination.IdempotentKey), publish: (&keyedPlatform{}).publish}}
	plain := &locatingStub{stubAccount: &stubAccount{manifest: stubManifest(destination.IdempotentNone)}}
	reg := registryOf(nil).add("acc_k", "stub", keyed).add("acc_n", "stub", plain)
	reg.rows["acc_n"] = store.Account{ID: "acc_n", OwnerID: store.DefaultOwner, Type: "stub", Label: "Stub", Handle: "@me@two.example"}
	st := openDispatchStore(t)
	for _, id := range []string{"acc_k", "acc_n"} {
		if err := ensureAccount(st, id, "stub"); err != nil {
			t.Fatal(err)
		}
	}
	policy := RetryPolicy{Enabled: true, MaxAttempts: 6, Base: time.Minute, Max: time.Hour}
	d := &Dispatcher{Registry: reg, Store: st, Policy: policy}
	ctx := context.Background()
	now := d.now()
	rec := d.planPost(ctx, "rr", PostSpec{MasterText: "hi", Targets: []TargetSpec{
		{AccountID: "acc_k", Type: "stub"}, {AccountID: "acc_n", Type: "stub"}}}, &now, "sending")
	if err := d.create(ctx, rec); err != nil {
		t.Fatal(err)
	}
	post, _ := st.GetPost("rr")
	for _, tg := range post.Targets {
		c, err := st.ClaimItem(ctx, d.now(), store.ClaimFilter{TargetID: tg.ID})
		if err != nil || c == nil || c.AccountID != tg.AccountID {
			t.Fatalf("claim %s: %+v, %v", tg.AccountID, c, err)
		}
		p, _ := st.GetPost("rr")
		ctg, it := findItem(p, c)
		if r := d.publishClaimed(ctx, p, *ctg, *it, nil); r.Status != "success" {
			t.Fatalf("send %s: %+v", tg.AccountID, r)
		}
	}
	al := &fakeAlerter{}
	d2 := &Dispatcher{Registry: reg, Store: st, Policy: policy, Alerter: al}
	d2.Recover(ctx)
	post, _ = st.GetPost("rr")
	for _, tg := range post.Targets {
		it := tg.Items[0]
		switch tg.AccountID {
		case "acc_k":
			if it.Status != store.ItemUnknown || it.NextAttemptAt == nil || tg.GaveUpAt != nil {
				t.Errorf("keyed account's item %+v, target %+v: want it sent again", it, tg)
			}
		case "acc_n":
			if it.Status != store.ItemUnknown || it.NextAttemptAt != nil || tg.GaveUpAt == nil {
				t.Errorf("plain account's item %+v, target %+v: want it left for the owner", it, tg)
			}
		}
	}
	if len(al.bodies) != 1 || !strings.Contains(al.bodies[0], "Stub (@me@two.example)") {
		t.Errorf("alerts %v; want one naming the plain account", al.bodies)
	}
	d2.runDue(ctx)
	if len(keyed.reqs) != 2 || len(plain.reqs) != 1 {
		t.Errorf("sent %d and %d; want the keyed item sent again, the other not", len(keyed.reqs), len(plain.reqs))
	}
	if !d2.CanLocate("acc_n") {
		t.Fatal("CanLocate")
	}
	if _, err := d2.Locate(ctx, "acc_n", "https://two.example/@me/1"); err != nil {
		t.Fatal(err)
	}
	if len(plain.asked) != 1 || len(keyed.asked) != 0 {
		t.Errorf("located with %v and %v; want the item's own account", keyed.asked, plain.asked)
	}
	if reg.held() != 0 {
		t.Errorf("%d leases still held", reg.held())
	}
}

// Fan-out goes through the job's own account, through another account of
// its type when that one can't be used, and waits (no failure counted)
// when none can.
func TestFanoutByAccount(t *testing.T) {
	own, other := &countingRebroadcaster{}, &countingRebroadcaster{}
	reg := registryOf(nil).
		add("acc_n1", "nostr", &legacyNostr{stubNostrFanout{own}}).
		add("acc_n2", "nostr", &legacyNostr{stubNostrFanout{other}})
	st := openDispatchStore(t)
	p := &store.Post{ID: "fp", CreatedAt: time.Now().UTC(), Platforms: []string{"nostr"}, Status: "success",
		Targets: []store.Target{{AccountID: "acc_n1", Platform: "nostr", Status: "success"}}}
	if err := saveWithAccounts(st, p); err != nil {
		t.Fatal(err)
	}
	if err := ensureAccount(st, "acc_n2", "nostr"); err != nil {
		t.Fatal(err)
	}
	saved, _ := st.GetPost("fp")
	itemID := saved.Targets[0].Items[0].ID
	d := &Dispatcher{Registry: reg, Store: st}
	f := NewFanout(d, 6, time.Minute, time.Hour, time.Second)
	ctx := context.Background()

	_ = st.EnqueueFanout("fp", itemID, `{"id":"e1"}`, []string{"wss://a"})
	f.runDue(ctx)
	if len(own.calls) != 1 || len(other.calls) != 0 {
		t.Fatalf("delivered %v / %v; want the job's own account", own.calls, other.calls)
	}

	reg.set("acc_n1", accounts.Locked, "")
	_ = st.EnqueueFanout("fp", itemID, `{"id":"e2"}`, []string{"wss://b"})
	f.runDue(ctx)
	if len(own.calls) != 1 || len(other.calls) != 1 {
		t.Fatalf("delivered %v / %v; want the other account while the own is locked", own.calls, other.calls)
	}

	reg.set("acc_n2", accounts.Disabled, "")
	_ = st.EnqueueFanout("fp", itemID, `{"id":"e3"}`, []string{"wss://c"})
	f.runDue(ctx)
	jobs, _ := st.DueFanout(time.Now(), 10)
	if len(own.calls)+len(other.calls) != 2 || len(jobs) != 1 || jobs[0].RetryCount != 0 {
		t.Fatalf("with no account: delivered %d, due %+v; want the job waiting, uncounted", len(own.calls)+len(other.calls), jobs)
	}
	if reg.held() != 0 {
		t.Errorf("%d leases still held", reg.held())
	}
}

// An interaction acts natively on its source account only: another
// account of the source's type posts a link-quote copy, and a retry finds
// the source target by account.
func TestInteractionActsOnTheSourceAccount(t *testing.T) {
	d, _, a, b := twoStubs(t, destination.IdempotentKey)
	for _, s := range []*stubAccount{a, b} {
		caps := s.Capabilities(context.Background())
		caps.Text.MaxLength = 500
		caps.Interactions = destination.InteractionCaps{Quote: true, Reply: true}
		s.caps = &caps
	}
	rec := d.InteractSpec(context.Background(), "ia", InteractSpec{Action: actionQuote, SourcePlatform: "stub",
		SourceAccount: "acc_b", Ref: InteractRef{URI: "stub://1"}, SourceURL: "https://stub.example/1",
		SourcePreview: SourcePreview{Author: "@x", Text: "the original"}, Text: "look",
		Targets: []TargetSpec{{AccountID: "acc_a", Type: "stub"}, {AccountID: "acc_b", Type: "stub", Text: "b's take"}}})
	if rec.Status != "success" || rec.Interaction.SourceAccount != "acc_b" {
		t.Fatalf("post %q, interaction %+v", rec.Status, rec.Interaction)
	}
	if len(b.reqs) != 1 || b.reqs[0].Quote == nil || b.reqs[0].Text != "b's take" {
		t.Errorf("acc_b sent %+v; want the native quote with its own text", b.reqs)
	}
	if len(a.reqs) != 1 || a.reqs[0].Quote != nil || !strings.Contains(a.reqs[0].Text, "https://stub.example/1") {
		t.Errorf("acc_a sent %+v; want a link-quote copy", a.reqs)
	}
	for _, tg := range rec.Targets {
		_, _, isAction, _ := interactionAction(rec, tg)
		if isAction != (tg.AccountID == "acc_b") {
			t.Errorf("%s: isAction %v", tg.AccountID, isAction)
		}
	}
}
