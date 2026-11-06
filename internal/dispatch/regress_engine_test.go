package dispatch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/store"
)

// a Mastodon-like (keyed, 1h TTL) item fails with a plain transient
// error (connection refused: nothing sent) and the owner retries it by hand
// through /v1 (:retry = Store.MarkDue, then the background worker claims
// it). The worker treats the claim as automatic and releases it unsent:
// the owner's retry never sends.
func TestV1RetryByHandOfOldTransientFailureIsReleasedUnsent(t *testing.T) {
	m := stubManifest(destination.IdempotentKey)
	m.Defaults.IdempotencyKeyTTLSecs = 3600
	acct := &stubAccount{manifest: m, publish: func(n int, _ destination.PublishRequest) (destination.PublishResult, error) {
		if n == 0 {
			return destination.PublishResult{}, &destination.Error{Class: destination.ClassTransient, Err: errors.New("dial tcp: connection refused")}
		}
		return published(n), nil
	}}
	d := stubDispatcher(t, acct)
	d.Policy.Enabled = false // keep it simple: no auto retry, failure waits for the owner
	ctx := context.Background()
	id := plan(t, d, "hello")
	d.runDue(ctx) // attempt 1: transient
	p, _ := d.Store.GetPost(id)
	if p.Targets[0].Status != "failed" {
		t.Fatalf("setup: target %q", p.Targets[0].Status)
	}
	// Two hours later the owner presses Retry (the PWA calls POST /v1/posts/{id}:retry).
	later := time.Now().UTC().Add(2 * time.Hour)
	d.Clock = func() time.Time { return later }
	if n, err := d.Store.MarkDue(ctx, id, []int64{p.Targets[0].ID}, later); err != nil || n != 1 {
		t.Fatalf("MarkDue: %d %v", n, err)
	}
	d.runDue(ctx) // what the worker does
	p, _ = d.Store.GetPost(id)
	if len(acct.reqs) != 2 {
		t.Errorf("owner's retry by hand sent %d more requests, want 1; item %q error %q, gave_up=%v",
			len(acct.reqs)-1, p.Targets[0].Items[0].Status, p.Targets[0].Items[0].Error, p.Targets[0].GaveUpAt != nil)
	}
}

var _ = store.ItemFailed

// with AUTO_RETRY_ENABLED=false a failure waits for the owner (no retry
// time, no give-up). When the owner later turns auto-retry on and restarts,
// the engine's pass treats every such old failure as a v4-migration row and
// re-sends it at once, weeks later, on a non-idempotent destination.
func TestEnablingAutoRetryResendsOldFailures(t *testing.T) {
	acct := &stubAccount{manifest: stubManifest(destination.IdempotentNone), publish: func(n int, _ destination.PublishRequest) (destination.PublishResult, error) {
		if n == 0 {
			return destination.PublishResult{}, &destination.Error{Class: destination.ClassTransient, Err: errors.New("502 bad gateway")}
		}
		return published(n), nil
	}}
	d := stubDispatcher(t, acct)
	d.Policy.Enabled = false
	ctx := context.Background()
	plan(t, d, "old news")
	d.runDue(ctx)

	// Three weeks later: AUTO_RETRY_ENABLED=true, restart.
	later := time.Now().UTC().Add(21 * 24 * time.Hour)
	d2 := &Dispatcher{Registry: d.Registry, Store: d.Store, Clock: func() time.Time { return later },
		Policy: RetryPolicy{Enabled: true, MaxAttempts: 6, Base: time.Minute, Max: time.Hour}}
	d2.Recover(ctx)
	d2.passes(ctx)
	d2.runDue(ctx)
	if len(acct.reqs) != 1 {
		t.Errorf("a three-week-old failure was re-sent automatically after enabling auto-retry (%d requests)", len(acct.reqs))
	}
}

// the publishing request (createRecord / threads_publish, marked
// Committing) answers 502 after the server stored the post. Classify reads
// it as transient ("nothing was applied"), and the engine auto-retries it on
// a destination that can't drop repeats: a second, duplicate post.
func TestCommitting5xxIsAutoRetriedOnNonIdempotent(t *testing.T) {
	var live int
	acct := &stubAccount{manifest: stubManifest(destination.IdempotentNone), publish: func(n int, _ destination.PublishRequest) (destination.PublishResult, error) {
		live++ // the server applied it either way
		if n == 0 {
			return destination.PublishResult{}, destination.Committing(&destination.HTTPError{Op: "createRecord", StatusCode: 502})
		}
		return published(n), nil
	}}
	d := stubDispatcher(t, acct)
	ctx := context.Background()
	plan(t, d, "hello")
	d.runDue(ctx)
	later := time.Now().UTC().Add(10 * time.Minute)
	d.Clock = func() time.Time { return later }
	d.runDue(ctx)
	if live > 1 {
		t.Errorf("a 502 on the committing request was retried automatically on a non-idempotent destination: %d posts live", live)
	}
}

// a container restart while an item is claimed but before anything was
// sent (media fetch/transcode, a Threads container still processing, PoW
// mining) is recovered as "may have been posted": the target gives up and
// the owner is alerted, although nothing went out.
func TestRestartBeforeCommitIsReportedAsMaybePosted(t *testing.T) {
	acct := &stubAccount{manifest: stubManifest(destination.IdempotentNone)}
	d := stubDispatcher(t, acct)
	ctx := context.Background()
	id := plan(t, d, "hello")
	p, _ := d.Store.GetPost(id)
	if c, err := d.Store.ClaimItem(ctx, d.now(), store.ClaimFilter{TargetID: p.Targets[0].ID}); err != nil || c == nil {
		t.Fatal(err)
	}
	// The process stops while preparing media: no request reached the platform.
	al := &fakeAlerter{}
	d2 := &Dispatcher{Registry: d.Registry, Store: d.Store, Alerter: al, Policy: d.Policy}
	d2.Recover(ctx)
	d2.runDue(ctx)
	p, _ = d.Store.GetPost(id)
	if len(acct.reqs) == 0 && p.Targets[0].GaveUpAt != nil {
		t.Errorf("nothing was sent, yet the item is %q, the target gave up and the owner got %v",
			p.Targets[0].Items[0].Status, al.calls)
	}
}

// default policy (2m base, 1h max, 6 attempts) against a Mastodon-like
// destination (key TTL 1h) that refuses connections (nothing ever reaches
// it). Attempts 1-5 run at +0, +2, +6, +14, +30 min; attempt 6 is due at
// +62 min, past the 55 min key window, so the worker releases it unsent and
// marks the item "unknown: may have been posted" although nothing was
// ever accepted.
func TestDefaultPolicyMastodonOutageEndsAsMaybePosted(t *testing.T) {
	m := stubManifest(destination.IdempotentKey)
	m.Defaults.IdempotencyKeyTTLSecs = 3600
	acct := &stubAccount{manifest: m, publish: func(int, destination.PublishRequest) (destination.PublishResult, error) {
		return destination.PublishResult{}, &destination.Error{Class: destination.ClassTransient, Err: errors.New("dial tcp: connection refused")}
	}}
	d := stubDispatcher(t, acct)
	d.Policy = RetryPolicy{Enabled: true, MaxAttempts: 6, Base: 2 * time.Minute, Max: time.Hour}
	ctx := context.Background()
	t0 := time.Now().UTC()
	clock := t0
	d.Clock = func() time.Time { return clock }
	id := plan(t, d, "hello")
	for i := 0; i < 200; i++ { // step the clock a minute at a time for 3h
		d.runDue(ctx)
		clock = clock.Add(time.Minute)
	}
	p, _ := d.Store.GetPost(id)
	it := p.Targets[0].Items[0]
	if len(acct.reqs) != 6 || it.Status == store.ItemUnknown {
		t.Errorf("sent %d of 6 attempts; item ended %q (%s)", len(acct.reqs), it.Status, it.Error)
	}
}

// a Nostr note one relay never accepts is partial; once that relay's
// retries are exhausted the target still has no gave_up_at, so the post
// stays in PostsNeedingRetry (and the Retrier reloads it every tick)
// forever. With one dead relay in the list every Nostr post joins the set.
func TestRetrierWorkSetGrowsForever(t *testing.T) {
	st := openDispatchStore(t)
	seedTarget(t, st, "dead1", "nostr", "partial", 1, []store.RelayState{
		{URL: "wss://ok", Status: "ok"},
		{URL: "wss://dead", Status: "failed"},
	}, `{"id":"abc"}`)
	d := &Dispatcher{Registry: legacyAccounts(legacy{Nostr: fakeRebroadcaster{ok: false}}), Store: st}
	now := time.Now().UTC()
	r := &Retrier{disp: d, notifier: &fakeAlerter{}, enabled: true, maxAttempts: 3,
		base: time.Minute, max: time.Hour, now: func() time.Time { return now }}
	for i := 0; i < 20; i++ {
		now = now.Add(2 * time.Hour)
		r.runDue(context.Background())
	}
	gp, _ := st.GetPost("dead1")
	if gp.Targets[0].Relays[1].GaveUpAt == nil {
		t.Fatalf("setup: relay should have given up: %+v", gp.Targets[0].Relays)
	}
	ids, _ := st.PostsNeedingRetry()
	if len(ids) != 0 {
		t.Errorf("post with every failed relay given up is still a retry candidate on every tick: %v", ids)
	}
}

// the owner says an unknown Mastodon item did not go out
// (:resolve outcome=not_published). The re-send meets a 503. The engine's
// duplicate guard still reads the pre-resolution uncertain attempt
// (target_attempts is not reset, only attempt_count), so the very first
// transient failure after the owner's word gives up with "idempotency key
// expired" instead of backing off.
func TestResolvedNotPublishedGivesUpOnFirstTransient(t *testing.T) {
	m := stubManifest(destination.IdempotentKey)
	m.Defaults.IdempotencyKeyTTLSecs = 3600
	calls := 0
	acct := &stubAccount{manifest: m, publish: func(n int, _ destination.PublishRequest) (destination.PublishResult, error) {
		calls++
		if n == 1 {
			return destination.PublishResult{}, &destination.Error{Class: destination.ClassTransient, Err: errors.New("503")}
		}
		return published(n), nil
	}}
	d := stubDispatcher(t, acct)
	id := plan(t, d, "hello")
	sendWithoutRecording(t, d, id)
	later := time.Now().UTC().Add(2 * time.Hour)
	d2 := &Dispatcher{Registry: d.Registry, Store: d.Store, Policy: d.Policy, Clock: func() time.Time { return later }}
	ctx := context.Background()
	d2.recoverSending(ctx)
	post, _ := d.Store.GetPost(id)
	if err := d.Store.ResolveItem(ctx, store.Resolution{PostID: id, TargetID: post.Targets[0].ID, Ordinal: 0, Continue: true, At: later}); err != nil {
		t.Fatal(err)
	}
	d2.runDue(ctx) // 503
	post, _ = d.Store.GetPost(id)
	if post.Targets[0].GaveUpAt != nil {
		t.Errorf("gave up on the first transient failure after the owner's resolution: item %q %q",
			post.Targets[0].Items[0].Status, post.Targets[0].Items[0].Error)
	}
}
