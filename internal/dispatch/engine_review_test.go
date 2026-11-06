package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/unfurl"
)

// A retry whose request goes away mid-send (a closed tab, a proxy timeout)
// still records its result: the item must not stay sending.
func TestCanceledRequestStillRecordsResult(t *testing.T) {
	var cancel context.CancelFunc
	acct := &stubAccount{manifest: stubManifest(destination.IdempotentNone),
		publish: func(n int, _ destination.PublishRequest) (destination.PublishResult, error) {
			if n == 0 {
				return destination.PublishResult{}, &destination.Error{Class: destination.ClassAuth, Err: errors.New("bad token")}
			}
			cancel() // the client goes away while the platform answers
			return published(n), nil
		}}
	d := stubDispatcher(t, acct)
	rec := d.Post(context.Background(), typeSpec{MasterText: "hi", Platforms: []string{"stub"}})
	ctx, c := context.WithCancel(context.Background())
	cancel = c
	if _, err := d.Retry(ctx, rec.ID, nil); err != nil {
		t.Fatal(err)
	}
	p, _ := d.Store.GetPost(rec.ID)
	if p.Status != "success" || p.Targets[0].Items[0].Status != store.ItemPublished {
		t.Errorf("after a canceled retry: post %q item %q; want success, published", p.Status, p.Targets[0].Items[0].Status)
	}
}

// After a restart, the key window is measured from the send that may have
// published, not from the restart.
func TestRecoveredKeyWindowStartsAtTheSend(t *testing.T) {
	m := stubManifest(destination.IdempotentKey)
	m.Defaults.IdempotencyKeyTTLSecs = 3600
	acct := &stubAccount{manifest: m, publish: func(n int, _ destination.PublishRequest) (destination.PublishResult, error) {
		if n == 1 {
			return destination.PublishResult{}, &destination.Error{Class: destination.ClassRateLimited, Err: errors.New("429")}
		}
		return published(n), nil
	}}
	d := stubDispatcher(t, acct)
	id := plan(t, d, "hello")
	t0 := time.Now().UTC()
	sendWithoutRecording(t, d, id) // sent at t0 with its key; the process dies

	at := t0.Add(50 * time.Minute) // inside the window: re-sent
	d2 := &Dispatcher{Registry: d.Registry, Store: d.Store, Policy: d.Policy, Clock: func() time.Time { return at }}
	d2.Recover(context.Background())
	d2.runDue(context.Background()) // 429
	at = t0.Add(100 * time.Minute)  // the key was forgotten at t0+60m
	d2.runDue(context.Background())
	if len(acct.reqs) != 2 {
		t.Errorf("sent %d times; want 2 (no send past the key's window from the first send)", len(acct.reqs))
	}
	// Given up unsent, the item is unknown, and a stream reader is told.
	evs, _ := d.Store.EventsSince(context.Background(), store.EventFilter{OwnerID: store.DefaultOwner, PostID: id})
	if last := evs[len(evs)-1]; last.Type != store.EventPostUpdated && last.Type != store.EventPostCompleted {
		t.Errorf("last event %+v", last)
	}
	if !slices.ContainsFunc(evs, func(e store.Event) bool {
		return e.Type == store.EventItemUnknown && strings.Contains(e.Data.Error, "no longer remembers its idempotency key")
	}) {
		t.Errorf("no item.unknown event for the item given up unsent")
	}
}

// A target that gives up on a later attempt is alerted even when the
// post's first run already alerted.
func TestGiveUpAfterRetryIsAlerted(t *testing.T) {
	acct := &stubAccount{manifest: stubManifest(destination.IdempotentNone),
		publish: func(n int, _ destination.PublishRequest) (destination.PublishResult, error) {
			switch n {
			case 0:
				return destination.PublishResult{}, &destination.Error{Class: destination.ClassTransient, Err: errors.New("down")}
			case 1:
				return published(n), nil
			}
			return destination.PublishResult{}, &destination.Error{Class: destination.ClassInvalid, Err: errors.New("rejected")}
		}}
	d := stubDispatcher(t, acct)
	al := &fakeAlerter{}
	d.Alerter = al
	rec := d.Post(context.Background(), marked(typeSpec{MasterText: "one\n---\ntwo", Platforms: []string{"stub"}}))
	n0 := len(al.calls)
	later := time.Now().Add(time.Hour)
	d.Clock = func() time.Time { return later }
	d.runDue(context.Background())
	p, _ := d.Store.GetPost(rec.ID)
	if p.Targets[0].GaveUpAt == nil {
		t.Fatalf("target should have given up: %+v", p.Targets[0])
	}
	if len(al.calls) != n0+1 || !strings.Contains(al.bodies[len(al.bodies)-1], "will not be retried") {
		t.Errorf("want one give-up alert after the first run's; got %v", al.bodies)
	}
}

// A first run that fails on one target for good and on another for now
// says which target won't be retried.
func TestFirstRunAlertNamesGivenUpTargets(t *testing.T) {
	a1 := &stubAccount{manifest: stubManifest(destination.IdempotentNone), publish: func(int, destination.PublishRequest) (destination.PublishResult, error) {
		return destination.PublishResult{}, &destination.Error{Class: destination.ClassAuth, Err: errors.New("token revoked")}
	}}
	m2 := stubManifest(destination.IdempotentNone)
	m2.Type, m2.Name = "stub2", "Other"
	a2 := &stubAccount{manifest: m2, publish: func(int, destination.PublishRequest) (destination.PublishResult, error) {
		return destination.PublishResult{}, &destination.Error{Class: destination.ClassTransient, Err: errors.New("503")}
	}}
	d := stubDispatcher(t, a1)
	d.Registry.(*testRegistry).add(acct("stub2"), "stub2", a2)
	al := &fakeAlerter{}
	d.Alerter = al
	d.Post(context.Background(), typeSpec{MasterText: "hi", Platforms: []string{"stub", "stub2"}})
	all := strings.Join(al.bodies, " | ")
	if !strings.Contains(all, "not on Stub") && !strings.Contains(all, "retried automatically on Stub") {
		t.Errorf("no alert names the given-up target: %v", al.bodies)
	}
}

// A Nostr repost is signed anew on every attempt, so the engine doesn't
// treat a repeat of one as deduped.
func TestRepostIsNotIdempotent(t *testing.T) {
	d := stubDispatcher(t, &stubAccount{manifest: stubManifest(destination.IdempotentNatural)})
	ctx := context.Background()
	ref, _ := json.Marshal(InteractRef{})
	now := time.Now().UTC()
	for _, tc := range []struct {
		id   string
		ix   *store.Interaction
		want destination.IdempotencyMode
	}{
		{"note", nil, destination.IdempotentNatural},
		{"repost", &store.Interaction{SourcePlatform: "stub", Action: actionRepost, Ref: ref}, destination.IdempotentNone},
	} {
		p := &store.Post{ID: tc.id, CreatedAt: now, Platforms: []string{"stub"}, Source: "test", Status: "sending",
			Interaction: tc.ix, Targets: []store.Target{{Platform: "stub", Status: "sending"}}}
		if err := saveWithAccounts(d.Store, p); err != nil {
			t.Fatal(err)
		}
		gp, _ := d.Store.GetPost(tc.id)
		c := store.Claim{PostID: tc.id, TargetID: gp.Targets[0].ID, ItemID: gp.Targets[0].Items[0].ID}
		caps := d.Caps(ctx, acct("stub"), "stub")
		if got := d.idempotency(ctx, c, caps); got != tc.want {
			t.Errorf("%s: idempotency %q, want %q", tc.id, got, tc.want)
		}
	}
}

// A publish that panics stops its target for the owner instead of taking
// the process down.
func TestPanickingPublishGivesUp(t *testing.T) {
	acct := &stubAccount{manifest: stubManifest(destination.IdempotentNatural),
		publish: func(int, destination.PublishRequest) (destination.PublishResult, error) { panic("driver bug") }}
	d := stubDispatcher(t, acct)
	al := &fakeAlerter{}
	d.Alerter = al
	id := plan(t, d, "hi")
	d.runDue(context.Background())
	p, _ := d.Store.GetPost(id)
	if p.Targets[0].GaveUpAt == nil || p.Targets[0].Items[0].Status != store.ItemUnknown || len(al.calls) == 0 {
		t.Errorf("after a panic: target %+v, alerts %v", p.Targets[0], al.bodies)
	}
}

// A server's Retry-After is honored up to a day.
func TestRetryAfterIsCapped(t *testing.T) {
	d := stubDispatcher(t, &stubAccount{manifest: stubManifest(destination.IdempotentNone)})
	now := time.Now().UTC()
	dec := d.decide(context.Background(), store.Claim{Platform: "stub"}, destination.ClassRateLimited, 400*24*time.Hour, 1, now)
	if dec.next == nil || dec.next.Sub(now) != maxRetryAfter {
		t.Errorf("next = %v, want now + %v", dec.next, maxRetryAfter)
	}
}

// The first send uses the thumbnail fetched while planning instead of
// downloading it again.
func TestFirstSendReusesCardThumbnail(t *testing.T) {
	f := &fakeBsky{failAt: -1}
	uf := &fakeUnfurler{card: &unfurl.Card{URI: "https://x.com/a", Title: "T", ThumbURL: "https://x.com/t.jpg",
		ThumbData: []byte("img"), ThumbMime: "image/jpeg"}}
	d := &Dispatcher{Store: openDispatchStore(t), Registry: legacyAccounts(legacy{Bluesky: f}), Unfurler: uf}
	d.Post(context.Background(), typeSpec{MasterText: "hello https://x.com/a", Platforms: []string{"bluesky"}})
	if uf.thumbs != 0 {
		t.Errorf("thumbnail downloaded %d more times", uf.thumbs)
	}
	if len(f.calls) != 1 || f.calls[0].card == nil || len(f.calls[0].card.ThumbData) == 0 {
		t.Errorf("card sent without its thumbnail: %+v", f.calls)
	}
}
