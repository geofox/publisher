package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/store"
)

// keyedPlatform is a platform that drops a repeat of an idempotency key, as
// Mastodon does: the second request with a key returns the first's post.
type keyedPlatform struct {
	mu      sync.Mutex
	byKey   map[string]destination.PublishResult
	created []destination.PublishRequest // posts that exist on the platform
}

func (k *keyedPlatform) publish(_ int, req destination.PublishRequest) (destination.PublishResult, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.byKey == nil {
		k.byKey = map[string]destination.PublishResult{}
	}
	if res, ok := k.byKey[req.IdempotencyKey]; ok && req.IdempotencyKey != "" {
		return res, nil
	}
	res := published(len(k.created))
	k.created = append(k.created, req)
	k.byKey[req.IdempotencyKey] = res
	return res, nil
}

// sendWithoutRecording claims the next item of postID and sends it, as a
// process does just before it stops: the platform has it, the store says
// sending.
func sendWithoutRecording(t *testing.T, d *Dispatcher, postID string) *store.Claim {
	t.Helper()
	ctx := context.Background()
	post, err := d.Store.GetPost(postID)
	if err != nil {
		t.Fatal(err)
	}
	c, err := d.Store.ClaimItem(ctx, d.now(), store.ClaimFilter{TargetID: post.Targets[0].ID})
	if err != nil || c == nil {
		t.Fatalf("claim: %+v, %v", c, err)
	}
	post, _ = d.Store.GetPost(postID)
	tg, it := findItem(post, c)
	if r := d.publishClaimed(ctx, post, *tg, *it, nil); r.Status != "success" {
		t.Fatalf("send: %+v", r)
	}
	return c
}

// plan saves a post to the stub account without sending it.
func plan(t *testing.T, d *Dispatcher, text string) string {
	t.Helper()
	now := d.now()
	rec := d.planPost(context.Background(), newID(), PostSpec{MasterText: text, Platforms: []string{"stub"}, Source: "test"}, &now, "sending")
	if err := d.create(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	return rec.ID
}

// A process stops after sending part 2 of a thread and before recording
// it. On restart the part is re-sent with its key, the platform drops the
// repeat, and the thread completes: three posts, none twice.
func TestCrashBetweenThreadItemsCompletesWithoutDuplicate(t *testing.T) {
	pf := &keyedPlatform{}
	acct := &stubAccount{manifest: stubManifest(destination.IdempotentKey), publish: pf.publish}
	d := stubDispatcher(t, acct)
	id := plan(t, d, "one\n---\ntwo\n---\nthree")

	head := sendWithoutRecording(t, d, id)
	if _, err := d.Store.RecordItemResult(context.Background(), store.ItemResult{Claim: *head, Status: store.ItemPublished,
		RemoteID: "id0", RemoteURL: "https://x/0"}); err != nil {
		t.Fatal(err)
	}
	sendWithoutRecording(t, d, id) // part 2 reaches the platform; the process stops

	// Restart: a new dispatcher on the same store.
	al := &fakeAlerter{}
	d2 := &Dispatcher{Accounts: d.Accounts, Store: d.Store, Alerter: al, Policy: d.Policy}
	d2.recoverSending(context.Background())
	if n := d2.runDue(context.Background()); n != 2 {
		t.Fatalf("ran %d items after restart, want 2 (part 2 again, part 3)", n)
	}

	if len(pf.created) != 3 {
		t.Fatalf("platform has %d posts, want 3: %+v", len(pf.created), pf.created)
	}
	if len(acct.reqs) != 4 || acct.reqs[1].IdempotencyKey != acct.reqs[2].IdempotencyKey {
		t.Fatalf("part 2 should be re-sent with its key: %d requests", len(acct.reqs))
	}
	last := acct.reqs[3]
	if last.InReplyTo == nil || last.InReplyTo.Parent.ID != "id1" || last.InReplyTo.Root.ID != "id0" {
		t.Errorf("part 3 threads under %+v, want parent id1, root id0", last.InReplyTo)
	}
	post, _ := d.Store.GetPost(id)
	if post.Status != "success" || post.Targets[0].Status != "success" {
		t.Errorf("post = %q, target = %q", post.Status, post.Targets[0].Status)
	}
	if len(al.calls) != 0 {
		t.Errorf("an idempotent recovery needs no alert: %v", al.calls)
	}
}

// Without idempotency, an item left in flight may be live: it is not
// re-sent, the owner is alerted, and a retry by hand sends it.
func TestRecoveryWithoutIdempotencyAsksTheOwner(t *testing.T) {
	acct := &stubAccount{manifest: stubManifest(destination.IdempotentNone)}
	d := stubDispatcher(t, acct)
	id := plan(t, d, "hello")
	sendWithoutRecording(t, d, id)

	al := &fakeAlerter{}
	d2 := &Dispatcher{Accounts: d.Accounts, Store: d.Store, Alerter: al, Policy: d.Policy}
	d2.recoverSending(context.Background())
	if n := d2.runDue(context.Background()); n != 0 {
		t.Fatalf("re-sent %d items that may be live", n)
	}
	post, _ := d.Store.GetPost(id)
	tg := post.Targets[0]
	if post.Status != "failed" || tg.GaveUpAt == nil || tg.Items[0].Status != store.ItemUnknown ||
		tg.ErrorClass != string(destination.ClassUnknownOutcome) {
		t.Fatalf("post %q target %+v", post.Status, tg)
	}
	if len(al.calls) != 1 || !strings.Contains(al.bodies[0], "may have been posted") {
		t.Fatalf("alerts = %v %v", al.calls, al.bodies)
	}
	if _, err := d2.Retry(context.Background(), id, nil); err != nil {
		t.Fatal(err)
	}
	if post, _ := d.Store.GetPost(id); post.Status != "success" || len(acct.reqs) != 2 {
		t.Errorf("retry by hand: status %q, %d requests", post.Status, len(acct.reqs))
	}
}

// Mastodon remembers a key for an hour: an item left in flight longer ago
// than that is treated like one without a key.
func TestRecoveryAfterTheKeyWindowAsksTheOwner(t *testing.T) {
	m := stubManifest(destination.IdempotentKey)
	m.Defaults.IdempotencyKeyTTLSecs = 3600
	acct := &stubAccount{manifest: m}
	d := stubDispatcher(t, acct)
	id := plan(t, d, "hello")
	sendWithoutRecording(t, d, id)

	later := time.Now().UTC().Add(2 * time.Hour)
	d2 := &Dispatcher{Accounts: d.Accounts, Store: d.Store, Policy: d.Policy, Clock: func() time.Time { return later }}
	d2.recoverSending(context.Background())
	if n := d2.runDue(context.Background()); n != 0 || len(acct.reqs) != 1 {
		t.Fatalf("re-sent after the key expired: %d items, %d requests", n, len(acct.reqs))
	}
}

// preparingAccount builds each item before sending it, like Nostr signing
// an event.
type preparingAccount struct {
	*stubAccount
	mu       sync.Mutex
	prepared int
}

func (p *preparingAccount) Prepare(context.Context, destination.PublishRequest) (json.RawMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prepared++
	return json.RawMessage(`{"event":` + itoa(p.prepared) + `}`), nil
}

// A prepared item is saved before it is sent, and every attempt sends the
// same one.
func TestPreparedItemIsSavedFirstAndReused(t *testing.T) {
	var d *Dispatcher
	var savedBeforeSend []string
	stub := &stubAccount{manifest: stubManifest(destination.IdempotentNatural),
		publish: func(n int, req destination.PublishRequest) (destination.PublishResult, error) {
			post, _ := d.Store.GetPost(postIDOf(d))
			savedBeforeSend = append(savedBeforeSend, string(post.Targets[0].Items[0].Ref))
			if n == 0 {
				return destination.PublishResult{}, &destination.Error{Class: destination.ClassTransient, Err: errors.New("relay down")}
			}
			return destination.PublishResult{Status: destination.StatusPublished,
				Ref: destination.ItemRef{ID: "ev", Data: req.Prepared}}, nil
		}}
	acct := &preparingAccount{stubAccount: stub}
	d = &Dispatcher{Accounts: map[string]destination.Account{"stub": acct}, Store: openDispatchStore(t),
		Policy: RetryPolicy{Enabled: true, MaxAttempts: 6, Base: time.Minute, Max: time.Hour}}
	rec := d.Post(context.Background(), PostSpec{MasterText: "hi", Platforms: []string{"stub"}})
	if rec.Status != "failed" {
		t.Fatalf("first attempt status = %q", rec.Status)
	}
	later := time.Now().UTC().Add(time.Hour)
	d.Clock = func() time.Time { return later }
	d.runDue(context.Background())

	if acct.prepared != 1 {
		t.Errorf("prepared %d times, want once", acct.prepared)
	}
	if len(stub.reqs) != 2 || string(stub.reqs[0].Prepared) != `{"event":1}` || string(stub.reqs[1].Prepared) != `{"event":1}` {
		t.Fatalf("attempts sent %+v", stub.reqs)
	}
	for i, s := range savedBeforeSend {
		if s != `{"event":1}` {
			t.Errorf("attempt %d: ref in the store before sending = %q", i, s)
		}
	}
	if p, _ := d.Store.GetPost(rec.ID); p.Status != "success" {
		t.Errorf("status = %q", p.Status)
	}
}

func postIDOf(d *Dispatcher) string {
	posts, _ := d.Store.ListPosts(1, 0)
	if len(posts) == 0 {
		return ""
	}
	return posts[0].ID
}

// Shutdown lets the item in flight finish, and once it has started no new
// work is sent: a post created during shutdown waits, saved, for the next
// start.
func TestShutdownFinishesTheItemInFlight(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	acct := &stubAccount{manifest: stubManifest(destination.IdempotentNone),
		publish: func(n int, _ destination.PublishRequest) (destination.PublishResult, error) {
			once.Do(func() { close(entered) })
			<-release
			return published(n), nil
		}}
	d := stubDispatcher(t, acct)
	d.PollInterval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	go d.Run(ctx)
	rec, err := d.Schedule(context.Background(), PostSpec{MasterText: "hi", Platforms: []string{"stub"}}, time.Now().Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker never sent the due post")
	}
	cancel()
	done := make(chan error, 1)
	go func() {
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer scancel()
		done <- d.Shutdown(sctx)
	}()
	select {
	case err := <-done:
		t.Fatalf("Shutdown returned with an item in flight: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if p, _ := d.Store.GetPost(rec.ID); p.Status != "success" {
		t.Errorf("in-flight post status = %q, want success", p.Status)
	}

	after := d.Post(context.Background(), PostSpec{MasterText: "late", Platforms: []string{"stub"}})
	if len(acct.reqs) != 1 {
		t.Errorf("a post created during shutdown was sent")
	}
	if after.Status != "sending" || after.Targets[0].Items[0].Status != store.ItemPlanned {
		t.Errorf("a post created during shutdown should wait, saved: %+v", after)
	}
}

func TestRateLimitWaitsAsAsked(t *testing.T) {
	d := &Dispatcher{Store: openDispatchStore(t), Policy: RetryPolicy{Enabled: true, MaxAttempts: 6, Base: time.Minute, Max: time.Hour}}
	now := time.Now().UTC()
	dec := d.decide(context.Background(), store.Claim{Platform: "stub"}, destination.ClassRateLimited, 10*time.Minute, 1, now)
	if dec.next == nil || !dec.next.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("next = %v, want the server's 10m", dec.next)
	}
	dec = d.decide(context.Background(), store.Claim{Platform: "stub"}, destination.ClassTransient, 0, 6, now)
	if dec.giveUp != "attempts exhausted" {
		t.Fatalf("at the cap: %+v", dec)
	}
	d.Policy.Enabled = false
	if dec := d.decide(context.Background(), store.Claim{Platform: "stub"}, destination.ClassTransient, 0, 1, now); dec.next != nil || dec.giveUp != "" {
		t.Fatalf("auto-retry off should schedule nothing: %+v", dec)
	}
}

// A quote's reproduction carries the source's media as the item's own, so
// a retry still has it.
func TestQuoteReproductionKeepsSourceMediaOnRetry(t *testing.T) {
	fail := true
	var mu sync.Mutex
	acct := &stubAccount{manifest: stubManifest(destination.IdempotentNone),
		publish: func(n int, req destination.PublishRequest) (destination.PublishResult, error) {
			mu.Lock()
			defer mu.Unlock()
			if fail {
				fail = false
				return destination.PublishResult{}, &destination.Error{Class: destination.ClassTransient, Err: errors.New("down")}
			}
			return published(n), nil
		}}
	src := &stubAccount{manifest: stubManifest(destination.IdempotentNone)}
	src.manifest.Type = "src"
	src.manifest.Defaults.Interactions.Quote = true
	d := &Dispatcher{Accounts: map[string]destination.Account{"stub": acct, "src": src}, Store: openDispatchStore(t),
		Fetcher: fakeFetcher{}}
	rec := d.Interact(context.Background(), InteractSpec{Action: actionQuote, SourcePlatform: "src",
		Ref: InteractRef{URI: "s://1"}, Text: "look", Fanout: []string{"stub"}, SourceURL: "https://s/1",
		SourcePreview:      SourcePreview{Author: "@a", Text: "orig"},
		SourceImages:       []Img{{Bytes: []byte("x"), Mime: "image/png", BlossomURL: "https://b/src0"}},
		SourceMediaRecords: []store.Media{{BlossomURL: "https://b/src0", Mime: "image/png"}}})
	var fan store.Target
	for _, tg := range rec.Targets {
		if tg.Platform == "stub" {
			fan = tg
		}
	}
	if fan.Status != "failed" || len(fan.Items) == 0 || len(fan.Items[0].Attach) != 1 || len(rec.Media) != 0 {
		t.Fatalf("reproduction = %+v (post media %+v)", fan, rec.Media)
	}
	if _, err := d.Retry(context.Background(), rec.ID, []string{"stub"}); err != nil {
		t.Fatal(err)
	}
	if len(acct.reqs) < 2 {
		t.Fatalf("not retried: %d requests", len(acct.reqs))
	}
	last := acct.reqs[1] // the first part again
	if len(last.Media) != 1 || last.Media[0].URL != "https://b/src0" || len(last.Media[0].Bytes) == 0 {
		t.Fatalf("retry sent media %+v", last.Media)
	}
}

// A target naming a stored account that isn't the connected one of its type
// is never sent with the connected account's credentials: it fails as an
// auth error (not retried), and nothing reaches the platform.
func TestTargetOfAnotherAccountIsNotSent(t *testing.T) {
	acct := &stubAccount{manifest: stubManifest(destination.IdempotentKey), publish: (&keyedPlatform{}).publish}
	d := stubDispatcher(t, acct)
	// The post names the stored stub account; the engine says another one is
	// connected (the same mismatch as a second stored account of the type).
	d.AccountIDs = map[string]string{"stub": "acc_connected"}
	now := d.now()
	rec := d.planPost(context.Background(), newID(), PostSpec{MasterText: "hi", Platforms: []string{"stub"}, Source: "test"}, &now, "sending")
	if err := d.create(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	d.runDue(context.Background())
	if len(acct.reqs) != 0 {
		t.Fatalf("sent %d requests as the connected account", len(acct.reqs))
	}
	post, _ := d.Store.GetPost(rec.ID)
	tg := post.Targets[0]
	if tg.AccountID == "" || tg.Status != "failed" || tg.ErrorClass != string(destination.ClassAuth) ||
		tg.GaveUpAt == nil || tg.Items[0].NextAttemptAt != nil {
		t.Fatalf("target = %+v (it must give up, not wait for a retry)", tg)
	}
	if !d.SendsAs("stub", "acc_connected") || d.SendsAs("stub", tg.AccountID) || !d.SendsAs("stub", "") {
		t.Error("SendsAs")
	}
}
