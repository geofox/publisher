package dispatch

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/store"
)

// An item left unknown after a key destination's window is sent again
// when the owner says it didn't go out: their word lifts the duplicate
// guard that held it back.
func TestResolvedNotPublishedIsSentAfterTheKeyWindow(t *testing.T) {
	m := stubManifest(destination.IdempotentKey)
	m.Defaults.IdempotencyKeyTTLSecs = 3600
	acct := &stubAccount{manifest: m}
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
	if n := d2.runDue(ctx); n != 1 || len(acct.reqs) != 2 {
		t.Fatalf("ran %d items, %d requests; want the item sent again", n, len(acct.reqs))
	}
	if post, _ := d.Store.GetPost(id); post.Status != "success" {
		t.Fatalf("status %q", post.Status)
	}
}

// A thread part marked posted without its reference stops the rest; a
// retry by hand fails them as invalid instead of posting them detached.
func TestThreadNeverRepliesToAPartWithoutReference(t *testing.T) {
	acct := &stubAccount{manifest: stubManifest(destination.IdempotentNone)}
	d := stubDispatcher(t, acct)
	ctx := context.Background()
	id := plan(t, d, "first part is here\n---\nsecond part\n---\nthird part")
	sendWithoutRecording(t, d, id)
	d.recoverSending(ctx)
	post, _ := d.Store.GetPost(id)
	tg := post.Targets[0]
	if len(tg.Items) != 3 || tg.Items[0].Status != store.ItemUnknown {
		t.Fatalf("items = %+v", tg.Items)
	}
	if err := d.Store.ResolveItem(ctx, store.Resolution{PostID: id, TargetID: tg.ID, Ordinal: 0, Published: true,
		RemoteURL: "https://x/0"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Store.MarkDue(ctx, id, []int64{tg.ID}, time.Now()); err != nil {
		t.Fatal(err)
	}
	d.runDue(ctx)
	if len(acct.reqs) != 1 {
		t.Fatalf("%d requests; the rest went out with nothing to reply to", len(acct.reqs))
	}
	post, _ = d.Store.GetPost(id)
	it := post.Targets[0].Items[1]
	if it.Status != store.ItemFailed || it.ErrorClass != string(destination.ClassInvalid) || !strings.Contains(it.Error, "no reference") {
		t.Fatalf("item = %+v", it)
	}
}
