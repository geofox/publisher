package dispatch

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// Fanout drains the nostr_fanout queue, rebroadcasting each signed event to its
// secondary relays at a paced rate (at most one delivery per relay per pass), with
// exponential backoff on failure and a give-up cap. A single in-process actor; the
// running guard prevents overlapping ticks. Sibling of Retrier.
type Fanout struct {
	disp        *Dispatcher
	enabled     bool
	maxAttempts int
	base, max   time.Duration
	tick        time.Duration
	batch       int
	now         func() time.Time
	running     atomic.Bool
}

func NewFanout(d *Dispatcher, enabled bool, maxAttempts int, base, max, tick time.Duration) *Fanout {
	return &Fanout{disp: d, enabled: enabled, maxAttempts: maxAttempts,
		base: base, max: max, tick: tick, batch: 200, now: time.Now}
}

// Start ticks until ctx is done. No-op when disabled.
func (f *Fanout) Start(ctx context.Context) {
	if !f.enabled {
		slog.Info("nostr fan-out worker disabled (NOSTR_PRIMARY_FANOUT=false)")
		return
	}
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
	sentThisPass := make(map[string]bool, len(jobs))
	for _, j := range jobs {
		if sentThisPass[j.RelayURL] {
			continue // pace: at most one delivery per relay per pass (no relay is bursted)
		}
		sentThisPass[j.RelayURL] = true
		ok, msg := f.disp.Nostr.RebroadcastToRelay(ctx, j.SignedEventJSON, j.RelayURL)
		if ok {
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
}
