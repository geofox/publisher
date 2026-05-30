package dispatch

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/geofox/publisher/internal/store"
)

// backoff returns the delay before the next retry: base * 2^(attempt-1),
// clamped to max. attempt is the number of attempts already made
// (post_targets.attempt_count). No jitter — a single-operator deployment
// has no thundering-herd to spread.
func backoff(attempt int, base, max time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := base
	for i := 1; i < attempt; i++ {
		next := d * 2
		if next <= 0 || next >= max { // next <= 0 catches int64 overflow
			return max
		}
		d = next
	}
	if d > max {
		return max
	}
	return d
}

// Retrier re-drives failed/partial deliveries with exponential backoff and
// gives up after a cap. It reuses Dispatcher.Retry / RetryRelay. A single
// in-process actor; the running guard prevents overlapping ticks.
type Retrier struct {
	disp        *Dispatcher
	notifier    Notifier // reuses the scheduler's Notifier interface (Alert)
	enabled     bool
	maxAttempts int
	base, max   time.Duration
	tick        time.Duration
	now         func() time.Time
	running     atomic.Bool
}

func NewRetrier(d *Dispatcher, n Notifier, enabled bool, maxAttempts int, base, max, tick time.Duration) *Retrier {
	return &Retrier{disp: d, notifier: n, enabled: enabled, maxAttempts: maxAttempts,
		base: base, max: max, tick: tick, now: time.Now}
}

// Start ticks until ctx is done. No-op (returns immediately) when disabled.
func (r *Retrier) Start(ctx context.Context) {
	if !r.enabled {
		slog.Info("retrier disabled (AUTO_RETRY_ENABLED=false)")
		return
	}
	t := time.NewTicker(r.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.runDue(ctx)
		}
	}
}

// runDue processes one pass. The running guard drops a tick if the previous
// pass is still in flight (a slow network retry must not let ticks pile up).
func (r *Retrier) runDue(ctx context.Context) {
	if !r.running.CompareAndSwap(false, true) {
		return
	}
	defer r.running.Store(false)

	ids, err := r.disp.Store.PostsNeedingRetry()
	if err != nil {
		slog.Error("retrier: candidate query failed", "err", err)
		return
	}
	for _, id := range ids {
		r.processPost(ctx, id)
	}
}

// retryablePlatform mirrors Dispatcher.Retry's predicate minus 'missed':
// failed targets (any platform) and threaded partials. Nostr single-post
// partials are handled at the relay level (Task 9), not here.
func retryablePlatform(t store.Target) bool {
	return t.Status == "failed" || (t.Status == "partial" && len(t.Segments) > 1)
}

func (r *Retrier) targetDue(t store.Target, now time.Time) bool {
	if t.LastAttempt.IsZero() {
		return true
	}
	return !now.Before(t.LastAttempt.Add(backoff(t.AttemptCount, r.base, r.max)))
}

func (r *Retrier) processPost(ctx context.Context, id string) {
	post, err := r.disp.Store.GetPost(id)
	if err != nil {
		slog.Error("retrier: load failed", "post_id", id, "err", err)
		return
	}
	now := r.now()
	var due []string       // platforms to retry this pass
	var exhausted []string // platforms that just hit the cap
	for _, t := range post.Targets {
		if !retryablePlatform(t) || t.GaveUpAt != nil {
			continue
		}
		if t.AttemptCount >= r.maxAttempts {
			if set, err := r.disp.Store.MarkTargetGaveUp(t.ID, now); err != nil {
				slog.Error("retrier: mark gave-up failed", "post_id", id, "platform", t.Platform, "err", err)
			} else if set {
				exhausted = append(exhausted, t.Platform)
			}
			continue
		}
		if r.targetDue(t, now) {
			due = append(due, t.Platform)
		}
	}
	var exhaustedRelays []string
	for ti := range post.Targets {
		t := post.Targets[ti]
		// Only single-post nostr partials use relay-level retry; threaded /
		// failed nostr targets are covered by the platform path above.
		if t.Platform != "nostr" || t.Status != "partial" || len(t.Segments) > 1 {
			continue
		}
		for _, rl := range t.Relays {
			if rl.Status != "failed" || rl.GaveUpAt != nil {
				continue
			}
			if rl.RetryCount >= r.maxAttempts {
				if set, err := r.disp.Store.MarkRelayGaveUp(t.ID, rl.URL, now); err != nil {
					slog.Error("retrier: mark relay gave-up failed", "post_id", id, "relay", rl.URL, "err", err)
				} else if set {
					exhaustedRelays = append(exhaustedRelays, rl.URL)
				}
				continue
			}
			if !r.relayDue(rl, now) {
				continue
			}
			if _, err := r.disp.RetryRelay(ctx, id, rl.URL); err != nil {
				slog.Error("retrier: relay retry failed", "post_id", id, "relay", rl.URL, "err", err)
			} else {
				slog.Info("retrier: rebroadcast to relay", "post_id", id, "relay", rl.URL)
			}
		}
	}

	if len(due) > 0 {
		if _, err := r.disp.Retry(ctx, id, due); err != nil {
			slog.Error("retrier: retry failed", "post_id", id, "platforms", due, "err", err)
		} else {
			slog.Info("retrier: re-drove targets", "post_id", id, "platforms", due)
		}
	}
	if len(exhausted) > 0 || len(exhaustedRelays) > 0 {
		r.alertGaveUp(ctx, id, exhausted, exhaustedRelays)
	}
}

// relayDue checks whether the relay's next retry is due. Uses AttemptedAt +
// backoff(RetryCount+1) so the first retry is after base*2^(RetryCount) wait.
func (r *Retrier) relayDue(rl store.RelayState, now time.Time) bool {
	if rl.AttemptedAt.IsZero() {
		return true
	}
	return !now.Before(rl.AttemptedAt.Add(backoff(rl.RetryCount+1, r.base, r.max)))
}

// alertGaveUp fires one operational alert summarizing newly given-up targets.
func (r *Retrier) alertGaveUp(ctx context.Context, postID string, platforms, relays []string) {
	parts := ""
	if len(platforms) > 0 {
		parts += "platforms " + strings.Join(platforms, ", ")
	}
	if len(relays) > 0 {
		if parts != "" {
			parts += "; "
		}
		parts += strings.Join(relays, ", ") + " relays"
	}
	body := "post " + postID + " gave up on " + parts + " after " +
		strconv.Itoa(r.maxAttempts) + " attempts; manual retry still available"
	if err := r.notifier.Alert(ctx, "Publisher: auto-retry exhausted", body); err != nil {
		slog.Error("retrier: give-up alert failed", "post_id", postID, "err", err)
	}
}

