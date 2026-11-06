package dispatch

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/geofox/publisher/internal/accounts"
	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/store"
)

// Fanout drains the nostr_fanout queue, rebroadcasting each signed event to its
// secondary relays at a paced rate (at most one delivery per relay per pass), with
// exponential backoff on failure and a give-up cap. A single in-process actor; the
// running guard prevents overlapping ticks. Sibling of Retrier. It always runs:
// each Nostr account's primary_fanout decides whether it enqueues anything.
type Fanout struct {
	disp        *Dispatcher
	maxAttempts int
	base, max   time.Duration
	tick        time.Duration
	batch       int
	now         func() time.Time
	running     atomic.Bool
	noAccount   lastingWarn // jobs that wait for an account
}

// fanoutWait is how long a job that no account can deliver waits before it
// is looked at again, so it doesn't hold its place at the head of the
// queue while jobs of other accounts are due.
const fanoutWait = time.Minute

func NewFanout(d *Dispatcher, maxAttempts int, base, max, tick time.Duration) *Fanout {
	return &Fanout{disp: d, maxAttempts: maxAttempts,
		base: base, max: max, tick: tick, batch: 200, now: time.Now}
}

// Start ticks until ctx is done.
func (f *Fanout) Start(ctx context.Context) {
	t := time.NewTicker(f.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f.runDue(ctx)
		}
	}
}

// runDue processes one pass. Per relay, at most one delivery happens (the rest
// wait for the next tick), so no relay is bursted.
func (f *Fanout) runDue(ctx context.Context) {
	if !f.running.CompareAndSwap(false, true) {
		return
	}
	defer f.running.Store(false)

	now := f.now()
	jobs, err := f.disp.Store.DueFanout(now, f.batch)
	if err != nil {
		slog.ErrorContext(ctx, "fan-out: due query failed", "err", err)
		return
	}
	// Each job goes out through its own account, else another of the type;
	// one lease per account for the pass.
	type leased struct {
		rd      destination.Redeliverer
		release func()
	}
	byAccount := map[string]leased{}
	defer func() {
		for _, l := range byAccount {
			if l.release != nil {
				l.release()
			}
		}
	}()
	sentThisPass := make(map[string]bool, len(jobs))
	waiting := 0
	for _, j := range jobs {
		if sentThisPass[j.RelayURL] {
			continue // pace: at most one delivery per relay per pass (no relay is bursted)
		}
		if ctx.Err() != nil {
			return // shutting down: the rest wait, and none loses a retry
		}
		l, seen := byAccount[j.AccountID]
		if !seen {
			rd, release, err := f.disp.redeliverer(j.AccountID)
			if err == nil {
				l = leased{rd: rd, release: release}
			}
			byAccount[j.AccountID] = l
		}
		if l.rd == nil {
			// No account can deliver it now: it waits, no failure counted.
			waiting++
			if err := f.disp.Store.DeferFanout(j.ID, now.Add(fanoutWait)); err != nil {
				slog.ErrorContext(ctx, "fan-out: defer failed", "id", j.ID, "err", err)
			}
			continue
		}
		sentThisPass[j.RelayURL] = true
		dl := l.rd.Redeliver(ctx, destination.ItemRef{Data: []byte(j.SignedEventJSON)}, j.RelayURL)
		msg := dl.Message
		if dl.Status != "ok" && ctx.Err() != nil {
			return // failed because the process is stopping, not the relay
		}
		if dl.Status == "ok" {
			if err := f.disp.Store.MarkFanoutOK(j.ID); err != nil {
				slog.ErrorContext(ctx, "fan-out: mark ok failed", "id", j.ID, "err", err)
			}
			continue
		}
		if j.RetryCount+1 >= f.maxAttempts {
			if err := f.disp.Store.MarkFanoutGaveUp(j.ID); err != nil {
				slog.ErrorContext(ctx, "fan-out: mark gave-up failed", "id", j.ID, "err", err)
			}
			slog.WarnContext(ctx, "fan-out: gave up", "relay", j.RelayURL, "msg", msg)
			continue
		}
		next := now.Add(backoff(j.RetryCount+1, f.base, f.max))
		if err := f.disp.Store.MarkFanoutRetry(j.ID, next); err != nil {
			slog.ErrorContext(ctx, "fan-out: mark retry failed", "id", j.ID, "err", err)
		}
	}
	// A waiting job comes back every fanoutWait: until it has had the
	// chance, a pass without one doesn't mean the wait is over.
	switch {
	case waiting > 0 && f.noAccount.on(now):
		slog.WarnContext(ctx, "fan-out: no account can deliver to relays now; jobs wait", "jobs", waiting)
	case waiting == 0 && f.noAccount.off(now, 2*fanoutWait):
		slog.InfoContext(ctx, "fan-out: no job waits for an account any more")
	}
}

// lastingWarnEvery is how often a condition that lasts is logged again.
const lastingWarnEvery = 15 * time.Minute

// lastingWarn keeps a condition that lasts across ticks (no account can
// deliver to relays) from logging on every tick: on says to log when it
// is first seen and then every lastingWarnEvery; off, when it hasn't been
// seen for grace, says to log that it ended. Its worker's running guard
// keeps it to one goroutine.
type lastingWarn struct{ seen, logged time.Time }

func (w *lastingWarn) on(now time.Time) bool {
	w.seen = now
	if !w.logged.IsZero() && now.Sub(w.logged) < lastingWarnEvery {
		return false
	}
	w.logged = now
	return true
}

func (w *lastingWarn) off(now time.Time, grace time.Duration) bool {
	if w.seen.IsZero() || now.Sub(w.seen) < grace {
		return false
	}
	*w = lastingWarn{}
	return true
}

// ErrNoRedeliverer: no connected account can deliver a signed event to
// relays now.
var ErrNoRedeliverer = errors.New("no connected account can deliver to relays")

// redeliverer leases an account that re-delivers events for account id:
// its own when it can be used, else the first connected one of the same
// type (the event is already signed; delivering it needs no credentials).
// For a job recorded without an account, any connected re-deliverer.
func (d *Dispatcher) redeliverer(id string) (destination.Redeliverer, func(), error) {
	if d.Registry == nil {
		return nil, nil, &destination.Error{Class: destination.ClassAuth, Err: ErrNoRedeliverer}
	}
	if id != "" {
		if acct, release, err := d.Registry.Acquire(id); err == nil {
			if rd, ok := acct.(destination.Redeliverer); ok {
				return rd, release, nil
			}
			release()
		}
	}
	owner, typ := store.DefaultOwner, ""
	if e, ok := d.Registry.Get(id); ok {
		owner, typ = e.Row.OwnerID, e.Row.Type
	}
	for _, e := range d.Registry.List(owner) {
		if e.Row.ID == id || e.Status != accounts.Connected || (typ != "" && e.Row.Type != typ) {
			continue
		}
		acct, release, err := d.Registry.Acquire(e.Row.ID)
		if err != nil {
			continue
		}
		if rd, ok := acct.(destination.Redeliverer); ok {
			return rd, release, nil
		}
		release()
	}
	return nil, nil, &destination.Error{Class: destination.ClassAuth, Err: ErrNoRedeliverer}
}
