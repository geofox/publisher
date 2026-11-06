package dispatch

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/geofox/publisher/internal/store"
)

// Inline sending, for tests. Production saves posts for the background
// workers (Enqueue, EnqueueInteraction) and makes retries due
// (store.MarkDue); the tests' wrappers (registry_test.go) send inline and
// return the post as recorded. The workers leave a post driven inline alone
// (inlinePosts).

// beginInline registers an inline drive of postID; false once shutting down.
func (d *Dispatcher) beginInline(postID string) bool {
	d.eng.mu.Lock()
	defer d.eng.mu.Unlock()
	if d.eng.stopping {
		return false
	}
	if d.eng.inline == nil {
		d.eng.inline = map[string]int{}
	}
	d.eng.inline[postID]++
	d.eng.active.Add(1)
	return true
}

func (d *Dispatcher) endInline(postID string) {
	d.eng.mu.Lock()
	if d.eng.inline[postID]--; d.eng.inline[postID] <= 0 {
		delete(d.eng.inline, postID)
	}
	d.eng.mu.Unlock()
	d.eng.active.Done()
	d.kick() // what the drive left (a retry later, say) is the workers' now
}

// runDue claims and publishes due items one after another until none is
// due, and returns how many it ran: one worker's loop, synchronously.
func (d *Dispatcher) runDue(ctx context.Context) int {
	n := 0
	for !d.isStopping() {
		now := d.now()
		c, err := d.Store.ClaimItem(ctx, now, store.ClaimFilter{Exclude: d.inlinePosts(), MissedBefore: d.missedBefore(now)})
		if err != nil || c == nil {
			return n
		}
		d.runClaim(ctx, c, nil, true)
		n++
	}
	return n
}

// drive publishes postID's due items now: targets in parallel, each
// target's items in order. It returns when none of them is due. persist,
// when set, saves the post or makes its items due: it runs after the post
// is registered as driven inline, so no worker claims its items first, and
// it runs even during shutdown (the next start sends what it saved).
func (d *Dispatcher) drive(ctx context.Context, postID string, mm *memMedia, persist func() error) error {
	// A send, once started, finishes and is recorded even if the request
	// that asked for it goes away (a closed tab, a proxy timeout): a
	// canceled record would leave the item sending until the next start.
	ctx = context.WithoutCancel(ctx)
	if !d.beginInline(postID) {
		if persist != nil {
			if err := persist(); err != nil {
				return err
			}
		}
		slog.WarnContext(ctx, "engine: shutting down; post left to the next start", "post_id", postID)
		return nil
	}
	defer d.endInline(postID)
	if persist != nil {
		if err := persist(); err != nil {
			return err
		}
	}
	post, err := d.Store.GetPost(postID)
	if err != nil {
		slog.ErrorContext(ctx, "engine: load post failed", "post_id", postID, "err", err)
		return nil
	}
	var wg sync.WaitGroup
	for _, tg := range post.Targets {
		wg.Add(1)
		go func(tg store.Target) {
			defer wg.Done()
			d.driveTarget(ctx, postID, tg, mm)
		}(tg)
	}
	wg.Wait()
	return nil
}

func (d *Dispatcher) driveTarget(ctx context.Context, _ string, tg store.Target, mm *memMedia) {
	for !d.isStopping() {
		c, err := d.Store.ClaimItem(ctx, d.now(), store.ClaimFilter{TargetID: tg.ID})
		if err != nil {
			slog.ErrorContext(ctx, "engine: claim failed", "target_id", tg.ID, "err", err)
			break
		}
		if c == nil {
			break
		}
		d.runClaim(ctx, c, mm, false)
	}
}

// notSaved is the record returned when a post couldn't be saved: nothing
// was sent, and every target says why.
func notSaved(rec *store.Post, err error) *store.Post {
	rec.Status = "failed"
	for i := range rec.Targets {
		rec.Targets[i].Status = "failed"
		rec.Targets[i].Attempts = []store.Attempt{{AttemptNo: 1, Status: "failed",
			Error: "not sent: the post could not be saved: " + err.Error(), AttemptedAt: time.Now().UTC()}}
	}
	return rec
}
